package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/UPAX-platform/attendance-relay/internal/config"
	"github.com/UPAX-platform/attendance-relay/internal/device"
	"github.com/UPAX-platform/attendance-relay/internal/identity"
	"github.com/UPAX-platform/attendance-relay/internal/relay"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
)

var stdin = bufio.NewReader(os.Stdin)

func prompt(q, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", q, def)
	} else {
		fmt.Printf("%s: ", q)
	}
	line, err := stdin.ReadString('\n')
	line = strings.TrimSpace(line)
	if err != nil && line == "" {
		fmt.Println()
		if def == "" {
			fmt.Fprintln(os.Stderr, "no input available; pass the values as flags instead (see setup -h)")
			os.Exit(1)
		}
	}
	if line == "" {
		return def
	}
	return line
}

func yes(q string, def bool) bool {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	a := strings.ToLower(prompt(q+" ("+d+")", ""))
	if a == "" {
		return def
	}
	return a == "y" || a == "yes"
}

func isTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

type terminalInfo struct {
	device.Info
	clock time.Time
}

func probeTerminal(ctx context.Context, t config.Terminal) (terminalInfo, error) {
	drv, err := device.Lookup(t.Vendor)
	if err != nil {
		return terminalInfo{}, err
	}
	c, err := drv.Dial(ctx, t.Target(), 8*time.Second)
	if err != nil {
		return terminalInfo{}, err
	}
	defer c.Close()
	var in terminalInfo
	if in.Info, err = c.Info(); err != nil {
		return in, err
	}
	in.clock, _ = c.Clock()
	return in, nil
}

func splitHostPort(s string) (string, int) {
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, _ := strconv.Atoi(p)
		return h, n
	}
	return s, 0
}

