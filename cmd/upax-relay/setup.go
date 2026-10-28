package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/UPAX-platform/zk-relay/internal/adms"
	"github.com/UPAX-platform/zk-relay/internal/config"
	"github.com/UPAX-platform/zk-relay/internal/relay"
	"github.com/UPAX-platform/zk-relay/internal/zk"
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

type deviceInfo struct {
	serial, firmware string
	sizes            zk.Sizes
	clock            time.Time
}

func probeDevice(ctx context.Context, d config.Device) (deviceInfo, error) {
	c, err := zk.Dial(ctx, d.Addr(), d.Password, 8*time.Second)
	if err != nil {
		return deviceInfo{}, err
	}
	defer c.Close()
	var in deviceInfo
	if in.serial, err = c.SerialNumber(); err != nil {
		return in, err
	}
	in.firmware, _ = c.Firmware()
	in.sizes, _ = c.Sizes()
	in.clock, _ = c.Time()
	return in, nil
}

// probeUPAX handshakes as the device. It carries the device summary, so a
// device UPAX hasn't activated yet shows its firmware next to the serial.
func probeUPAX(ctx context.Context, url string, d config.Device, in deviceInfo) (adms.Options, error) {
	ac := &adms.Client{Base: url, SN: in.serial, UserAgent: "upax-zk-relay/" + version}
	info := fmt.Sprintf("%s,%d,%d,%d,%s", in.firmware, in.sizes.Users, in.sizes.Fingers, in.sizes.Records, d.Host)
	return ac.Handshake(ctx, info)
}

