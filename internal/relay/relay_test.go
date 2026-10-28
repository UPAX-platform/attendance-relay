package relay

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UPAX-platform/zk-relay/internal/config"
	"github.com/UPAX-platform/zk-relay/internal/zk"
	"github.com/UPAX-platform/zk-relay/internal/zk/zksim"
)

// fakeUPAX mimics the platform's iclock endpoints.
type fakeUPAX struct {
	mu       sync.Mutex
	lines    []string
	replies  []string
	commands []string
	fail     int // number of ATTLOG posts to reject with 503
	stamp    int
}

func (f *fakeUPAX) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, "/device/test/") {
		http.Error(w, "not found", 404)
		return
	}
	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/iclock/cdata"):
		fmt.Fprintf(w, "GET OPTION FROM: %s\nATTLOGStamp=%d\nDelay=1\nErrorDelay=1\nTransFlag=TransData AttLog\nRealtime=1\n", r.URL.Query().Get("SN"), f.stamp)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/iclock/cdata"):
		if f.fail > 0 {
			f.fail--
			w.Header().Set("Retry-After", "1")
			http.Error(w, "unavailable", 503)
			return
		}
		b, _ := io.ReadAll(r.Body)
		n := 0
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				f.lines = append(f.lines, l)
				n++
			}
		}
		f.stamp, _ = strconv.Atoi(r.URL.Query().Get("Stamp"))
		fmt.Fprintf(w, "OK: %d", n)
	case strings.HasSuffix(r.URL.Path, "/iclock/getrequest"):
		if len(f.commands) == 0 {
			fmt.Fprint(w, "OK")
		}
		for _, c := range f.commands {
			fmt.Fprintln(w, c)
		}
		f.commands = nil
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/iclock/devicecmd"):
		b, _ := io.ReadAll(r.Body)
		f.replies = append(f.replies, string(b))
		fmt.Fprint(w, "OK")
	default:
		http.Error(w, "not found", 404)
	}
}

func (f *fakeUPAX) snapshot() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...), append([]string(nil), f.replies...)
}

func (f *fakeUPAX) queue(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, c)
}

type harness struct {
	t      *testing.T
	dev    *zksim.Device
	upax   *fakeUPAX
	url    string
	host   string
	port   int
	dir    string
	cancel context.CancelFunc
	done   chan struct{}
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, dev: zksim.New("K14SIM"), upax: &fakeUPAX{}, dir: t.TempDir()}
	addr, err := h.dev.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(addr)
	h.host = host
	h.port, _ = strconv.Atoi(port)
	srv := httptest.NewServer(h.upax)
	h.url = srv.URL + "/device/test"
	t.Cleanup(func() { h.stop(); srv.Close(); h.dev.Close() })
	return h
}

func (h *harness) start(backfill string) {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	w := &Worker{
		Device:     config.Device{Host: h.host, Port: h.port},
		URL:        h.url,
		Backfill:   backfill,
		Poll:       50 * time.Millisecond,
		StateDir:   h.dir,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:    "test",
		MinBackoff: 50 * time.Millisecond,
	}
	go func() { w.Run(ctx); close(h.done) }()
}

func (h *harness) stop() {
	if h.cancel != nil {
		h.cancel()
		<-h.done
		h.cancel = nil
	}
}

