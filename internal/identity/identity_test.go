package identity

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestFingerprint(t *testing.T) {
	k, err := FromSeed(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	fp := k.Fingerprint()
	if len(fp) != 26 {
		t.Fatalf("fingerprint %q is %d chars, want 26", fp, len(fp))
	}
	if g := Group(fp); len(g) != 32 || g[4] != '-' || g[29] != '-' {
		t.Fatalf("grouped %q", g)
	}
	// Bits are most-significant first: the first character is the top
	// five bits of the hash.
	if encoding.EncodeToString([]byte{0xff, 0}) != "ZW00" {
		t.Fatalf("alphabet/bit order: %q", encoding.EncodeToString([]byte{0xff, 0}))
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.key")
	k, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Save(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
		}
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fingerprint() != k.Fingerprint() {
		t.Fatal("loaded key differs")
	}
}

func TestSourceReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.key")
	s := &Source{Path: path}
	if _, err := s.Key(); err != ErrNoKey {
		t.Fatalf("no file: %v, want ErrNoKey", err)
	}
	a, _ := Generate()
	_ = a.Save(path)
	k, err := s.Key()
	if err != nil || k.Fingerprint() != a.Fingerprint() {
		t.Fatalf("first key: %v", err)
	}
	b, _ := Generate()
	_ = b.Save(path)
	_ = os.Chtimes(path, time.Now(), time.Now().Add(time.Second))
	if k, _ := s.Key(); k.Fingerprint() != b.Fingerprint() {
		t.Fatal("source did not reload the rotated key")
	}
}

func TestPairingCode(t *testing.T) {
	for in, want := range map[string]string{
		"RLY-7KQ2-M9XD-4TPA": "RLY-7KQ2-M9XD-4TPA",
		"rly 7kq2 m9xd 4tpa": "RLY-7KQ2-M9XD-4TPA",
		"7KQ2M9XD4TPA":       "RLY-7KQ2-M9XD-4TPA",
		"RLY-OKQI-M9XD-4TPL": "RLY-0KQ1-M9XD-4TP1",
	} {
		got, err := PairingCode(in)
		if err != nil || got != want {
			t.Errorf("PairingCode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "RLY-7KQ2-M9XD", "RLY-7KQ2-M9XD-4TPU", "RLY-7KQ2-M9XD-4TPA-1"} {
		if _, err := PairingCode(bad); err == nil {
			t.Errorf("PairingCode(%q) accepted", bad)
		}
	}
}