func cmdSetup(args []string) error {
	fs, path := newFlags("setup")
	base := fs.String("upax", "", "UPAX address, e.g. https://<your-org>.upax.io")
	code := fs.String("code", "", "pairing code from UPAX (RLY-XXXX-XXXX-XXXX)")
	newKey := fs.Bool("new-key", false, "pair with a new key even if this relay has one")
	termFlag := fs.String("terminal", "", "terminal IP or host[:port] to add")
	vendor := fs.String("vendor", device.DefaultVendor, "terminal vendor")
	pw := fs.Uint("password", 0, "terminal comm key, if one is set")
	name := fs.String("name", "", "friendly name for the terminal")
	backfill := fs.String("backfill", "", "first upload from a terminal: stored history (all), only new punches (none), or since:YYYY-MM-DD")
	listener := fs.String("listener", "", "receive from push-capable terminals on the LAN: yes or no")
	install := fs.String("install", "ask", "install and start the background service: yes, no or ask")
	_ = fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Println("UPAX attendance relay setup")
	fmt.Println("===========================")

	cfg, err := config.Load(*path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg = config.New(*path)
	case err != nil:
		return wrapPerm(err)
	}

	// 1. UPAX and pairing.
	if *base != "" {
		cfg.UPAX = strings.TrimRight(strings.TrimSpace(*base), "/")
	}
	if cfg.UPAX == "" {
		fmt.Println("\nThe UPAX address is your organisation's UPAX domain, e.g. https://<your-org>.upax.io")
		cfg.UPAX = strings.TrimRight(prompt("UPAX address", ""), "/")
	}
	if err := upax.ValidateBase(cfg.UPAX); err != nil {
		return err
	}
	if _, err := identity.Load(cfg.KeyPath()); cfg.RelayID != "" && err == nil && !*newKey && *code == "" {
		k, _ := identity.Load(cfg.KeyPath())
		fmt.Printf("\nThis relay is already paired (fingerprint %s).\n", identity.Group(k.Fingerprint()))
	} else {
		if *code == "" {
			fmt.Println("\nCreate a pairing code in UPAX: HR › Devices › Relays › Add relay.")
			*code = prompt("Pairing code", "")
		}
		k, _, err := pair(ctx, cfg, *code, *newKey)
		if err != nil {
			return err
		}
		printFingerprint(k)
	}

	// 2. Terminals pulled over the LAN.
	added := 0
	if *termFlag != "" {
		h, p := splitHostPort(*termFlag)
		if err := addTerminal(ctx, cfg, config.Terminal{Name: *name, Vendor: vendorOrEmpty(*vendor), Host: h, Port: p, Password: uint32(*pw)}); err != nil {
			return err
		}
		added++
	} else if isTTY() {
		fmt.Println("\nThe relay reads terminals over your local network (ZKTeco: TCP port 4370).")
		for _, t := range cfg.Terminals {
			fmt.Printf("  configured: %s\n", t.Label())
		}
		ask := "Add a terminal"
		def := len(cfg.Terminals) == 0
		if !def {
			ask = "Add another terminal"
		}
		for yes(ask+"?", def) {
			if err := addTerminalInteractive(ctx, cfg); err != nil {
				fmt.Println("  ", err)
			} else {
				added++
			}
			ask, def = "Add another terminal", false
		}
	}

	// 3. History.
	if *backfill != "" {
		if err := config.ValidateBackfill(*backfill); err != nil {
			return err
		}
		cfg.Backfill = *backfill
	} else if added > 0 && isTTY() {
		fmt.Println("\nOn first contact, upload the punches already stored on new terminals?")
		fmt.Println("  all = every stored punch, none = only new punches, since:YYYY-MM-DD = from that date")
		for {
			b := prompt("Upload", cfg.Backfill)
			if err := config.ValidateBackfill(b); err != nil {
				fmt.Println(" ", err)
				continue
			}
			cfg.Backfill = b
			break
		}
	}

	// 4. Push terminals (opt-in).
	switch *listener {
	case "yes":
		cfg.Listener.Enabled = true
	case "no":
		cfg.Listener.Enabled = false
	case "":
		if isTTY() && !cfg.Listener.Enabled {
			fmt.Println("\nPush-capable terminals can send to this relay instead of to UPAX directly, so")
			fmt.Println("they need no internet access and hold no UPAX credential.")
			cfg.Listener.Enabled = yes("Receive from push terminals on this network?", false)
		}
	default:
		return fmt.Errorf("--listener must be yes or no")
	}
	if cfg.Listener.Enabled {
		fmt.Printf("  Point each push terminal's cloud server setting at this computer's LAN address, port %s.\n", listenerPort(cfg.Listener.Bind))
	}

	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return wrapPerm(err)
	}
	fmt.Printf("\nSaved %s\n", cfg.Path())

	// 5. Service.
	doInstall := *install == "yes" || (*install == "ask" && isTTY() && yes("Install and start the relay as a background service?", true))
	if !doInstall {
		fmt.Printf("\nInstall and start the service later with:  upax-relay install --config %q && upax-relay start\n", cfg.Path())
		return nil
	}
	started := time.Now()
	if err := installAndStart(cfg.Path()); err != nil {
		return err
	}
	if err := waitForService(ctx, cfg, started); err != nil {
		return err
	}
	fmt.Println("\nDone. The relay runs in the background and starts automatically at boot.")
	fmt.Println("Check on it any time with:  upax-relay status")
	return nil
}

func vendorOrEmpty(v string) string {
	if v == device.DefaultVendor {
		return ""
	}
	return v
}

func listenerPort(bind string) string {
	_, p, err := net.SplitHostPort(bind)
	if err != nil {
		return bind
	}
	return p
}

func addTerminalInteractive(ctx context.Context, cfg *config.Config) error {
	fmt.Println("Find the terminal's IP on it (ZKTeco: Menu › Comm. › Ethernet). Leave blank to scan the network.")
	host := prompt("Terminal IP address", "")
	vendor := ""
	if host == "" {
		found := scanNetwork(ctx, "", true)
		if len(found) == 0 {
			return errors.New("no terminals found; make sure the terminal is on the same network and enter its IP")
		}
		for i, f := range found {
			fmt.Printf("  %d) %s:%d  %s  %s\n", i+1, f.addr, f.port, f.vendor, f.serial)
		}
		n, _ := strconv.Atoi(prompt("Which terminal", "1"))
		if n < 1 || n > len(found) {
			n = 1
		}
		host, vendor = net.JoinHostPort(found[n-1].addr, strconv.Itoa(found[n-1].port)), vendorOrEmpty(found[n-1].vendor)
	}
	h, p := splitHostPort(host)
	t := config.Terminal{Vendor: vendor, Host: h, Port: p}
	t.Name = prompt("Name for this terminal (e.g. Front door)", t.Label())
	if t.Name == t.Addr() {
		t.Name = ""
	}
	return addTerminal(ctx, cfg, t)
}

