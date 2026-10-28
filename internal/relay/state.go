package relay

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/UPAX-platform/zk-relay/internal/zk"
)

// State remembers how far each device's log has been uploaded, so restarts
// never re-send or skip punches.
type State struct {
	Serial      string `json:"serial"`
	Initialized bool   `json:"initialized"`
	// Count is the position (1-based) in the device log of the last
	// uploaded record; LastKey/LastTime identify that record so we can
	// detect the log being cleared or rolled over.
	Count        int       `json:"count"`
	LastKey      string    `json:"last_key,omitempty"`
	LastTime     time.Time `json:"last_time,omitempty"`
	Uploaded     int64     `json:"uploaded_total"`
	LastUploadAt time.Time `json:"last_upload_at,omitempty"`

	path string
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// StatePath is the state file for a serial number.
func StatePath(dir, serial string) string {
	return filepath.Join(dir, "state-"+unsafeChars.ReplaceAllString(serial, "_")+".json")
}

func loadState(dir, serial string) (*State, error) {
	s := &State{Serial: serial, path: StatePath(dir, serial)}
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("corrupt state file %s: %w", s.path, err)
	}
	return s, nil
}

// LoadStateFile reads a state file for display.
func LoadStateFile(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := &State{path: path}
	return s, json.Unmarshal(b, s)
}

func (s *State) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *State) markUploaded(pos int, r zk.Record) {
	s.Count = pos
	s.LastKey = recordKey(r)
	s.LastTime = r.Time
}

func recordKey(r zk.Record) string {
	return fmt.Sprintf("%s|%s|%d|%d", r.UserID, r.Time.Format(time.DateTime), r.Verify, r.Punch)
}

// plan decides which records (by index) still need uploading.
func plan(recs []zk.Record, st *State, serverStamp int64, backfill string) (idx []int, note string) {
	n := len(recs)
	from := func(start int) []int {
		out := make([]int, 0, n-start)
		for i := start; i < n; i++ {
			out = append(out, i)
		}
		return out
	}

	if !st.Initialized {
		st.Initialized = true
		start := 0
		switch {
		case serverStamp > 0 && serverStamp <= int64(n):
			start = int(serverStamp)
			note = fmt.Sprintf("first run: platform already has %d records, resuming after them", start)
		case backfill == "none":
			start = n
			note = fmt.Sprintf("first run: skipping %d historic records (backfill: none)", n)
		default:
			note = fmt.Sprintf("first run: uploading all %d stored records (backfill: all)", n)
		}
		if start > 0 {
			st.markUploaded(start, recs[start-1])
		}
		return from(start), note
	}

	if st.Count == 0 {
		return from(0), ""
	}
	if st.Count <= n && recordKey(recs[st.Count-1]) == st.LastKey {
		return from(st.Count), ""
	}
	// The log changed underneath us (cleared, or oldest entries rolled off).
	for i := n - 1; i >= 0; i-- {
		if recordKey(recs[i]) == st.LastKey {
			st.Count = i + 1
			return from(i + 1), fmt.Sprintf("device log shifted; last uploaded record now at position %d", i+1)
		}
	}
	for i, r := range recs {
		if r.Time.After(st.LastTime) {
			idx = append(idx, i)
		}
	}
	return idx, fmt.Sprintf("device log was cleared or replaced; uploading %d records newer than %s", len(idx), st.LastTime.Format(time.DateTime))
}
