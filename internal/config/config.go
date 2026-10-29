// Package config loads and saves the relay configuration file (version 2).
//
// The file holds no UPAX secret: the relay authenticates with its key
// (relay.key, next to the config). It does hold terminal comm keys, so it
// stays readable by administrators only.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/UPAX-platform/attendance-relay/internal/atomicfile"
	"github.com/UPAX-platform/attendance-relay/internal/device"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
)

// Version is the config format this relay reads.
const Version = 2

// Terminal is one terminal the relay pulls over the LAN.
type Terminal struct {
	Name string `yaml:"name,omitempty"`
	// Vendor selects the driver; default zkteco.
	Vendor   string `yaml:"vendor,omitempty"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port,omitempty"`     // default: the vendor's SDK port
	Password uint32 `yaml:"password,omitempty"` // terminal comm key, 0 = none
	// ManageCommKey overrides manage_comm_keys for this terminal.
	ManageCommKey *bool `yaml:"manage_comm_key,omitempty"`
}

// VendorName is the terminal's vendor with the default filled in.
func (t Terminal) VendorName() string {
	if t.Vendor == "" {
		return device.DefaultVendor
	}
	return t.Vendor
}

// Target is where to dial the terminal.
func (t Terminal) Target() device.Target {
	return device.Target{Host: t.Host, Port: t.Port, Password: t.Password}
}

// Addr returns host:port with the vendor's default port filled in.
func (t Terminal) Addr() string {
	port := 0
	if d, err := device.Lookup(t.Vendor); err == nil {
		port = d.DefaultPort()
	}
	return t.Target().Addr(port)
}

// Label is a human-friendly name for logs.
func (t Terminal) Label() string {
	if t.Name != "" {
		return t.Name
	}
	return t.Addr()
}

// Listener is the LAN server push-capable terminals send to.
type Listener struct {
	Enabled bool   `yaml:"enabled"`
	Bind    string `yaml:"bind,omitempty"`
}

// DefaultListenerBind is where the listener listens unless told otherwise.
const DefaultListenerBind = "0.0.0.0:8081"

// Config is the whole file.
type Config struct {
	Version int `yaml:"version"`
	// UPAX is the organisation's public plane origin: no path, no token.
	UPAX string `yaml:"upax"`
	// RelayID is written by pairing. Public, not a secret.
	RelayID   string     `yaml:"relay_id,omitempty"`
	Terminals []Terminal `yaml:"terminals"`
	// ManageCommKeys sets and keeps a random comm key on every pulled
	// terminal that has none (off by default).
	ManageCommKeys bool     `yaml:"manage_comm_keys"`
	Listener       Listener `yaml:"listener"`
	// Backfill controls the first upload from a pulled terminal: "all"
	// uploads its stored history, "none" only new punches,
	// "since:YYYY-MM-DD" stored punches on/after that date.
	Backfill string `yaml:"backfill,omitempty"`
	// PollInterval is how often each terminal's record counter is checked.
	PollInterval Duration `yaml:"poll_interval,omitempty"`
	// DataDir holds the key, state, health files and logs; defaults to the
	// config file's directory.
	DataDir string `yaml:"data_dir,omitempty"`

	path string
	mu   sync.Mutex // serialises Save (the service writes managed comm keys)
}

// Duration is a time.Duration that reads/writes as "5s".
type Duration time.Duration

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// DefaultPath is where the installed service looks for its config.
func DefaultPath() string {
	switch runtime.GOOS {
	case "windows":
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, "UPAX Relay", "config.yaml")
	case "darwin":
		return "/Library/Application Support/UPAX Relay/config.yaml"
	default:
		return "/etc/upax-relay/config.yaml"
	}
}

// Load reads and validates the config at path. Unknown keys are errors, so
// a typo doesn't silently change behaviour.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.path = path
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// New returns an empty config that will be saved at path.
func New(path string) *Config {
	c := &Config{Version: Version, path: path}
	c.applyDefaults()
	return c
}