// addTerminal connects to t (asking for its comm key if needed) and adds
// or replaces it in cfg.
func addTerminal(ctx context.Context, cfg *config.Config, t config.Terminal) error {
	for _, e := range cfg.Terminals {
		if e.Addr() == t.Addr() {
			if t.Name == "" {
				t.Name = e.Name
			}
			if t.Password == 0 {
				t.Password = e.Password
			}
			t.ManageCommKey = e.ManageCommKey
		}
	}
	var info terminalInfo
	var err error
	for {
		fmt.Printf("Connecting to %s ... ", t.Addr())
		info, err = probeTerminal(ctx, t)
		if err == nil {
			break
		}
		fmt.Println("failed")
		if errors.Is(err, device.ErrUnauthorized) && isTTY() {
			fmt.Println("The terminal has a comm key (ZKTeco: Menu › Comm. › PC Connection › Comm Key).")
			n, perr := strconv.ParseUint(prompt("Comm key", ""), 10, 32)
			if perr != nil {
				return fmt.Errorf("the comm key is a number")
			}
			t.Password = uint32(n)
			continue
		}
		if errors.Is(err, device.ErrUnauthorized) {
			return fmt.Errorf("%s: %w; pass it with --password", t.Addr(), err)
		}
		return fmt.Errorf("could not connect to %s: %w", t.Addr(), err)
	}
	fmt.Println("ok")
	fmt.Printf("  serial %s, %s, firmware %s, %d users, %d stored punches\n", info.Serial, orUnknown(info.Model), info.Firmware, info.Users, info.Records)
	warnClock(info.clock)
	replaced := false
	for i, e := range cfg.Terminals {
		if e.Addr() == t.Addr() {
			cfg.Terminals[i], replaced = t, true
		}
	}
	if !replaced {
		cfg.Terminals = append(cfg.Terminals, t)
	}
	fmt.Println("  Once the relay reports it, add it in UPAX from the relay's discovered terminals.")
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "model unknown"
	}
	return s
}

