// Command upax-relay forwards punches from ZKTeco terminals that can't talk
// to UPAX directly (e.g. K14 Pro) to the UPAX platform.
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
	"sort"
	"strings"
	"time"

	"github.com/kardianos/service"

	"github.com/UPAX-platform/zk-relay/internal/config"
	"github.com/UPAX-platform/zk-relay/internal/relay"
)

var version = "dev"

const usage = `upax-relay %s — relay ZKTeco devices to UPAX

Usage:
  upax-relay setup       interactive setup: find the device, test, install service
  upax-relay test        check connectivity to every configured device and UPAX
  upax-relay scan        look for ZKTeco devices on the local network
  upax-relay run         run in the foreground (what the service runs)
  upax-relay status      show service status and upload progress
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
	cfg, err := config.Load(p.cfgPath)
	if err != nil {
		if p.logger != nil {
			_ = p.logger.Error(err)
		}
		return err
	}
	log, closeLog := newLogger(cfg.StateDir())
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	log.Info("upax-relay starting", "version", version, "config", p.cfgPath, "devices", len(cfg.Devices))
	go func() {
		defer close(p.done)
		defer closeLog()
		relay.Run(ctx, cfg, log, version)
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
	target := installedBinary()
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return "", err
	}
	if t, err := filepath.EvalSymlinks(target); err == nil && (t == self || runtime.GOOS == "windows" && strings.EqualFold(t, self)) {
		return target, nil
	}
	src, err := os.Open(self)
	if err != nil {
		return "", err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", wrapPerm(err)
	}
	tmp := target + ".new"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", wrapPerm(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmp)
		return "", err
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return "", wrapPerm(err)
	}
	fmt.Printf("Installed %s\n", target)
	return target, nil
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
		DisplayName: "UPAX ZKTeco Relay",
		Description: "Forwards attendance punches from local ZKTeco devices to UPAX.",
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
	if _, err := config.Load(*path); err != nil {
		return fmt.Errorf("%w\nrun \"upax-relay setup\" first", err)
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
		if _, err := config.Load(*path); err != nil {
			return fmt.Errorf("%w\nrun \"upax-relay setup\" first", err)
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
func installAndStart(cfgPath string) error {
	old, _, err := newService(cfgPath, "")
	if err != nil {
		return err
	}
	if st, err := old.Status(); err == nil && st != service.StatusUnknown {
		_ = old.Stop()
		_ = old.Uninstall()
	}
	exe, err := ensureInstalledBinary()
	if err != nil {
		return err
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

func cmdStatus(args []string) error {
	fs, path := newFlags("status")
	_ = fs.Parse(args)
	s, _, err := newService(*path, "")
	if err != nil {
		return err
	}
	st, err := s.Status()
	running := false
	switch {
	case errors.Is(err, service.ErrNotInstalled):
		fmt.Println("service: not installed")
	case err != nil:
		fmt.Println("service: unknown (", err, ")")
	case st == service.StatusRunning:
		fmt.Println("service: running")
		running = true
	case st == service.StatusStopped:
		fmt.Println("service: stopped")
	default:
		fmt.Println("service: unknown")
	}

	// The config is readable by administrators only (it holds the device
	// addresses); the service's health files next to it are not.
	dir := filepath.Dir(*path)
	cfg, err := config.Load(*path)
	switch {
	case err == nil:
		dir = cfg.StateDir()
		fmt.Println("config:", cfg.Path())
	case errors.Is(err, os.ErrPermission):
		fmt.Println("config:", *path, "(readable with", adminWord()+")")
	default:
		fmt.Println("config:", err)
	}
	fmt.Println("log:   ", filepath.Join(dir, "relay.log"))

	hs := relay.LoadAllHealth(dir)
	for _, h := range hs {
		printHealth(h, running)
	}
	if len(hs) > 0 {
		return nil
	}
	// No health files: a relay from before they existed.
	files, _ := filepath.Glob(filepath.Join(dir, "state-*.json"))
	sort.Strings(files)
	for _, f := range files {
		ss, err := relay.LoadStateFile(f)
		if err != nil {
			continue
		}
		last := "never"
		if !ss.LastUploadAt.IsZero() {
			last = ss.LastUploadAt.Local().Format(time.DateTime)
		}
		fmt.Printf("device %s: %d punches uploaded, last upload %s, last punch %s\n",
			ss.Serial, ss.Uploaded, last, ss.LastTime.Format(time.DateTime))
	}
	return nil
}

func printHealth(h *relay.Health, running bool) {
	fmt.Printf("\n%s", h.Device)
	if h.Device != h.Addr {
		fmt.Printf(" (%s)", h.Addr)
	}
	if h.Serial != "" {
		fmt.Printf(", serial %s", h.Serial)
	}
	fmt.Println()
	contact := h.LastHandshake
	if h.LastHeartbeat.After(contact) {
		contact = h.LastHeartbeat
	}
	switch h.UPAXStatus {
	case "active", "unknown":
		fmt.Println("  UPAX:     connected")
	case "pending":
		fmt.Println("  UPAX:     waiting for activation (HR > Devices in UPAX); punches stay on the device until then")
	case "paused":
		fmt.Println("  UPAX:     paused in UPAX; punches stay on the device until it is resumed")
	case "rejected":
		fmt.Println("  UPAX:     the device address was rejected; check it in UPAX and run setup again")
	default:
		fmt.Println("  UPAX:     not reached yet")
	}
	fmt.Println("  contact: ", ago(contact))
	fmt.Printf("  uploaded:  %d punches, last %s\n", h.Uploaded, ago(h.LastUpload))
	if h.Pending > 0 {
		fmt.Printf("  pending:   %d punches\n", h.Pending)
	}
	if h.LastError != "" && h.LastErrorAt.After(contact) {
		fmt.Printf("  error:     %s (%s)\n", h.LastError, ago(h.LastErrorAt))
	}
	if running && time.Since(h.UpdatedAt) > 5*time.Minute {
		fmt.Printf("  warning:   no update from the service for %s; see the log\n", time.Since(h.UpdatedAt).Round(time.Second))
	}
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return fmt.Sprintf("%s (%s ago)", t.Local().Format(time.DateTime), time.Since(t).Round(time.Second))
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
