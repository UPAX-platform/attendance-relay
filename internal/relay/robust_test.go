package relay

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/device"
)

func TestPlanBadCountResendsAll(t *testing.T) {
	recs := []device.Punch{{PIN: "1", Time: time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)}}
	for _, count := range []int{-5, -1, 7} {
		st := &State{Initialized: true, Count: count, LastKey: "x"}
		if idx, _ := plan(recs, st, "all"); len(idx) != 1 {
			t.Fatalf("count %d: plan %v, want everything", count, idx)
		}
	}
}

func TestCorruptStateSetAside(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{"", "{", `{"count": -3}`, "\x00\x00"} {
		path := StatePath(dir, "SIM0001")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		st, err := loadState(dir, "SIM0001")
		if !errors.Is(err, errCorruptState) || st == nil || !st.Initialized || st.Count != 0 {
			t.Fatalf("%q: state %+v, err %v", body, st, err)
		}
		if _, err := os.Stat(path + ".corrupt"); err != nil {
			t.Fatalf("%q: not set aside: %v", body, err)
		}
		if err := st.save(); err != nil {
			t.Fatal(err)
		}
		if _, err := loadState(dir, "SIM0001"); err != nil {
			t.Fatalf("%q: saved state unreadable: %v", body, err)
		}
	}
}

func TestRecoveredTurnsPanicIntoError(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f := func() (err error) {
		defer recovered(log, "test", &err)
		panic("terminal sent something odd")
	}
	if err := f(); err == nil {
		t.Fatal("panic not turned into an error")
	}
}

func TestDelaysFromUPAXAreCapped(t *testing.T) {
	if got := seconds(1<<62, time.Second); got != maxDelay {
		t.Fatalf("seconds = %v, want %v", got, maxDelay)
	}
	if got := seconds(-1, time.Second); got != time.Second {
		t.Fatalf("seconds = %v", got)
	}
}