// waitForService waits for the freshly started service to reach UPAX, then
// (in a terminal) for the relay to be activated, so setup reports what
// actually happened rather than that it started.
func waitForService(ctx context.Context, cfg *config.Config, since time.Time) error {
	fmt.Print("\nWaiting for the relay service to reach UPAX ... ")
	deadline := time.Now().Add(90 * time.Second)
	var h *relay.RelayHealth
	for {
		var err error
		h, err = relay.LoadRelayHealth(cfg.StateDir())
		fresh := err == nil && h.StartedAt.After(since)
		if fresh && (h.LastSync.After(since) || h.UPAXStatus == "unpaired" || h.UPAXStatus == "signature_invalid") {
			break
		}
		if time.Now().After(deadline) {
			fmt.Println("no contact")
			if fresh && h.LastError != "" {
				fmt.Println("  last error:", h.LastError)
			} else if !fresh {
				fmt.Println("  the service has not started; check the system log, or run \"upax-relay run\" to see why")
			}
			return fmt.Errorf("the relay service has not reached UPAX yet; see %s", filepath.Join(cfg.StateDir(), "relay.log"))
		}
		if !sleepCtx(ctx, time.Second) {
			fmt.Println()
			return nil
		}
	}
	fmt.Println("ok")
	fmt.Println("  " + describeRelay(h.UPAXStatus))
	if h.UPAXStatus != upax.StatusPending || !isTTY() {
		return nil
	}
	fmt.Print("\nWaiting for activation in UPAX (Ctrl-C to finish setup now; the service keeps waiting) ... ")
	for {
		if !sleepCtx(ctx, 2*time.Second) {
			fmt.Println("\n  Setup is done; the relay starts uploading once UPAX activates it.")
			return nil
		}
		if h, err := relay.LoadRelayHealth(cfg.StateDir()); err == nil && h.UPAXStatus != upax.StatusPending {
			fmt.Println("ok")
			fmt.Println("  " + describeRelay(h.UPAXStatus))
			return nil
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func warnClock(t time.Time) {
	if t.IsZero() {
		return
	}
	now := time.Now()
	local := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), 0, time.UTC)
	if skew := t.Sub(local); skew > 2*time.Minute || skew < -2*time.Minute {
		fmt.Printf("  warning: the terminal clock (%s) differs from this computer (%s) by %s; punches carry the terminal's time.\n",
			t.Format(time.DateTime), local.Format(time.DateTime), skew.Round(time.Second))
		fmt.Println("  UPAX can keep it right: turn on clock sync for the terminal in UPAX.")
	}
}

func cmdTest(args []string) error {
	fs, path := newFlags("test")
	_ = fs.Parse(args)
	cfg, _, api, err := loadPaired(*path)
	if err != nil {
		return wrapPerm(err)
	}
	ctx := context.Background()
	failed := false
	var reports []upax.TerminalReport
	for _, t := range cfg.Terminals {
		fmt.Printf("%s\n  terminal %s ... ", t.Label(), t.Addr())
		info, err := probeTerminal(ctx, t)
		if err != nil {
			fmt.Println("FAILED:", err)
			failed = true
			continue
		}
		fmt.Printf("ok (serial %s, firmware %s, %d punches)\n", info.Serial, info.Firmware, info.Records)
		warnClock(info.clock)
		reports = append(reports, upax.TerminalReport{
			Serial: info.Serial, Vendor: t.VendorName(), LANAddr: t.Host, Connection: upax.ConnPull, Reachable: true,
			Model: info.Model, Firmware: info.Firmware, Users: info.Users, Fingerprints: info.Fingerprints, Records: info.Records,
		})
	}
	fmt.Printf("UPAX %s ... ", cfg.UPAX)
	rep, err := api.Sync(ctx, upax.SyncRequest{Version: version, HostName: hostName(), Terminals: reports})
	if err != nil {
		fmt.Println("FAILED:", explainAPI(err))
		return errors.New("some checks failed")
	}
	fmt.Println("ok (signed request accepted)")
	fmt.Println("  " + describeRelay(rep.Status))
	if off, ok := api.ClockOffset(); ok && (off > 30*time.Second || off < -30*time.Second) {
		fmt.Printf("  warning: this computer's clock is %s off UPAX's; turn on automatic time\n", off)
	}
	registered := map[string]upax.TerminalState{}
	for _, t := range rep.Terminals {
		registered[t.Serial] = t
	}
	for _, r := range reports {
		if st, ok := registered[r.Serial]; ok {
			fmt.Printf("  terminal %s: %s in UPAX\n", r.Serial, st.Status)
		} else if rep.Status == upax.StatusActive {
			fmt.Printf("  terminal %s: not added in UPAX yet (add it from the relay's discovered terminals)\n", r.Serial)
		}
	}
	if failed {
		return errors.New("some checks failed")
	}
	return nil
}

func explainAPI(err error) string {
	switch {
	case upax.IsStatus(err, 404):
		return "UPAX doesn't know this relay (unpaired or retired); pair it again with: upax-relay pair"
	case upax.IsCode(err, upax.CodeClockSkew):
		return "this computer's clock is too far off; turn on automatic time"
	case upax.IsStatus(err, 401):
		return "UPAX refused the relay's signature; pair it again with: upax-relay pair"
	}
	return err.Error()
}

func describeRelay(status string) string {
	switch status {
	case upax.StatusActive:
		return "active: UPAX accepts punches through this relay"
	case upax.StatusPending:
		return "waiting for activation: in UPAX, open HR › Devices › Relays, check the fingerprint matches, and activate. Punches stay on the terminals until then"
	case upax.StatusPaused:
		return "paused in UPAX: resume the relay there to upload punches"
	case "unpaired":
		return "UPAX doesn't know this relay (unpaired or retired); pair it again with: upax-relay pair"
	case "signature_invalid":
		return "UPAX refused the relay's signature; pair it again with: upax-relay pair"
	case "":
		return "not reached UPAX yet"
	}
	return status
}
