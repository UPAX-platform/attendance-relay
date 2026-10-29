package push

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/device"
)

// Spool is a small on-disk queue of punches for one push terminal: one file
// per accepted upload, fsynced before the terminal is told OK, removed once
// UPAX has the punches. It is what makes acknowledging a terminal safe while
// UPAX is unreachable.
type Spool struct {
	dir string

	mu      sync.Mutex
	pending int
	last    int64
}

type spoolPunch struct {
	PIN      string `json:"pin"`
	Local    string `json:"local"`
	State    int    `json:"state"`
	Verify   int    `json:"verify"`
	WorkCode string `json:"workcode"`
}

// OpenSpool opens (creating) the spool in dir.
func OpenSpool(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Spool{dir: dir}
	names, err := s.files()
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		p, err := s.read(n)
		if err != nil {
			return nil, err
		}
		s.pending += len(p)
	}
	return s, nil
}

// Dir is the spool's directory.
func (s *Spool) Dir() string { return s.dir }

// Pending is the number of spooled punches.
func (s *Spool) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

func (s *Spool) files() ([]string, error) {
	m, err := filepath.Glob(filepath.Join(s.dir, "*.punches"))
	if err != nil {
		return nil, err
	}
	sort.Strings(m)
	return m, nil
}

// Append spools punches durably.
func (s *Spool) Append(punches []device.Punch) error {
	if len(punches) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := time.Now().UnixNano()
	if id <= s.last {
		id = s.last + 1
	}
	s.last = id
	name := filepath.Join(s.dir, fmt.Sprintf("%020d.punches", id))
	if err := writeSynced(name, encode(punches)); err != nil {
		return err
	}
	s.pending += len(punches)
	return nil
}

// Oldest returns the oldest spool file and its punches; ok is false when
// the spool is empty.
func (s *Spool) Oldest() (name string, punches []device.Punch, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := s.files()
	if err != nil || len(names) == 0 {
		return "", nil, false, err
	}
	p, err := s.read(names[0])
	return names[0], p, err == nil, err
}

// Done records that the first n punches of file name reached UPAX.
func (s *Spool) Done(name string, n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.read(name)
	if err != nil {
		return err
	}
	n = min(n, len(p))
	s.pending -= n
	if n == len(p) {
		return os.Remove(name)
	}
	return writeSynced(name, encode(p[n:]))
}

func (s *Spool) read(name string) ([]device.Punch, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var in []spoolPunch
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("corrupt spool file %s: %w", name, err)
	}
	out := make([]device.Punch, 0, len(in))
	for _, x := range in {
		t, err := time.Parse(time.DateTime, x.Local)
		if err != nil {
			return nil, fmt.Errorf("corrupt spool file %s: %w", name, err)
		}
		out = append(out, device.Punch{PIN: x.PIN, Time: t, State: x.State, Verify: x.Verify, WorkCode: x.WorkCode})
	}
	return out, nil
}

func encode(punches []device.Punch) []byte {
	out := make([]spoolPunch, len(punches))
	for i, p := range punches {
		out[i] = spoolPunch{PIN: p.PIN, Local: p.Time.Format(time.DateTime), State: p.State, Verify: p.Verify, WorkCode: p.WorkCode}
	}
	b, _ := json.Marshal(out)
	return b
}

// writeSynced writes name atomically and durably: tmp file, fsync, rename,
// fsync the directory.
func writeSynced(name string, b []byte) error {
	tmp := name + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, name); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(name)); err == nil {
		_ = d.Sync() // not supported on every platform (Windows); best effort
		d.Close()
	}
	return nil
}

// SpoolDir is the spool directory for a push terminal's serial.
func SpoolDir(stateDir, serial string) string {
	return filepath.Join(stateDir, "spool-"+strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, serial))
}
