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
	"sync/atomic"
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
	// status is the device's UPAX state; "" omits UPAXStatus (an older
	// platform). Like UPAX, uploads for a device that isn't active get 503.
	status     string
	drop       bool // answer uploads with a bare "OK", as for a dropped table
	handshakes int
	infos      []string
}

func (f *fakeUPAX) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.URL.Path, "/device/") {
		http.Error(w, "not found", 404)
		return
	}
	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/iclock/cdata"):
		f.handshakes++
		f.infos = append(f.infos, r.URL.Query().Get("INFO"))
		fmt.Fprintf(w, "GET OPTION FROM: %s\nATTLOGStamp=%d\nDelay=1\nErrorDelay=1\nTransFlag=TransData AttLog\nRealtime=1\n", r.URL.Query().Get("SN"), f.stamp)
		if r.URL.Query().Get("UPAXRelay") == "1" && f.status != "" {
			fmt.Fprintf(w, "UPAXStatus=%s\n", f.status)
		}
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/iclock/cdata"):
		if f.status != "" && f.status != "active" {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "unavailable", 503)
			return
		}
		if f.drop {
			fmt.Fprint(w, "OK")
			return
		}
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
		if i := r.URL.Query().Get("INFO"); i != "" {
			f.infos = append(f.infos, i)
		}
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

func (f *fakeUPAX) set(fn func(*fakeUPAX)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
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
	base   string
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
	h.base = srv.URL
	h.url = srv.URL + "/device/test"
	t.Cleanup(func() { h.stop(); srv.Close(); h.dev.Close() })
	return h
}

func (h *harness) start(backfill string) { h.startAt(h.url, backfill) }

