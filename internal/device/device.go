// Package device is the relay's vendor-neutral view of an attendance
// terminal. Each vendor's protocol lives in its own package under
// internal/device (zkteco, ...) and registers a Driver here; the relay only
// ever talks to the interfaces below.
package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Target is where a pulled terminal is and how to log in to it.
type Target struct {
	Host     string
	Port     int    // 0 = the driver's default
	Password uint32 // the terminal's comm key / password, 0 = none
}

// Addr is host:port, with the driver's default port filled in.
func (t Target) Addr(defaultPort int) string {
	p := t.Port
	if p == 0 {
		p = defaultPort
	}
	return net.JoinHostPort(t.Host, strconv.Itoa(p))
}

// Info describes a terminal and its counters.
type Info struct {
	Serial       string
	Model        string
	Firmware     string
	Users        int
	Fingerprints int
	Records      int
}

// Punch is one attendance record as the terminal stores it. Time is the
// terminal's wall clock with no zone (carried as UTC; never convert it).
// State, Verify and WorkCode are the vendor's own codes: UPAX maps them with
// the vendor's tables, so the relay passes them through untouched.
type Punch struct {
	PIN      string
	Time     time.Time
	State    int
	Verify   int
	WorkCode string
}

// Conn is a session with one pulled terminal. It is not safe for concurrent
// use.
type Conn interface {
	// Info reads the terminal's identity and counters.
	Info() (Info, error)
	// RecordCount is the number of stored punches: cheap, polled often.
	RecordCount() (int, error)
	// Punches downloads the whole stored log, oldest first.
	Punches() ([]Punch, error)
	// Clock reads the terminal's wall clock (zone-less, as Punch.Time).
	Clock() (time.Time, error)
	// SetClock sets the terminal's wall clock.
	SetClock(time.Time) error
	// SetUser creates or updates a user. An empty name keeps the stored one;
	// a terminal administrator is never demoted.
	SetUser(pin, name string, privilege int) error
	// DeleteUser removes a user; deleting an unknown PIN is not an error.
	DeleteUser(pin string) error
	Close() error
}

// CommKeySetter is implemented by terminals whose comm key the relay can
// manage (manage_comm_keys).
type CommKeySetter interface {
	// SetCommKey sets the password new sessions must present. The current
	// session stays logged in.
	SetCommKey(key uint32) error
	// CommKeyRange is the inclusive range of valid keys.
	CommKeyRange() (min, max uint32)
}

// Driver connects to one vendor's terminals.
type Driver interface {
	// DefaultPort is the terminal's SDK port.
	DefaultPort() int
	// Dial opens and authenticates a session.
	Dial(ctx context.Context, t Target, timeout time.Duration) (Conn, error)
}

// ErrUnauthorized is returned by Dial when the terminal refuses the password.
var ErrUnauthorized = errors.New("the terminal rejected the communication password (comm key)")

// DefaultVendor is the vendor of a terminal whose config doesn't name one.
const DefaultVendor = "zkteco"

var (
	mu      sync.RWMutex
	drivers = map[string]Driver{}
)

// Register makes a driver available under a vendor name. Drivers call it
// from init.
func Register(vendor string, d Driver) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := drivers[vendor]; dup {
		panic("device: driver registered twice: " + vendor)
	}
	drivers[vendor] = d
}

// Lookup returns the driver for vendor ("" = DefaultVendor).
func Lookup(vendor string) (Driver, error) {
	if vendor == "" {
		vendor = DefaultVendor
	}
	mu.RLock()
	defer mu.RUnlock()
	d, ok := drivers[vendor]
	if !ok {
		return nil, fmt.Errorf("unsupported terminal vendor %q (supported: %v)", vendor, vendorsLocked())
	}
	return d, nil
}

// Vendors lists the registered vendor names.
func Vendors() []string {
	mu.RLock()
	defer mu.RUnlock()
	return vendorsLocked()
}

func vendorsLocked() []string {
	out := make([]string, 0, len(drivers))
	for v := range drivers {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// IsNetErr reports whether err means the session is broken and must be
// re-established, whatever the vendor.
func IsNetErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed)
}