func (h *harness) waitLines(n int) []string {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		lines, _ := h.upax.snapshot()
		if len(lines) >= n || time.Now().After(deadline) {
			if len(lines) != n {
				h.t.Fatalf("got %d lines, want %d:\n%s", len(lines), n, strings.Join(lines, "\n"))
			}
			return lines
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settle waits long enough for several poll cycles, to catch duplicates.
func settle() { time.Sleep(400 * time.Millisecond) }

var t0 = time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)

func TestBackfillLiveAndRestart(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 3; i++ {
		h.dev.Punch(fmt.Sprint(100+i), t0.Add(time.Duration(i)*time.Hour), 1, 0)
	}
	h.start("all")
	lines := h.waitLines(3)
	if lines[0] != "100\t2026-10-28 08:00:00\t0\t1\t0\t0\t0" {
		t.Fatalf("bad ATTLOG line %q", lines[0])
	}

	h.dev.Punch("200", t0.Add(5*time.Hour), 15, 1)
	lines = h.waitLines(4)
	if lines[3] != "200\t2026-10-28 13:00:00\t1\t15\t0\t0\t0" {
		t.Fatalf("bad live line %q", lines[3])
	}

	// restart: nothing must be re-sent; punches made while down must arrive
	h.stop()
	h.dev.Punch("201", t0.Add(6*time.Hour), 1, 0)
	h.start("all")
	settle()
	h.waitLines(5)
}

func TestBackfillNone(t *testing.T) {
	h := newHarness(t)
	h.dev.Punch("1", t0, 1, 0)
	h.dev.Punch("2", t0.Add(time.Minute), 1, 0)
	h.start("none")
	settle()
	h.waitLines(0)
	h.dev.Punch("3", t0.Add(2*time.Minute), 1, 0)
	if l := h.waitLines(1); !strings.HasPrefix(l[0], "3\t") {
		t.Fatalf("got %q", l[0])
	}
}

func TestUploadRetryAfterOutage(t *testing.T) {
	h := newHarness(t)
	h.upax.fail = 2
	h.dev.Punch("1", t0, 1, 0)
	h.dev.Punch("2", t0.Add(time.Minute), 1, 0)
	h.start("all")
	time.Sleep(2500 * time.Millisecond) // Retry-After: 1 twice
	settle()
	h.waitLines(2)
}

func TestLogClearedAndRolled(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		h.dev.Punch(fmt.Sprint(i), t0.Add(time.Duration(i)*time.Minute), 1, 0)
	}
	h.start("all")
	h.waitLines(5)

	// oldest records roll off while a new one arrives
	h.dev.DropOldest(3)
	h.dev.Punch("5", t0.Add(10*time.Minute), 1, 0)
	h.waitLines(6)

	// log wiped, then new punches
	h.dev.ClearLog()
	settle()
	h.dev.Punch("6", t0.Add(20*time.Minute), 1, 0)
	lines := h.waitLines(7)
	if !strings.HasPrefix(lines[6], "6\t") {
		t.Fatalf("got %q", lines[6])
	}
	settle()
	h.waitLines(7)
}

func TestCommands(t *testing.T) {
	h := newHarness(t)
	h.dev.Punch("1", t0, 1, 0)
	h.dev.Punch("2", t0.Add(time.Hour), 1, 0)
	h.start("all")
	h.waitLines(2)

	h.upax.queue("C:11:INFO")
	h.upax.queue("C:12:DATA QUERY ATTLOG StartTime=2026-10-28 07:00:00\tEndTime=2026-10-28 08:30:00")
	h.upax.queue("C:13:SET OPTIONS Foo=1")
	h.upax.queue("C:14:REBOOT")
	deadline := time.Now().Add(5 * time.Second)
	var replies []string
	for time.Now().Before(deadline) {
		_, replies = h.upax.snapshot()
		if len(replies) >= 4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	want := []string{"ID=11&Return=0&CMD=INFO", "ID=12&Return=0&CMD=DATA", "ID=13&Return=-1002&CMD=SET", "ID=14&Return=0&CMD=REBOOT"}
	if len(replies) != len(want) {
		t.Fatalf("replies: %q", replies)
	}
	for i, w := range want {
		if !strings.HasPrefix(replies[i], w) {
			t.Fatalf("reply %d = %q, want prefix %q", i, replies[i], w)
		}
	}
	if !strings.Contains(replies[0], "~SerialNumber=K14SIM") {
		t.Fatalf("INFO reply missing serial: %q", replies[0])
	}
	h.waitLines(3) // the DATA QUERY re-sent the 08:00 punch
	if h.dev.Restarts() != 1 {
		t.Fatalf("restarts = %d", h.dev.Restarts())
	}
}

func TestPlanResumesFromPlatformStamp(t *testing.T) {
	var recs []zk.Record
	for i := 0; i < 10; i++ {
		recs = append(recs, zk.Record{UserID: fmt.Sprint(i), Time: t0.Add(time.Duration(i) * time.Minute)})
	}
	st := &State{}
	idx, _ := plan(recs, st, 7, "all")
	if len(idx) != 3 || idx[0] != 7 {
		t.Fatalf("idx = %v", idx)
	}
	// a stamp that can't be a record position is ignored
	st = &State{}
	if idx, _ = plan(recs, st, 1761638400, "all"); len(idx) != 10 {
		t.Fatalf("idx = %v", idx)
	}
}
