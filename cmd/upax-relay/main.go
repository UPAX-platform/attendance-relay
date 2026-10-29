// Command upax-relay is the UPAX attendance relay: it forwards punches from
// attendance terminals on the local network to UPAX, signing every request
// with a key paired to the organisation's UPAX account.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	_ "time/tzdata" // UPAX zone names must resolve on Windows too

	"github.com/kardianos/service"

	"github.com/UPAX-platform/attendance-relay/internal/config"
	_ "github.com/UPAX-platform/attendance-relay/internal/device/drivers"
	"github.com/UPAX-platform/attendance-relay/internal/identity"
	"github.com/UPAX-platform/attendance-relay/internal/relay"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
)

var version = "dev"

const usage = `upax-relay %s — the UPAX attendance relay

Usage:
  upax-relay setup          pair with UPAX, add terminals, install the service
  upax-relay status         relay and terminal state, upload progress (no admin needed)
  upax-relay test           check every terminal and one signed call to UPAX
  upax-relay scan           look for terminals on the local network
  upax-relay pair           pair with UPAX using a new pairing code
  upax-relay unpair         forget this relay's key and pairing
  upax-relay rotate-key     replace the relay key (also done yearly by the service)
  upax-relay show-comm-key  print a comm key the relay manages, for vendor tools
  upax-relay run            run in the foreground (what the service runs)
  upax-relay install | uninstall | start | stop | restart
  upax-relay version

Common flags:
  --config PATH   config file (default %s)

Run "upax-relay <command> -h" for command flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, version, config.DefaultPath())
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "setup":
		err = cmdSetup(args)
	case "test":
		err = cmdTest(args)
	case "scan":
		err = cmdScan(args)
	case "run":
		err = cmdRun(args)
	case "status":
		err = cmdStatus(args)
	case "pair":
		err = cmdPair(args)
	case "unpair":
		err = cmdUnpair(args)
	case "rotate-key":
		err = cmdRotateKey(args)
	case "show-comm-key":
		err = cmdShowCommKey(args)
	case "install", "uninstall", "start", "stop", "restart":
		err = cmdService(cmd, args)
	case "version", "--version", "-v":
		fmt.Println("upax-relay", version, runtime.GOOS+"/"+runtime.GOARCH)
	case "help", "-h", "--help":
		fmt.Printf(usage, version, config.DefaultPath())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Fprintf(os.Stderr, usage, version, config.DefaultPath())
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if errors.Is(err, os.ErrPermission) {
			fmt.Fprintln(os.Stderr, adminHint())
		}
		os.Exit(1)
	}
}

func adminHint() string {
	if runtime.GOOS == "windows" {
		return "hint: run this from a terminal opened with \"Run as administrator\""
	}
	return "hint: run this with sudo"
}

func newFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	path := fs.String("config", config.DefaultPath(), "config file")
	return fs, path
}

// ---- service plumbing -------------------------------------------------------

type program struct {
	cfgPath string
	cancel  context.CancelFunc
	done    chan struct{}
	logger  service.Logger
}

func (p *program) Start(s service.Service) error {
	cfg, keys, api, err := loadPaired(p.cfgPath)
	if err != nil {
		if p.logger != nil {
			_ = p.logger.Error(err)
		}
		return err
	}
	log, closeLog := newLogger(cfg.StateDir())
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	log.Info("upax-relay starting", "version", version, "config", p.cfgPath, "relay_id", cfg.RelayID, "terminals", len(cfg.Terminals), "listener", cfg.Listener.Enabled)
	r := relay.New(relay.Options{Config: cfg, API: api, Keys: keys, Log: log, Version: version, HostName: hostName()})
	go func() {
		defer close(p.done)
		defer closeLog()
		_ = r.Run(ctx)
		log.Info("upax-relay stopped")
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
		}
	}
	return nil
}

// installedBinary is where the service runs the relay from: the path the
// install scripts use, so the service never depends on wherever setup
// happened to be run from (a download folder, or a `go run` build that is
// deleted when it exits).
func installedBinary() string {
	if runtime.GOOS == "windows" {
		pf := os.Getenv("ProgramFiles")
		if pf == "" {
			pf = `C:\Program Files`
		}
		return filepath.Join(pf, "UPAX Relay", "upax-relay.exe")
	}
	return "/usr/local/bin/upax-relay"
}

// ensureInstalledBinary copies the running binary to installedBinary unless
// it is already running from there.
func ensureInstalledBinary() (string, error) {
	target, staged, err := stageBinary()
	if err != nil || staged == "" {
		return target, err
	}
	return target, placeBinary(target, staged)
}

// stageBinary copies the running binary next to installedBinary, ready to
// be moved into place; staged is "" when the relay already runs from there.
// Nothing installed is touched.
func stageBinary() (target, staged string, err error) {
	target = installedBinary()
	self, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return "", "", err
	}
	if t, err := filepath.EvalSymlinks(target); err == nil && (t == self || runtime.GOOS == "windows" && strings.EqualFold(t, self)) {
		return target, "", nil
	}
	src, err := os.Open(self)
	if err != nil {
		return "", "", err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", "", wrapPerm(err)
	}
	tmp := target + ".new"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", "", wrapPerm(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmp)
		return "", "", err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	return target, tmp, nil
}

// placeBinary moves a staged binary over target. On Windows the old exe
// can stay locked for a moment after its service stops, so it retries.
func placeBinary(target, staged string) error {
	var err error
	for range 10 {
		if err = os.Rename(staged, target); err == nil || runtime.GOOS != "windows" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		os.Remove(staged)
		return wrapPerm(err)
	}
	fmt.Printf("Installed %s\n", target)
	return nil
}

// newService describes the service. exe is the binary it runs; "" leaves it
// unset, which is fine for every action but install.
func newService(cfgPath, exe string) (service.Service, *program, error) {
	abs, err := filepath.Abs(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	p := &program{cfgPath: abs}
	sc := &service.Config{
		Executable:  exe,
		Name:        "upax-relay",
		DisplayName: "UPAX Attendance Relay",
		Description: "Forwards attendance punches from terminals on the local network to UPAX.",
		Arguments:   []string{"run", "--config", abs},
		Option: service.KeyValue{
			"KeepAlive":              true, // macOS launchd
			"RunAtLoad":              true, // macOS launchd
			"Restart":                "always",
			"OnFailure":              "restart", // Windows
			"OnFailureDelayDuration": "10s",
			"DelayedAutoStart":       true,
		},
	}
	if runtime.GOOS == "linux" {
		sc.Dependencies = []string{"After=network-online.target", "Wants=network-online.target"}
	}
	s, err := service.New(p, sc)
	if err != nil {
		return nil, nil, err
	}
	p.logger, _ = s.Logger(nil)
	return s, p, nil
}

func cmdRun(args []string) error {
	fs, path := newFlags("run")
	_ = fs.Parse(args)
	if _, _, _, err := loadPaired(*path); err != nil {
		return err
	}
	s, _, err := newService(*path, "")
	if err != nil {
		return err
	}
	return s.Run()
}

func cmdService(action string, args []string) error {
	fs, path := newFlags(action)
	_ = fs.Parse(args)
	exe := ""
	if action == "install" {
		if _, _, _, err := loadPaired(*path); err != nil {
			return err
		}
		var err error
		if exe, err = ensureInstalledBinary(); err != nil {
			return err
		}
	}
	s, _, err := newService(*path, exe)
	if err != nil {
		return err
	}
	if err := service.Control(s, action); err != nil {
		return wrapPerm(err)
	}
	fmt.Printf("service %s: ok\n", action)
	return nil
}

// installAndStart (re)installs the service pointing at cfgPath and starts it.
// The new binary is in place before the old service is removed, so a
// failed copy leaves the old service as it was.
func installAndStart(cfgPath string) error {
	exe, staged, err := stageBinary()
	if err != nil {
		return err
	}
	old, _, err := newService(cfgPath, "")
	if err != nil {
		os.Remove(staged)
		return err
	}
	st, err := old.Status()
	installed := err == nil && st != service.StatusUnknown
	if installed {
		_ = old.Stop()
	}
	if staged != "" {
		if err := placeBinary(exe, staged); err != nil {
			if installed && st == service.StatusRunning {
				_ = old.Start()
			}
			return fmt.Errorf("could not replace %s; the service was left as it was: %w", exe, err)
		}
	}
	if installed {
		_ = old.Uninstall()
	}
	s, _, err := newService(cfgPath, exe)
	if err != nil {
		return err
	}
	if err := s.Install(); err != nil {
		return wrapPerm(err)
	}
	return wrapPerm(s.Start())
}

func wrapPerm(err error) error {
	if err == nil {
		return nil
	}
	if os.Geteuid() > 0 || errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%v: %w", err, os.ErrPermission)
	}
	return err
}

func adminWord() string {
	if runtime.GOOS == "windows" {
		return "an administrator terminal"
	}
	return "sudo"
}

// ---- logging -----------------------------------------------------------------

// newLogger logs to stderr and to <dir>/relay.log (rotated at 10 MB).
func newLogger(dir string) (*slog.Logger, func()) {
	var w io.Writer = os.Stderr
	closeFn := func() {}
	if rw, err := newRotatingFile(filepath.Join(dir, "relay.log"), 10<<20); err == nil {
		w = io.MultiWriter(os.Stderr, rw)
		closeFn = func() { rw.Close() }
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})), closeFn
}

// ---- shared ------------------------------------------------------------------

func hostName() string {
	h, _ := os.Hostname()
	return h
}

func userAgent() string { return "upax-relay/" + version }

// loadPaired loads the config and the relay key, for commands that need a
// paired relay.
func loadPaired(path string) (*config.Config, *identity.Source, *upax.Client, error) {
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil, fmt.Errorf("%w\nrun \"upax-relay setup\" first", err)
		}
		return nil, nil, nil, err
	}
	if cfg.RelayID == "" {
		return nil, nil, nil, errors.New("this relay isn't paired with UPAX yet; run \"upax-relay setup\" or \"upax-relay pair\"")
	}
	keys := &identity.Source{Path: cfg.KeyPath()}
	if _, err := keys.Key(); err != nil {
		if errors.Is(err, identity.ErrNoKey) {
			return nil, nil, nil, fmt.Errorf("%s is missing: pair again with \"upax-relay pair\"", cfg.KeyPath())
		}
		return nil, nil, nil, err
	}
	api := &upax.Client{Base: cfg.UPAX, RelayID: cfg.RelayID, Keys: keys, UserAgent: userAgent()}
	return cfg, keys, api, nil
}
