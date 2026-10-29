package push

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/atomicfile"
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
		if errors.Is(err, errCorrupt) {
			s.quarantine(n)
			continue
		}
		if err != nil {
			return nil, err
		}
		s.pending += len(p)
	}
	return s, nil
}

// errCorrupt marks a spool file that can't be read back.
var errCorrupt = errors.New("corrupt spool file")

// quarantine sets an unreadable spool file aside (*.corrupt, kept for
// inspection) so the files after it still go out.
func (s *Spool) quarantine(name string) {
	_ = os.Rename(name, strings.TrimSuffix(name, ".punches")+".corrupt")
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
	if err := atomicfile.Write(name, encode(punches), 0o600); err != nil {
		return err
	}
	s.pending += len(punches)
	return nil
}

// Oldest returns the oldest spool file and its punches; ok is false when
// the spool is empty. Unreadable files are set aside, and empty ones
// removed, on the way.
func (s *Spool) Oldest() (name string, punches []device.Punch, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := s.files()
	if err != nil {
		return "", nil, false, err
	}
	for _, n := range names {
		p, err := s.read(n)
		switch {
		case errors.Is(err, errCorrupt):
			s.quarantine(n)
		case err != nil:
			return "", nil, false, err
		case len(p) == 0:
			_ = os.Remove(n)
		default:
			return n, p, true, nil
		}
	}
	return "", nil, false, nil
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
	return atomicfile.Write(name, encode(p[n:]), 0o600)
}

func (s *Spool) read(name string) ([]device.Punch, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	var in []spoolPunch
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("%w %s: %v", errCorrupt, name, err)
	}
	out := make([]device.Punch, 0, len(in))
	for _, x := range in {
		t, err := time.Parse(time.DateTime, x.Local)
		if err != nil {
			return nil, fmt.Errorf("%w %s: %v", errCorrupt, name, err)
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

// SpoolDir is the spool directory for a push terminal's serial.
func SpoolDir(stateDir, serial string) string {
	return filepath.Join(stateDir, "spool-"+strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' {
			return '_'
		}
		return r
	}, serial))
}
