package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/UPAX-platform/attendance-relay/internal/device/drivers"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	c, err := Load(write(t, `version: 2
upax: https://acme.upax.io
relay_id: 01q2a0000000000000000000ab
terminals:
  - name: Front door
    host: 192.168.1.201
    password: 12
    manage_comm_key: false
manage_comm_keys: true
listener: {enabled: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	tm := c.Terminals[0]
	if tm.Addr() != "192.168.1.201:4370" || tm.VendorName() != "zkteco" || c.ManagesCommKey(tm) {
		t.Fatalf("terminal %+v", tm)
	}
	if c.Listener.Bind != DefaultListenerBind || c.Backfill != "all" {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c.KeyPath() != filepath.Join(filepath.Dir(c.Path()), "relay.key") {
		t.Fatal(c.KeyPath())
	}
}

func TestRejects(t *testing.T) {
	for name, body := range map[string]string{
		"old format":   "devices:\n  - host: 1.2.3.4\n    upax_url: https://x/dev/abc\n",
		"no version":   "upax: https://acme.upax.io\nterminals: []\n",
		"plain http":   "version: 2\nupax: http://acme.upax.io\nterminals: []\n",
		"path in upax": "version: 2\nupax: https://acme.upax.io/dev/tok\nterminals: []\n",
		"bad vendor":   "version: 2\nupax: https://acme.upax.io\nterminals: [{host: 1.2.3.4, vendor: acme}]\n",
		"duplicate":    "version: 2\nupax: https://acme.upax.io\nterminals: [{host: 1.2.3.4}, {host: 1.2.3.4, port: 4370}]\n",
		"bad relay id": "version: 2\nupax: https://acme.upax.io\nrelay_id: XYZ\nterminals: []\n",
		"bad backfill": "version: 2\nupax: https://acme.upax.io\nbackfill: some\nterminals: []\n",
	} {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSaveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	c := New(p)
	c.UPAX = "https://acme.upax.io"
	c.Terminals = []Terminal{{Name: "A", Host: "10.0.0.5"}}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if err := c.SetPassword("10.0.0.5:4370", 777); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Terminals[0].Password != 777 {
		t.Fatalf("password %d", got.Terminals[0].Password)
	}
	b, _ := os.ReadFile(p)
	if !strings.HasPrefix(string(b), "# UPAX attendance relay") || !strings.Contains(string(b), "version: 2") {
		t.Fatalf("file:\n%s", b)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}
