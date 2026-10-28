package relay

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Health is what the running service last knew about one device, written to
// health-<addr>.json for `status` and `setup` to read. It holds no secrets
// (never the UPAX address, which carries the device token), so it is
// world-readable and `status` works without sudo.
type Health struct {
	Device  string `json:"device"`
	Addr    string `json:"addr"`
	Serial  string `json:"serial,omitempty"`
	Version string `json:"version"`
	// UPAXStatus is the device's state in UPAX: active, pending, paused,
	// unknown (the platform doesn't report it) or rejected (the address
	// is not recognised).
	UPAXStatus    string    `json:"upax_status,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	LastHandshake time.Time `json:"last_handshake,omitzero"`
	LastHeartbeat time.Time `json:"last_heartbeat,omitzero"`
	LastUpload    time.Time `json:"last_upload,omitzero"`
	Uploaded      int64     `json:"uploaded_total"`
	Pending       int       `json:"pending"`
	LastError     string    `json:"last_error,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitzero"`
	UpdatedAt     time.Time `json:"updated_at"`

	mu   sync.Mutex
	path string
}

// HealthPath is the health file for a device address (host:port).
func HealthPath(dir, addr string) string {
	return filepath.Join(dir, "health-"+unsafeChars.ReplaceAllString(addr, "_")+".json")
}

func newHealth(dir, device, addr, version string) *Health {
	return &Health{Device: device, Addr: addr, Version: version, StartedAt: time.Now(), path: HealthPath(dir, addr)}
}

// update applies f and rewrites the file. Write errors are ignored: health
// is advisory and must never stop the relay.
func (h *Health) update(f func(*Health)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
	h.UpdatedAt = time.Now()
	b, _ := json.MarshalIndent(h, "", "  ")
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		_ = os.Rename(tmp, h.path)
	}
}

func (h *Health) fail(err error) {
	h.update(func(h *Health) { h.LastError, h.LastErrorAt = err.Error(), time.Now() })
}

// LoadHealth reads one health file.
func LoadHealth(path string) (*Health, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	h := &Health{path: path}
	return h, json.Unmarshal(b, h)
}

// LoadAllHealth reads every health file in dir, ordered by device address.
func LoadAllHealth(dir string) []*Health {
	files, _ := filepath.Glob(filepath.Join(dir, "health-*.json"))
	sort.Strings(files)
	var out []*Health
	for _, f := range files {
		if h, err := LoadHealth(f); err == nil {
			out = append(out, h)
		}
	}
	return out
}
