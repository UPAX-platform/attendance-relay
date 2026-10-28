package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/UPAX-platform/zk-relay/internal/adms"
	"github.com/UPAX-platform/zk-relay/internal/config"
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

func probeUPAX(ctx context.Context, url, serial string) (adms.Options, error) {
	ac := &adms.Client{Base: url, SN: serial, UserAgent: "upax-zk-relay/" + version}
	return ac.Handshake(ctx)
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
	backfill := fs.String("backfill", "", "first run: upload stored history (all) or only new punches (none)")
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
	fmt.Printf("\nChecking UPAX ... ")
	opts, err := probeUPAX(ctx, u, info.serial)
	if err != nil {
		fmt.Println("failed")
		if adms.IsNotFound(err) {
			return fmt.Errorf("UPAX does not recognise %s — double-check the address", u)
		}
		return fmt.Errorf("could not reach UPAX: %w", err)
	}
	fmt.Println("ok")
	if opts.ATTLOGStamp > 0 {
		fmt.Printf("  UPAX already has punches from this device (stamp %d); the relay will continue from there.\n", opts.ATTLOGStamp)
	}
	d.UpaxURL = u

	// 3. history
	if *backfill != "" {
		cfg.Backfill = *backfill
	} else if info.sizes.Records > 0 && opts.ATTLOGStamp == 0 && isTTY() {
		fmt.Printf("\nThe device holds %d past punches.\n", info.sizes.Records)
		if yes("Upload them to UPAX on first start?", true) {
			cfg.Backfill = "all"
		} else {
			cfg.Backfill = "none"
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
	if err := installAndStart(cfg.Path()); err != nil {
		return err
	}
	fmt.Println("\nDone. The relay is running and will start automatically at boot.")
	fmt.Println("Check on it any time with:  upax-relay status")
	return nil
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
		opts, err := probeUPAX(ctx, cfg.URLFor(d), sn)
		if err != nil {
			fmt.Println("FAILED:", err)
			failed = true
			continue
		}
		fmt.Printf("ok (heartbeat %s, stamp %d, transfers %q)\n", opts.Delay, opts.ATTLOGStamp, opts.TransFlag)
	}
	if failed {
		return errors.New("some checks failed")
	}
	return nil
}
