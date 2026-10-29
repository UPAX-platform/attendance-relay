package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/kardianos/service"

	"github.com/UPAX-platform/attendance-relay/internal/config"
	"github.com/UPAX-platform/attendance-relay/internal/identity"
	"github.com/UPAX-platform/attendance-relay/internal/relay"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
)

// pair registers this relay's key with UPAX using a one-time code, and
// records the relay id in cfg (saved). A new key is generated unless one
// exists and newKey is false: re-pairing the same key restores a lost UPAX
// record without changing the fingerprint.
func pair(ctx context.Context, cfg *config.Config, code string, newKey bool) (*identity.Key, upax.PairReply, error) {
	code, err := identity.PairingCode(code)
	if err != nil {
		return nil, upax.PairReply{}, err
	}
	path := cfg.KeyPath()
	key, err := identity.Load(path)
	fresh := newKey || errors.Is(err, os.ErrNotExist)
	switch {
	case fresh:
		fmt.Print("Generating this relay's key ... ")
		if key, err = identity.Generate(); err != nil {
			return nil, upax.PairReply{}, err
		}
		// Kept aside until UPAX accepts it, so a failed pairing never
		// replaces a key that works.
		if err := key.Save(path + ".pending"); err != nil {
			return nil, upax.PairReply{}, wrapPerm(err)
		}
		fmt.Println("ok")
	case err != nil:
		return nil, upax.PairReply{}, wrapPerm(err)
	}

	fmt.Print("Pairing with UPAX ... ")
	api := &upax.Client{Base: cfg.UPAX, UserAgent: userAgent()}
	rep, err := api.Pair(ctx, code, key, upax.PairRequest{HostName: hostName(), OS: runtime.GOOS, Arch: runtime.GOARCH, Version: version})
	if err != nil {
		fmt.Println("failed")
		os.Remove(path + ".pending")
		switch {
		case upax.IsCode(err, upax.CodePairingUsed):
			return nil, rep, errors.New("this pairing code was already used. Ask your UPAX admin for a new one; if you didn't use it, tell them")
		case upax.IsStatus(err, 404):
			return nil, rep, errors.New("UPAX doesn't know this pairing code (wrong, expired or already used); ask for a new one in HR › Devices › Relays")
		}
		return nil, rep, fmt.Errorf("could not pair: %w", err)
	}
	fmt.Println("ok")
	if fresh {
		if err := os.Rename(path+".pending", path); err != nil {
			return nil, rep, err
		}
		os.Remove(path + ".prev")
	}
	cfg.RelayID = rep.RelayID
	if err := cfg.Save(); err != nil {
		return nil, rep, wrapPerm(err)
	}
	return key, rep, nil
}

func printFingerprint(k *identity.Key) {
	fmt.Println()
	fmt.Println("  This relay's fingerprint:  " + identity.Group(k.Fingerprint()))
	fmt.Println("  In UPAX, open the relay (HR › Devices › Relays), check that the fingerprint")
	fmt.Println("  there matches this one exactly, and click Activate.")
}

func cmdPair(args []string) error {
	fs, path := newFlags("pair")
	code := fs.String("code", "", "pairing code from UPAX (RLY-XXXX-XXXX-XXXX)")
	base := fs.String("upax", "", "UPAX address, e.g. https://<your-org>.upax.io (default: the configured one)")
	newKey := fs.Bool("new-key", false, "generate a new key instead of re-pairing the existing one")
	_ = fs.Parse(args)

	cfg, err := config.Load(*path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		cfg = config.New(*path)
	case err != nil:
		return wrapPerm(err)
	}
	if *base != "" {
		cfg.UPAX = strings.TrimRight(strings.TrimSpace(*base), "/")
	}
	if cfg.UPAX == "" {
		cfg.UPAX = prompt("UPAX address (https://<your-org>.upax.io)", "")
	}
	if err := upax.ValidateBase(cfg.UPAX); err != nil {
		return err
	}
	if *code == "" {
		*code = prompt("Pairing code (HR › Devices › Relays › Add relay)", "")
	}
	key, _, err := pair(context.Background(), cfg, *code, *newKey)
	if err != nil {
		return err
	}
	printFingerprint(key)
	restartIfRunning(cfg.Path())
	return nil
}

// restartIfRunning restarts the service so it picks up a new pairing or
// config. It reports whether the service was running.
func restartIfRunning(cfgPath string) bool {
	s, _, err := newService(cfgPath, "")
	if err != nil {
		return false
	}
	if st, err := s.Status(); err != nil || st != service.StatusRunning {
		return false
	}
	if err := s.Restart(); err != nil {
		fmt.Println("\nCould not restart the relay service; restart it to apply the changes:", err)
	} else {
		fmt.Println("\nRestarted the relay service.")
	}
	return true
}

func cmdUnpair(args []string) error {
	fs, path := newFlags("unpair")
	force := fs.Bool("yes", false, "don't ask for confirmation")
	_ = fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return wrapPerm(err)
	}
	if !*force && (!isTTY() || !yes("Forget this relay's key and pairing? Punches stay on the terminals until it is paired again", false)) {
		return errors.New("not unpaired")
	}
	if s, _, err := newService(cfg.Path(), ""); err == nil {
		if st, err := s.Status(); err == nil && st == service.StatusRunning {
			if err := s.Stop(); err == nil {
				fmt.Println("Stopped the relay service.")
			}
		}
	}
	for _, suffix := range []string{"", ".prev", ".next", ".pending"} {
		if err := os.Remove(cfg.KeyPath() + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return wrapPerm(err)
		}
	}
	cfg.RelayID = ""
	if err := cfg.Save(); err != nil {
		return wrapPerm(err)
	}
	fmt.Println("Unpaired. Also unpair or retire the relay in UPAX (HR › Devices › Relays).")
	fmt.Println("Pair again any time with: upax-relay pair")
	return nil
}

func cmdRotateKey(args []string) error {
	fs, path := newFlags("rotate-key")
	_ = fs.Parse(args)
	cfg, _, api, err := loadPaired(*path)
	if err != nil {
		return wrapPerm(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	next, err := relay.RotateKey(ctx, api, cfg.KeyPath(), upax.SyncRequest{Version: version, HostName: hostName()})
	if err != nil {
		return wrapPerm(fmt.Errorf("could not rotate the key: %w", err))
	}
	fmt.Println("Key rotated. New fingerprint:", identity.Group(next.Fingerprint()))
	fmt.Println("UPAX accepts the old key for another 24 hours; the running service switches by itself.")
	return nil
}

func cmdShowCommKey(args []string) error {
	fs, path := newFlags("show-comm-key")
	_ = fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return wrapPerm(err)
	}
	if fs.NArg() != 1 {
		return errors.New("usage: upax-relay show-comm-key <terminal name or address>")
	}
	want := fs.Arg(0)
	for _, t := range cfg.Terminals {
		if strings.EqualFold(t.Name, want) || t.Host == want || t.Addr() == want {
			if t.Password == 0 {
				fmt.Printf("%s has no comm key.\n", t.Label())
			} else {
				fmt.Printf("%s comm key: %d\n", t.Label(), t.Password)
			}
			return nil
		}
	}
	return fmt.Errorf("no terminal %q in %s", want, cfg.Path())
}
