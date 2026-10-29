package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/config"
	"github.com/UPAX-platform/attendance-relay/internal/device/zkteco/zksim"
	"github.com/UPAX-platform/attendance-relay/internal/identity"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
	"github.com/UPAX-platform/attendance-relay/internal/upax/upaxtest"
)

// capture runs f with stdout redirected and returns what it printed.
func capture(t *testing.T, f func() error) (string, error) {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := f()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b), err
}

func TestPairTestShowCommKey(t *testing.T) {
	f := upaxtest.New()
	defer f.Close()
	f.Codes["RLY-7KQ2-M9XD-4TPA"] = "01q2a0000000000000000000cd"

	sim := zksim.New("SIM42")
	sim.Password = 31
	addr, _ := sim.Listen("127.0.0.1:0")
	defer sim.Close()
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)

	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.New(path)
	cfg.UPAX = f.URL
	cfg.Terminals = []config.Terminal{{Name: "Front door", Host: host, Port: p, Password: 31}}

	out, err := capture(t, func() error {
		k, _, err := pair(context.Background(), cfg, "rly 7kq2 m9xd 4tpa", false)
		if err == nil {
			printFingerprint(k)
		}
		return err
	})
	if err != nil {
		t.Fatalf("pair: %v\n%s", err, out)
	}
	k, err := identity.Load(cfg.KeyPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, identity.Group(k.Fingerprint())) {
		t.Fatalf("fingerprint not shown:\n%s", out)
	}
	saved, err := config.Load(path)
	if err != nil || saved.RelayID != "01q2a0000000000000000000cd" {
		t.Fatalf("saved config: %+v %v", saved, err)
	}

	// A second use of the code is refused and leaves the key alone.
	_, err = capture(t, func() error { _, _, err := pair(context.Background(), saved, "RLY-7KQ2-M9XD-4TPA", true); return err })
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("reused code: %v", err)
	}
	if k2, _ := identity.Load(cfg.KeyPath()); k2.Fingerprint() != k.Fingerprint() {
		t.Fatal("failed pairing replaced the key")
	}

	f.Do(func(f *upaxtest.Fake) { f.Status = upax.StatusActive })
	out, err = capture(t, func() error { return cmdTest([]string{"--config", path}) })
	if err != nil || !strings.Contains(out, "serial SIM42") || !strings.Contains(out, "signed request accepted") || !strings.Contains(out, "not added in UPAX yet") {
		t.Fatalf("test: %v\n%s", err, out)
	}

	out, err = capture(t, func() error { return cmdShowCommKey([]string{"--config", path, "front door"}) })
	if err != nil || !strings.Contains(out, "comm key: 31") {
		t.Fatalf("show-comm-key: %v\n%s", err, out)
	}

	out, err = capture(t, func() error { return cmdRotateKey([]string{"--config", path}) })
	if err != nil || !strings.Contains(out, "Key rotated") {
		t.Fatalf("rotate-key: %v\n%s", err, out)
	}
	if k3, _ := identity.Load(cfg.KeyPath()); k3.Fingerprint() == k.Fingerprint() {
		t.Fatal("key not rotated")
	}
}

func TestServiceProgram(t *testing.T) {
	f := upaxtest.New()
	defer f.Close()
	sim := zksim.New("SIM43")
	addr, _ := sim.Listen("127.0.0.1:0")
	defer sim.Close()
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	sim.Punch("1", time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC), 1, 0)

	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.New(path)
	cfg.UPAX, cfg.RelayID = f.URL, f.RelayID
	cfg.PollInterval = config.Duration(50 * time.Millisecond)
	cfg.Terminals = []config.Terminal{{Host: host, Port: p}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	k, _ := identity.Generate()
	_ = k.Save(cfg.KeyPath())
	f.Pair(k, upax.StatusActive)
	f.Register("SIM43", upaxtest.Terminal{})

	prog := &program{cfgPath: path}
	if err := prog.Start(nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.PunchCount("SIM43") == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	_ = prog.Stop(nil)
	if f.PunchCount("SIM43") != 1 {
		t.Fatal("the service didn't upload the punch")
	}
	if b, err := os.ReadFile(filepath.Join(cfg.StateDir(), "relay.log")); err != nil || !strings.Contains(string(b), "upax-relay starting") {
		t.Fatalf("log: %v\n%s", err, b)
	}
}

func TestSetupNewAddressPairsAgain(t *testing.T) {
	old, now := upaxtest.New(), upaxtest.New()
	defer old.Close()
	defer now.Close()
	old.Codes["RLY-7KQ2-M9XD-4TPA"] = "01q2a0000000000000000000aa"
	now.Codes["RLY-8KQ2-M9XD-4TPA"] = "01q2a0000000000000000000bb"

	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.New(path)
	cfg.UPAX = old.URL
	if out, err := capture(t, func() error { _, _, err := pair(context.Background(), cfg, "RLY-7KQ2-M9XD-4TPA", false); return err }); err != nil {
		t.Fatalf("pair: %v\n%s", err, out)
	}

	// Moved to another UPAX without a code on the command line: setup asks
	// for one instead of keeping the old relay id.
	defer func(r *bufio.Reader) { stdin = r }(stdin)
	stdin = bufio.NewReader(strings.NewReader("RLY-8KQ2-M9XD-4TPA\n"))
	out, err := capture(t, func() error {
		return cmdSetup([]string{"--config", path, "--upax", now.URL, "--listener", "no", "--install", "no"})
	})
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if strings.Contains(out, "already paired") {
		t.Fatalf("kept the old pairing:\n%s", out)
	}
	saved, err := config.Load(path)
	if err != nil || saved.UPAX != now.URL || saved.RelayID != "01q2a0000000000000000000bb" {
		t.Fatalf("saved config: %+v %v", saved, err)
	}
}
