package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Health files are what the running service last knew, written next to the
// config for `status` and `setup` to read. They hold no secrets (no key, no
// comm key), so they are world-readable and `status` works without sudo.

// RelayHealth is health-relay.json.
type RelayHealth struct {
	RelayID     string `json:"relay_id,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Version     string `json:"version"`
	// UPAXStatus is the relay's state in UPAX: active, pending, paused,
	// unpaired (UPAX doesn't know the key), signature_invalid, or "" before
	// the first answer.
	UPAXStatus string    `json:"upax_status,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	LastSync   time.Time `json:"last_sync,omitzero"`
	// ClockOffsetS is UPAX's clock minus this machine's, once measured.
	ClockOffsetS *int      `json:"clock_offset_s,omitempty"`
	Listener     string    `json:"listener,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	LastErrorAt  time.Time `json:"last_error_at,omitzero"`
	UpdatedAt    time.Time `json:"updated_at"`

	mu   sync.Mutex
	path string
}

// TerminalHealth is health-<host>_<port>.json (pulled) or
// health-push-<serial>.json (push).
type TerminalHealth struct {
	Name       string `json:"name"`
	Addr       string `json:"addr"`
	Vendor     string `json:"vendor"`
	Connection string `json:"connection"`
	Serial     string `json:"serial,omitempty"`
	Model      string `json:"model,omitempty"`
	Firmware   string `json:"firmware,omitempty"`
	// Registered: UPAX lists the terminal under this relay. UPAXStatus is
	// its state there (active, pending, paused), or "unregistered".
	Registered  bool       `json:"registered"`
	DeviceID    string     `json:"device_id,omitempty"`
	UPAXStatus  string     `json:"upax_status,omitempty"`
	Reachable   bool       `json:"reachable"`
	ClockSync   bool       `json:"clock_sync"`
	ClockSkewS  *int       `json:"clock_skew_s,omitempty"`
	LastContact time.Time  `json:"last_contact,omitzero"`
	LastUpload  time.Time  `json:"last_upload,omitzero"`
	Uploaded    int64      `json:"uploaded_total"`
	Pending     int        `json:"pending"`
	Spooled     int        `json:"spooled,omitempty"`
	Refused     int64      `json:"refused_total,omitempty"` // punches UPAX refused, set aside since start
	CommKey     string     `json:"comm_key,omitempty"`      // "managed" when the relay set it; never the key
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt time.Time  `json:"last_error_at,omitzero"`
	StartedAt   time.Time  `json:"started_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClockSetAt  *time.Time `json:"clock_set_at,omitempty"`

	mu   sync.Mutex
	path string
}

// RelayHealthPath is the relay's health file.
func RelayHealthPath(dir string) string { return filepath.Join(dir, "health-relay.json") }

// TerminalHealthPath is a pulled terminal's health file (addr = host:port).
func TerminalHealthPath(dir, addr string) string {
	return filepath.Join(dir, "health-"+unsafeChars.ReplaceAllString(addr, "_")+".json")
}

// PushHealthPath is a push terminal's health file.
func PushHealthPath(dir, serial string) string {
	return filepath.Join(dir, "health-push-"+unsafeChars.ReplaceAllString(serial, "_")+".json")
}

func newRelayHealth(dir string) *RelayHealth {
	return &RelayHealth{StartedAt: time.Now(), path: RelayHealthPath(dir)}
}

func newTerminalHealth(path string) *TerminalHealth {
	return &TerminalHealth{StartedAt: time.Now(), path: path}
}

// writeJSON rewrites a health file. Errors are ignored: health is advisory
// and must never stop the relay.
func writeJSON(path string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		_ = os.Rename(tmp, path)
	}
}

func (h *RelayHealth) update(f func(*RelayHealth)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
	h.UpdatedAt = time.Now()
	writeJSON(h.path, h)
}

func (h *RelayHealth) fail(err error) {
	h.update(func(h *RelayHealth) { h.LastError, h.LastErrorAt = err.Error(), time.Now() })
}

func (h *TerminalHealth) update(f func(*TerminalHealth)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
	h.UpdatedAt = time.Now()
	writeJSON(h.path, h)
}

func (h *TerminalHealth) fail(err error) {
	h.update(func(h *TerminalHealth) { h.LastError, h.LastErrorAt = err.Error(), time.Now() })
}

// LoadRelayHealth reads health-relay.json.
func LoadRelayHealth(dir string) (*RelayHealth, error) {
	b, err := os.ReadFile(RelayHealthPath(dir))
	if err != nil {
		return nil, err
	}
	h := &RelayHealth{}
	return h, json.Unmarshal(b, h)
}

// LoadTerminalHealth reads one terminal health file.
func LoadTerminalHealth(path string) (*TerminalHealth, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	h := &TerminalHealth{path: path}
	return h, json.Unmarshal(b, h)
}

// LoadAllTerminalHealth reads every terminal health file in dir: pulled
// terminals first, by address, then push terminals by serial.
func LoadAllTerminalHealth(dir string) []*TerminalHealth {
	files, _ := filepath.Glob(filepath.Join(dir, "health-*.json"))
	sort.Slice(files, func(i, j int) bool {
		pi, pj := strings.Contains(files[i], "health-push-"), strings.Contains(files[j], "health-push-")
		if pi != pj {
			return !pi
		}
		return files[i] < files[j]
	})
	var out []*TerminalHealth
	for _, f := range files {
		if f == RelayHealthPath(dir) {
			continue
		}
		if h, err := LoadTerminalHealth(f); err == nil {
			out = append(out, h)
		}
	}
	return out
}
