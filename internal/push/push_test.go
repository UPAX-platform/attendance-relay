package push

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/device"
)

func TestIsPrivate(t *testing.T) {
	for addr, want := range map[string]bool{
		"192.168.1.201": true, "10.0.0.5": true, "172.16.4.4": true, "127.0.0.1": true,
		"169.254.10.1": true, "fd00::1": true, "fe80::1": true, "::1": true,
		"8.8.8.8": false, "172.32.0.1": false, "2001:4860::8888": false, "100.64.0.1": false,
	} {
		if got := IsPrivate(&net.TCPAddr{IP: net.ParseIP(addr)}); got != want {
			t.Errorf("IsPrivate(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestValidSerial(t *testing.T) {
	for s, want := range map[string]bool{"CQZ7232560123": true, "A-1_b.2": true, "": false, "../x": false, "a b": false, "a/b": false} {
		if ValidSerial(s) != want {
			t.Errorf("ValidSerial(%q) != %v", s, want)
		}
	}
}

func TestSpool(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool-X")
	s, err := OpenSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)
	p := func(pin string, m int) device.Punch {
		return device.Punch{PIN: pin, Time: t0.Add(time.Duration(m) * time.Minute), State: 1, Verify: 15, WorkCode: "0"}
	}
	if err := s.Append([]device.Punch{p("1", 0), p("2", 1), p("3", 2)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append([]device.Punch{p("4", 3)}); err != nil {
		t.Fatal(err)
	}
	if s.Pending() != 4 {
		t.Fatalf("pending %d", s.Pending())
	}
	name, got, ok, err := s.Oldest()
	if err != nil || !ok || len(got) != 3 || got[0] != p("1", 0) {
		t.Fatalf("oldest %v %v %v", got, ok, err)
	}
	// Part of a file uploaded, then a restart.
	if err := s.Done(name, 2); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Pending() != 2 {
		t.Fatalf("pending after reopen %d, want 2", s2.Pending())
	}
	_, got, _, _ = s2.Oldest()
	if len(got) != 1 || got[0].PIN != "3" {
		t.Fatalf("remaining %v", got)
	}
	name, _, _, _ = s2.Oldest()
	_ = s2.Done(name, 1)
	_, got, _, _ = s2.Oldest()
	if got[0].PIN != "4" {
		t.Fatalf("next file %v", got)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("spool dir mode %v", fi.Mode().Perm())
	}
}

func TestSpoolSkipsCorruptAndEmptyFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool-X")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"00000000000000000001.punches": "{not json",
		"00000000000000000002.punches": `[{"pin":"1","local":"yesterday"}]`,
		"00000000000000000003.punches": "null",
		"00000000000000000004.punches": `[{"pin":"7","local":"2026-10-28 08:00:00","state":0,"verify":1,"workcode":"0"}]`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := OpenSpool(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Pending() != 1 {
		t.Fatalf("pending %d, want 1", s.Pending())
	}
	name, got, ok, err := s.Oldest()
	if err != nil || !ok || len(got) != 1 || got[0].PIN != "7" {
		t.Fatalf("oldest %v %v %v", got, ok, err)
	}
	if err := s.Done(name, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.Oldest(); ok || err != nil {
		t.Fatalf("spool not empty: %v %v", ok, err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.corrupt")); len(m) != 2 {
		t.Fatalf("set aside %v, want the 2 unreadable files", m)
	}
}
