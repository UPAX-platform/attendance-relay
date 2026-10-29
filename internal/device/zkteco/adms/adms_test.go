package adms

import (
	"strings"
	"testing"
	"time"
)

func TestParseAttLog(t *testing.T) {
	got, bad := ParseAttLog("42\t2026-10-28 08:01:12\t0\t1\t0\t0\t0\r\n43\t2026-10-28 08:02:00\t1\t15\t7\n\nbroken line\n44\tnot a time\t0\t1\n")
	if bad != 2 || len(got) != 2 {
		t.Fatalf("got %d punches, %d bad", len(got), bad)
	}
	if got[0].PIN != "42" || !got[0].Time.Equal(time.Date(2026, 10, 28, 8, 1, 12, 0, time.UTC)) || got[0].State != 0 || got[0].Verify != 1 || got[0].WorkCode != "0" {
		t.Fatalf("first %+v", got[0])
	}
	if got[1].State != 1 || got[1].Verify != 15 || got[1].WorkCode != "7" {
		t.Fatalf("second %+v", got[1])
	}
}

func TestZoneOffset(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	for name, want := range map[string]string{"Africa/Cairo": "2", "Asia/Kolkata": "330", "UTC": "0", "America/New_York": "-5"} {
		loc, _ := time.LoadLocation(name)
		if got := zoneOffset(now, loc); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

func TestParseGarbage(t *testing.T) {
	for _, s := range []string{"", "\n\n", "\t\t\t", "1", "1\t", "\t2026-10-28 08:00:00", "x\ty\tz\tw\tv\tu", ",,,,", "\x00\xff\t\x00", strings.Repeat("9\t", 1000)} {
		_, _ = ParseAttLog(s)
		_ = parseInfo(s)
	}
}
