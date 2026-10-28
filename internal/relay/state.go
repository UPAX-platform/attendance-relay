package relay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	Count    int       `json:"count"`
	LastKey  string    `json:"last_key,omitempty"`
	LastTime time.Time `json:"last_time,omitempty"`
	Uploaded int64     `json:"uploaded_total"`
	// Address identifies the UPAX device address the log was uploaded to
	// (addressID), so a device moved to another UPAX record starts over.
	Address      string    `json:"address,omitempty"`
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
//
// UPAX skips a punch it already has (same device, PIN, time and state), so
// whenever the relay can't tell exactly where it left off it re-sends rather
// than guesses: a duplicate costs bandwidth, a skipped punch is lost.
// serverStamp (UPAX's ATTLOGStamp) only says whether UPAX has punches from
// this device at all. It is not used as a log position, because the log may
// have been cleared or rolled since it was set.
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
		case strings.HasPrefix(backfill, "since:"):
			// Skip up to the first record on/after the date. A record after
			// it with an earlier time (the device clock was set back) is
			// still sent: over-sending is safe, skipping is not.
			since, _ := time.ParseInLocation(time.DateOnly, strings.TrimPrefix(backfill, "since:"), time.UTC)
			start = n
			for i, r := range recs {
				if !r.Time.Before(since) {
					start = i
					break
				}
			}
			note = fmt.Sprintf("first run: skipping %d records before %s, uploading %d (backfill: %s)", start, since.Format(time.DateOnly), n-start, backfill)
		case serverStamp > 0:
			// UPAX already has punches from this device but the relay has
			// no record of sending them (a reinstall, or another machine):
			// re-send the log so nothing made in between is missed.
			note = fmt.Sprintf("first run: UPAX already has punches from this device; re-sending all %d stored records (UPAX skips the ones it has)", n)
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
	// The last uploaded record is gone, so everything left is either newer
	// or already in UPAX. Times can't decide which: the clock may have been
	// set back. Send it all.
	st.Count = 0
	return from(0), fmt.Sprintf("device log was cleared or replaced; re-sending all %d records (UPAX skips the ones it has)", n)
}

// addressID identifies a UPAX device address without storing it: the
// address carries the device token, a credential.
func addressID(u string) string {
	sum := sha256.Sum256([]byte(strings.TrimRight(strings.TrimSpace(u), "/")))
	return hex.EncodeToString(sum[:8])
}