// describeStatus explains a device's UPAX status in one line.
func describeStatus(o adms.Options, serial string) string {
	switch o.Status {
	case adms.StatusActive:
		return "active, UPAX accepts punches from this device"
	case adms.StatusPending:
		return fmt.Sprintf("waiting for activation: open HR > Devices in UPAX and activate the device with serial %s. Punches stay on the device until then", serial)
	case adms.StatusPaused:
		return "paused in UPAX: resume the device there to upload punches"
	}
	return "ok"
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
	url := fs.String("url", "", "UPAX device address, e.g. https://upax.example.com/<device-address>")
	dev := fs.String("device", "", "device IP or host[:port]")
	pw := fs.Uint("password", 0, "device communication password (comm key), if one is set")
	name := fs.String("name", "", "friendly name for the device")
	backfill := fs.String("backfill", "", "first run: upload stored history (all), only new punches (none), or history from a date (since:YYYY-MM-DD)")
	install := fs.String("install", "ask", "install and start the background service: yes, no or ask")
	_ = fs.Parse(args)
	ctx := context.Background()

	fmt.Println("UPAX ZKTeco relay setup")
	fmt.Println("=======================")

	cfg, err := config.Load(*path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg = config.New(*path)
	case cfg == nil:
		return err
	}

	// 1. the device
	d := config.Device{Name: *name, Password: uint32(*pw)}
	host := *dev
	if host == "" {
		fmt.Println("\nThe relay talks to the device over your local network (TCP port 4370).")
		fmt.Println("Find its IP on the device: Menu > Comm. > Ethernet. Leave blank to scan the network.")
		host = prompt("Device IP address", "")
		if host == "" {
			found := scanNetwork(ctx, "", true)
			if len(found) == 0 {
				return errors.New("no ZKTeco devices found; make sure the device is on the same network and enter its IP")
			}
			host = found[0].addr
			if len(found) > 1 {
				for i, f := range found {
					fmt.Printf("  %d) %s  %s\n", i+1, f.addr, f.serial)
				}
				n, _ := strconv.Atoi(prompt("Which device", "1"))
				if n < 1 || n > len(found) {
					n = 1
				}
				host = found[n-1].addr
			}
		}
	}
	d.Host, d.Port = splitHostPort(host)

	var info deviceInfo
	for {
		fmt.Printf("\nConnecting to device %s ... ", d.Addr())
		info, err = probeDevice(ctx, d)
		if err == nil {
			break
		}
		fmt.Println("failed")
		if errors.Is(err, zk.ErrUnauthorized) && isTTY() {
			fmt.Println("The device has a communication password (Menu > Comm. > PC Connection > Comm Key).")
			n, perr := strconv.ParseUint(prompt("Comm key", ""), 10, 32)
			if perr != nil {
				return fmt.Errorf("comm key must be a number")
			}
			d.Password = uint32(n)
			continue
		}
		if errors.Is(err, zk.ErrUnauthorized) {
			return fmt.Errorf("%s: %w; pass it with --password", d.Addr(), err)
		}
		return fmt.Errorf("could not connect to %s: %w", d.Addr(), err)
	}
	fmt.Println("ok")
	fmt.Printf("  serial %s, firmware %s, %d users, %d stored punches\n", info.serial, info.firmware, info.sizes.Users, info.sizes.Records)
	warnClock(info.clock)

	// 2. UPAX address
	existing := ""
	for _, e := range cfg.Devices {
		if e.Host == d.Host && e.Port == d.Port {
			existing = cfg.URLFor(e)
			if d.Name == "" {
				d.Name = e.Name
			}
		}
	}
	u := *url
	if u == "" {
		fmt.Println("\nPaste the device address UPAX gave you for this device")
		fmt.Println("(looks like https://upax.example.com/<device-address>).")
		u = prompt("UPAX device address", existing)
	}
	u = strings.TrimRight(strings.TrimSpace(u), "/")
	if err := config.ValidateURL(u); err != nil {
		return err
	}
	if existing != "" && existing != u {
		fmt.Println("  This device was set up with a different UPAX address. If the new address is a new")
		fmt.Println("  device in UPAX, the relay uploads the device's punches to it again (see backfill).")
	}
	fmt.Printf("\nChecking UPAX ... ")
	opts, err := probeUPAX(ctx, u, d, info)
	if err != nil {
		fmt.Println("failed")
		if adms.IsNotFound(err) {
			return fmt.Errorf("UPAX does not recognise %s — double-check the address", u)
		}
		return fmt.Errorf("could not reach UPAX: %w", err)
	}
	fmt.Println("ok")
	fmt.Println("  " + describeStatus(opts, info.serial))
	if opts.ATTLOGStamp > 0 {
		fmt.Println("  UPAX already has punches from this device; any the relay re-sends are skipped there.")
	}
	d.UpaxURL = u

	// 3. history
	if *backfill != "" {
		cfg.Backfill = *backfill
	} else if info.sizes.Records > 0 && opts.ATTLOGStamp == 0 && isTTY() {
		fmt.Printf("\nThe device holds %d past punches. Upload them to UPAX on first start?\n", info.sizes.Records)
		fmt.Println("  all = every stored punch, none = only new punches, since:YYYY-MM-DD = from that date")
		for {
			b := prompt("Upload", "all")
			if err := config.ValidateBackfill(b); err != nil {
				fmt.Println(" ", err)
				continue
			}
			cfg.Backfill = b
			break
		}
	}

	// 4. save
	replaced := false
	for i, e := range cfg.Devices {
		if e.Host == d.Host && e.Port == d.Port {
			cfg.Devices[i], replaced = d, true
		}
	}
	if !replaced {
		cfg.Devices = append(cfg.Devices, d)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("\nSaved %s\n", cfg.Path())

	// 5. service
	doInstall := *install == "yes" || (*install == "ask" && isTTY() && yes("Install and start the relay as a background service?", true))
	if !doInstall {
		fmt.Printf("\nInstall and start the service later with:  upax-relay install --config %q && upax-relay start\n", cfg.Path())
		return nil
	}
	started := time.Now()
	if err := installAndStart(cfg.Path()); err != nil {
		return err
	}
	if err := waitForService(cfg, d, started); err != nil {
		return err
	}
	fmt.Println("\nDone. The relay runs in the background and starts automatically at boot.")
	fmt.Println("Check on it any time with:  upax-relay status")
	return nil
}

// waitForService waits for the freshly started service to reach UPAX for d,
// so setup reports what actually happened rather than that it started.
func waitForService(cfg *config.Config, d config.Device, since time.Time) error {
	fmt.Print("\nWaiting for the relay service to reach UPAX ... ")
	path := relay.HealthPath(cfg.StateDir(), d.Addr())
	deadline := time.Now().Add(90 * time.Second)
	for {
		h, err := relay.LoadHealth(path)
		fresh := err == nil && h.StartedAt.After(since)
		switch {
		case fresh && h.LastHandshake.After(since):
			fmt.Println("ok")
			status := h.UPAXStatus
			if status == "unknown" {
				status = ""
			}
			fmt.Println("  " + describeStatus(adms.Options{Status: status}, h.Serial))
			return nil
		case fresh && h.UPAXStatus == "rejected":
			fmt.Println("failed")
			return errors.New("UPAX rejected the device address; check it in UPAX and run setup again")
		case time.Now().After(deadline):
			fmt.Println("no contact")
			if fresh && h.LastError != "" {
				fmt.Println("  last error:", h.LastError)
			} else if !fresh {
				fmt.Println("  the service has not started; check the system log, or run \"upax-relay run\" to see why")
			}
			return fmt.Errorf("the relay service has not reached UPAX yet; see %s", filepath.Join(cfg.StateDir(), "relay.log"))
		}
		time.Sleep(time.Second)
	}
}

func warnClock(t time.Time) {
	if t.IsZero() {
		return
	}
	now := time.Now()
	local := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second(), 0, time.UTC)
	if skew := t.Sub(local); skew > 2*time.Minute || skew < -2*time.Minute {
		fmt.Printf("  warning: device clock (%s) differs from this computer (%s) by %s — punches are sent with the device's time\n",
			t.Format(time.DateTime), local.Format(time.DateTime), skew.Round(time.Second))
	}
}

func isTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

func cmdTest(args []string) error {
	fs, path := newFlags("test")
	_ = fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx := context.Background()
	failed := false
	for _, d := range cfg.Devices {
		fmt.Printf("%s\n  device %s ... ", d.Label(), d.Addr())
		info, err := probeDevice(ctx, d)
		if err != nil {
			fmt.Println("FAILED:", err)
			failed = true
			continue
		}
		fmt.Printf("ok (serial %s, firmware %s, %d punches)\n", info.serial, info.firmware, info.sizes.Records)
		warnClock(info.clock)
		sn := info.serial
		if d.Serial != "" {
			sn = d.Serial
		}
		fmt.Printf("  UPAX %s ... ", cfg.URLFor(d))
		info.serial = sn
		opts, err := probeUPAX(ctx, cfg.URLFor(d), d, info)
		if err != nil {
			fmt.Println("FAILED:", err)
			failed = true
			continue
		}
		fmt.Printf("ok (heartbeat %s, transfers %q)\n", opts.Delay, opts.TransFlag)
		fmt.Println("  " + describeStatus(opts, sn))
	}
	if failed {
		return errors.New("some checks failed")
	}
	return nil
}