func (c *Config) applyDefaults() {
	if c.Backfill == "" {
		c.Backfill = "all"
	}
	if c.PollInterval <= 0 {
		c.PollInterval = Duration(5 * time.Second)
	}
	// Faster than once a second only hammers the terminals.
	c.PollInterval = max(c.PollInterval, Duration(time.Second))
	if c.Listener.Bind == "" {
		c.Listener.Bind = DefaultListenerBind
	}
}

// StateDir is where the key, state and logs are kept.
func (c *Config) StateDir() string {
	if c.DataDir != "" {
		return c.DataDir
	}
	return filepath.Dir(c.path)
}

// KeyPath is the relay's private key file.
func (c *Config) KeyPath() string { return filepath.Join(c.StateDir(), "relay.key") }

// Path is the file this config was loaded from.
func (c *Config) Path() string { return c.path }

// ManagesCommKey reports whether the relay manages t's comm key.
func (c *Config) ManagesCommKey(t Terminal) bool {
	if t.ManageCommKey != nil {
		return *t.ManageCommKey
	}
	return c.ManageCommKeys
}

// Validate checks the config for mistakes a user is likely to make.
func (c *Config) Validate() error {
	var errs []error
	if c.Version != Version {
		errs = append(errs, fmt.Errorf("version must be %d (this relay reads config version %d only); run \"upax-relay setup\" to write a new config", Version, Version))
	}
	if err := upax.ValidateBase(c.UPAX); err != nil {
		errs = append(errs, fmt.Errorf("upax: %w", err))
	}
	if c.RelayID != "" {
		if err := upax.ValidateRelayID(c.RelayID); err != nil {
			errs = append(errs, err)
		}
	}
	if err := ValidateBackfill(c.Backfill); err != nil {
		errs = append(errs, err)
	}
	if c.Listener.Enabled {
		if _, _, err := net.SplitHostPort(c.Listener.Bind); err != nil {
			errs = append(errs, fmt.Errorf("listener.bind %q: %w", c.Listener.Bind, err))
		}
	}
	seen := map[string]string{}
	for i, t := range c.Terminals {
		if t.Host == "" {
			errs = append(errs, fmt.Errorf("terminals[%d]: host is required", i))
		}
		if _, err := device.Lookup(t.Vendor); err != nil {
			errs = append(errs, fmt.Errorf("terminals[%d] (%s): %w", i, t.Label(), err))
		}
		if prev, dup := seen[t.Addr()]; dup {
			errs = append(errs, fmt.Errorf("terminals %s and %s are the same address %s", prev, t.Label(), t.Addr()))
		}
		seen[t.Addr()] = t.Label()
	}
	return errors.Join(errs...)
}

// ValidateBackfill checks a backfill setting: all, none or since:YYYY-MM-DD.
func ValidateBackfill(s string) error {
	if s == "all" || s == "none" {
		return nil
	}
	d, ok := strings.CutPrefix(s, "since:")
	if _, err := time.Parse(time.DateOnly, d); !ok || err != nil {
		return fmt.Errorf("backfill must be \"all\", \"none\" or \"since:YYYY-MM-DD\", got %q", s)
	}
	return nil
}

// SetPassword records a terminal's new comm key and saves the file.
func (c *Config) SetPassword(addr string, key uint32) error {
	c.mu.Lock()
	for i := range c.Terminals {
		if c.Terminals[i].Addr() == addr {
			c.Terminals[i].Password = key
		}
	}
	c.mu.Unlock()
	return c.Save()
}

// Password returns the configured comm key of the terminal at addr.
func (c *Config) Password(addr string) uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.Terminals {
		if t.Addr() == addr {
			return t.Password
		}
	}
	return 0
}

// Save writes the config file atomically (creating its directory).
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	if err := atomicfile.SecureDir(filepath.Dir(c.path)); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	// It holds terminal comm keys.
	header := "# UPAX attendance relay configuration. See README for all options.\n"
	return atomicfile.WritePrivate(c.path, append([]byte(header), b...))
}