func (h *harness) startAt(url, backfill string) {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	w := &Worker{
		Device:        config.Device{Host: h.host, Port: h.port},
		URL:           url,
		Backfill:      backfill,
		Poll:          50 * time.Millisecond,
		StateDir:      h.dir,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version:       "test",
		MinBackoff:    50 * time.Millisecond,
		InactiveCheck: 100 * time.Millisecond,
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

func TestBackfillSince(t *testing.T) {
	h := newHarness(t)
	h.dev.Punch("1", time.Date(2025, 12, 31, 18, 0, 0, 0, time.UTC), 1, 0)
	h.dev.Punch("2", time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC), 1, 0)
	h.dev.Punch("3", t0, 1, 0)
	h.start("since:2026-01-01")
	settle()
	lines := h.waitLines(2)
	if !strings.HasPrefix(lines[0], "2\t") || !strings.HasPrefix(lines[1], "3\t") {
		t.Fatalf("got %q", lines)
	}
	h.dev.Punch("4", t0.Add(time.Hour), 1, 0)
	if l := h.waitLines(3); !strings.HasPrefix(l[2], "4\t") {
		t.Fatalf("got %q", l)
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
	h.dev.AddUser("9", "Device Admin", 14)
	h.dev.Punch("1", t0, 1, 0)
	h.start("all")
	h.waitLines(1)

	h.upax.queue("C:11:INFO")
	h.upax.queue("C:12:DATA UPDATE USERINFO PIN=42\tName=Mona Ali\tPri=0")
	h.upax.queue("C:13:DATA UPDATE USERINFO PIN=9\tName=Ahmed Admin\tPri=0")
	h.upax.queue("C:14:DATA DELETE USERINFO PIN=1")
	h.upax.queue("C:15:SET OPTIONS Foo=1")
	h.upax.queue("C:16:DATA QUERY ATTLOG StartTime=2026-10-28 07:00:00\tEndTime=2026-10-28 08:30:00")
	want := []string{"ID=11&Return=0&CMD=INFO", "ID=12&Return=0&CMD=DATA", "ID=13&Return=0&CMD=DATA",
		"ID=14&Return=0&CMD=DATA", "ID=15&Return=-1002&CMD=SET", "ID=16&Return=-1002&CMD=DATA"}
	deadline := time.Now().Add(5 * time.Second)
	var replies []string
	for time.Now().Before(deadline) {
		_, replies = h.upax.snapshot()
		if len(replies) >= len(want) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
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
	if name, pri, ok := h.dev.User("42"); !ok || name != "Mona Ali" || pri != 0 {
		t.Fatalf("user 42 = %q %d %v", name, pri, ok)
	}
	if name, pri, _ := h.dev.User("9"); name != "Ahmed Admin" || pri != 14 {
		t.Fatalf("admin 9 = %q %d (must stay an administrator)", name, pri)
	}
	if _, _, ok := h.dev.User("1"); ok {
		t.Fatal("user 1 was not deleted")
	}
	settle()
	h.waitLines(1) // no command re-sends punches
}

func TestParseUserCommand(t *testing.T) {
	op, f, ok := parseUserCommand("DATA UPDATE USERINFO PIN=7\tName=A B\tPri=14")
	if !ok || op != "UPDATE" || f["PIN"] != "7" || f["Name"] != "A B" || f["Pri"] != "14" {
		t.Fatalf("got %q %v %v", op, f, ok)
	}
	if op, f, ok = parseUserCommand("DATA DELETE USERINFO PIN=7"); !ok || op != "DELETE" || f["PIN"] != "7" {
		t.Fatalf("got %q %v %v", op, f, ok)
	}
	for _, bad := range []string{"DATA UPDATE USERINFO", "DATA UPDATE USERINFOX PIN=1", "DATA QUERY ATTLOG", "DATA UPDATE FINGERTMP PIN=1"} {
		if _, _, ok := parseUserCommand(bad); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestPlanFirstRun(t *testing.T) {
	var recs []zk.Record
	for i := 0; i < 10; i++ {
		recs = append(recs, zk.Record{UserID: fmt.Sprint(i), Time: t0.Add(time.Duration(i) * time.Minute)})
	}
	// UPAX already has punches but the relay has no state: the stamp is not
	// trusted as a position, everything is re-sent (UPAX skips duplicates)
	for _, backfill := range []string{"all", "none"} {
		if idx, _ := plan(recs, &State{}, 7, backfill); len(idx) != 10 {
			t.Fatalf("%s: idx = %v", backfill, idx)
		}
	}
	if idx, _ := plan(recs, &State{}, 0, "none"); len(idx) != 0 {
		t.Fatalf("none: idx = %v", idx)
	}
	if idx, _ := plan(recs, &State{}, 7, "since:2026-10-28"); len(idx) != 10 {
		t.Fatalf("since: idx = %v", idx)
	}
}

func TestPlanLogReplacedResendsAll(t *testing.T) {
	// the device clock was set back after the log was cleared: the new
	// records look older than the last uploaded one and must still be sent
	st := &State{Initialized: true, Count: 5, LastKey: "gone", LastTime: t0}
	recs := []zk.Record{{UserID: "1", Time: t0.Add(-time.Hour)}, {UserID: "2", Time: t0.Add(-30 * time.Minute)}}
	if idx, _ := plan(recs, st, 9, "all"); len(idx) != 2 {
		t.Fatalf("idx = %v", idx)
	}
}

func TestPendingThenActivated(t *testing.T) {
	h := newHarness(t)
	var reads atomic.Int32
	h.dev.BeforeAttLogRead = func() { reads.Add(1) }
	h.upax.status = "pending"
	h.dev.Punch("1", t0, 1, 0)
	h.start("all")
	settle()
	h.waitLines(0)
	if n := reads.Load(); n != 0 {
		t.Fatalf("device log read %d times while the device was pending", n)
	}
	if hs := LoadAllHealth(h.dir); len(hs) != 1 || hs[0].UPAXStatus != "pending" {
		t.Fatalf("health = %+v", hs)
	}
	h.dev.Punch("2", t0.Add(time.Minute), 1, 0)
	h.upax.set(func(f *fakeUPAX) { f.status = "active" })
	h.waitLines(2)

	// paused: the next upload's 503 re-handshakes, and the relay waits
	h.upax.set(func(f *fakeUPAX) { f.status = "paused" })
	h.dev.Punch("3", t0.Add(2*time.Minute), 1, 0)
	time.Sleep(300 * time.Millisecond)
	if hs := LoadAllHealth(h.dir); hs[0].UPAXStatus != "paused" {
		t.Fatalf("health = %+v", hs[0])
	}
	h.upax.set(func(f *fakeUPAX) { f.status = "active" })
	h.waitLines(3)
}

func TestHandshakeCarriesInfo(t *testing.T) {
	h := newHarness(t)
	h.dev.Punch("1", t0, 1, 0)
	h.start("all")
	h.waitLines(1)
	h.dev.Punch("1", t0.Add(time.Minute), 1, 1) // same user: only the record count changes
	h.waitLines(2)
	time.Sleep(1200 * time.Millisecond) // Delay=1s: at least one heartbeat
	h.upax.mu.Lock()
	infos := append([]string(nil), h.upax.infos...)
	h.upax.mu.Unlock()
	if len(infos) == 0 || !strings.HasPrefix(infos[0], "Ver 6.60") {
		t.Fatalf("handshake INFO = %q", infos)
	}
	// a punch changes only the record count: no INFO re-sent on heartbeats
	if len(infos) != 1 {
		t.Fatalf("INFO sent %d times: %q", len(infos), infos)
	}
	hs := LoadAllHealth(h.dir)
	if len(hs) != 1 || hs[0].UPAXStatus != "unknown" || hs[0].LastHeartbeat.IsZero() || hs[0].Uploaded != 2 {
		t.Fatalf("health = %+v", hs)
	}
}

func TestUploadNotAcceptedIsRetried(t *testing.T) {
	h := newHarness(t)
	h.upax.drop = true
	h.dev.Punch("1", t0, 1, 0)
	h.start("all")
	settle()
	h.upax.set(func(f *fakeUPAX) { f.drop = false })
	time.Sleep(1200 * time.Millisecond) // ErrorDelay 1s
	h.waitLines(1)
}

func TestAddressChange(t *testing.T) {
	h := newHarness(t)
	h.dev.Punch("1", t0, 1, 0)
	h.dev.Punch("2", t0.Add(time.Minute), 1, 0)
	h.start("all")
	h.waitLines(2)
	h.stop()

	// same UPAX record under a new token: UPAX has the punches, carry on
	h.startAt(h.base+"/device/rotated", "all")
	settle()
	h.waitLines(2)
	h.stop()

	// a new UPAX record (no punches yet): start over with the backfill
	h.upax.set(func(f *fakeUPAX) { f.stamp = 0 })
	h.startAt(h.base+"/device/new-record", "all")
	h.waitLines(4)
}
