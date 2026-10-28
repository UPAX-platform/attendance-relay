// Package config loads and saves the relay configuration file.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Device is one ZKTeco terminal on the local network.
type Device struct {
	Name     string `yaml:"name,omitempty"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port,omitempty"`     // default 4370
	Password uint32 `yaml:"password,omitempty"` // device comm key, 0 = none
	// UpaxURL overrides the top-level upax_url for this device. Each
	// physical device should have its own UPAX device address.
	UpaxURL string `yaml:"upax_url,omitempty"`
	// Serial overrides the serial number read from the device.
	Serial string `yaml:"serial,omitempty"`
}

// Addr returns host:port.
func (d Device) Addr() string {
	p := d.Port
	if p == 0 {
		p = 4370
	}
	return net.JoinHostPort(d.Host, strconv.Itoa(p))
}

// Label is a human-friendly name for logs.
func (d Device) Label() string {
	if d.Name != "" {
		return d.Name
	}
	return d.Addr()
}

// Config is the whole file.
type Config struct {
	UpaxURL string   `yaml:"upax_url,omitempty"`
	Devices []Device `yaml:"devices"`
	// Backfill controls what happens the first time a device is seen:
	// "all" uploads the device's stored history, "none" only new punches.
	Backfill string `yaml:"backfill,omitempty"`
	// PollInterval is how often the device is checked for new punches.
	PollInterval Duration `yaml:"poll_interval,omitempty"`
	// DataDir holds state and logs; defaults to the config file's directory.
	DataDir string `yaml:"data_dir,omitempty"`

	path string
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

// Load reads and validates the config at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.path = path
	c.applyDefaults()
	return c, c.Validate()
}

// New returns an empty config that will be saved at path.
func New(path string) *Config {
	c := &Config{path: path}
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
}

// StateDir is where state and logs are kept.
func (c *Config) StateDir() string {
	if c.DataDir != "" {
		return c.DataDir
	}
	return filepath.Dir(c.path)
}

// Path is the file this config was loaded from.
func (c *Config) Path() string { return c.path }

// URLFor returns the UPAX device address to use for d.
func (c *Config) URLFor(d Device) string {
	if d.UpaxURL != "" {
		return d.UpaxURL
	}
	return c.UpaxURL
}

// Validate checks the config for mistakes a user is likely to make.
func (c *Config) Validate() error {
	var errs []error
	if len(c.Devices) == 0 {
		errs = append(errs, errors.New("no devices configured"))
	}
	if c.Backfill != "all" && c.Backfill != "none" {
		errs = append(errs, fmt.Errorf("backfill must be \"all\" or \"none\", got %q", c.Backfill))
	}
	urls := map[string]string{}
	for i, d := range c.Devices {
		if d.Host == "" {
			errs = append(errs, fmt.Errorf("devices[%d]: host is required", i))
		}
		u := c.URLFor(d)
		if err := ValidateURL(u); err != nil {
			errs = append(errs, fmt.Errorf("devices[%d] (%s): %w", i, d.Label(), err))
		}
		if prev, dup := urls[u]; dup {
			errs = append(errs, fmt.Errorf("devices %s and %s share the same UPAX address; give each device its own upax_url", prev, d.Label()))
		}
		urls[u] = d.Label()
	}
	return errors.Join(errs...)
}

// ValidateURL checks a UPAX device address.
func ValidateURL(s string) error {
	if s == "" {
		return errors.New("upax_url is required")
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("upax_url %q is not a valid http(s) URL", s)
	}
	if strings.Contains(u.Path, "/iclock") {
		return fmt.Errorf("upax_url should be the device address from UPAX, without /iclock/...")
	}
	return nil
}

// Save writes the config file (creating its directory).
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	b, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := "# UPAX ZKTeco relay configuration. See README for all options.\n"
	return os.WriteFile(c.path, append([]byte(header), b...), 0o600)
}
