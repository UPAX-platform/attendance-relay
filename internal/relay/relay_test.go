package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/config"
	"github.com/UPAX-platform/attendance-relay/internal/device"
	_ "github.com/UPAX-platform/attendance-relay/internal/device/drivers"
	"github.com/UPAX-platform/attendance-relay/internal/device/zkteco/zksim"
	"github.com/UPAX-platform/attendance-relay/internal/identity"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
	"github.com/UPAX-platform/attendance-relay/internal/upax/upaxtest"
)

const serial = "K14SIM"

type harness struct {
	t      *testing.T
	dev    *zksim.Device
	upax   *upaxtest.Fake
	cfg    *config.Config
	key    *identity.Key
	api    *upax.Client
	relay  *Relay
	ln     net.Listener // push listener, when enabled
	cancel context.CancelFunc
	done   chan struct{}
}

// newHarness pairs a relay with the fake UPAX and gives it one simulated
// terminal. The relay and terminal start active unless the test says not.
func newHarness(t *testing.T) *harness {
	h := &harness{t: t, dev: zksim.New(serial), upax: upaxtest.New()}
	addr, err := h.dev.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	dir := t.TempDir()
	h.cfg = config.New(filepath.Join(dir, "config.yaml"))
	h.cfg.UPAX = h.upax.URL
	h.cfg.RelayID = h.upax.RelayID
	h.cfg.PollInterval = config.Duration(50 * time.Millisecond)
	h.cfg.Terminals = []config.Terminal{{Name: "Front door", Host: host, Port: p}}
	if err := h.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	h.key, _ = identity.Generate()
	if err := h.key.Save(h.cfg.KeyPath()); err != nil {
		t.Fatal(err)
	}
	h.upax.Pair(h.key, upax.StatusActive)
	h.upax.Register(serial, upaxtest.Terminal{})
	t.Cleanup(func() { h.stop(); h.upax.Close(); h.dev.Close() })
	return h
}

func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	keys := &identity.Source{Path: h.cfg.KeyPath()}
	h.api = &upax.Client{Base: h.cfg.UPAX, RelayID: h.cfg.RelayID, Keys: keys}
	o := Options{
		Config: h.cfg, API: h.api, Keys: keys, Version: "test", HostName: "test-pc",
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MinBackoff: 50 * time.Millisecond, MaxBackoff: 200 * time.Millisecond,
		UnpairedRetry: 100 * time.Millisecond, SignatureBackoff: 100 * time.Millisecond,
		SyncEvery: 100 * time.Millisecond, ErrorDelay: 100 * time.Millisecond,
	}
	if h.cfg.Listener.Enabled {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			h.t.Fatal(err)
		}
		h.ln, o.PushListener = ln, ln
	}
	h.relay = New(o)
	go func() { _ = h.relay.Run(ctx); close(h.done) }()
}

func (h *harness) stop() {
	if h.cancel != nil {
		h.cancel()
		<-h.done
		h.cancel = nil
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *harness) waitPunches(n int) []upax.Punch {
	h.t.Helper()
	eventually(h.t, fmt.Sprintf("%d punches", n), func() bool { return h.upax.PunchCount(serial) >= n })
	settle()
	got := h.upax.GotPunches(serial)
	if len(got) != n {
		h.t.Fatalf("UPAX has %d punches, want %d: %+v", len(got), n, got)
	}
	return got
}

// settle waits long enough for several poll and sync cycles.
func settle() { time.Sleep(400 * time.Millisecond) }

var t0 = time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC)

func TestBackfillLiveAndRestart(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 3; i++ {
		h.dev.Punch(fmt.Sprint(100+i), t0.Add(time.Duration(i)*time.Hour), 1, 0)
	}
	h.start()
	got := h.waitPunches(3)
	if want := (upax.Punch{PIN: "100", Local: "2026-10-28 08:00:00", State: 0, Verify: 1, WorkCode: "0"}); got[0] != want {
		t.Fatalf("punch %+v, want %+v", got[0], want)
	}
	h.dev.Punch("200", t0.Add(5*time.Hour), 15, 1)
	got = h.waitPunches(4)
	if want := (upax.Punch{PIN: "200", Local: "2026-10-28 13:00:00", State: 1, Verify: 15, WorkCode: "0"}); got[3] != want {
		t.Fatalf("live punch %+v", got[3])
	}

	// Restart: nothing is sent again, and a punch made while down arrives.
	h.stop()
	h.dev.Punch("201", t0.Add(6*time.Hour), 1, 0)
	h.start()
	h.waitPunches(5)
	if n := h.upax.ReceivedCount(serial); n != 5 {
		t.Fatalf("UPAX received %d punches in all, want 5 (no re-sends)", n)
	}
}

func TestBackfillNoneAndSince(t *testing.T) {
	h := newHarness(t)
	h.cfg.Backfill = "since:2026-01-01"
	h.dev.Punch("1", time.Date(2025, 12, 31, 18, 0, 0, 0, time.UTC), 1, 0)
	h.dev.Punch("2", time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC), 1, 0)
	h.start()
	if got := h.waitPunches(1); got[0].PIN != "2" {
		t.Fatalf("got %+v", got)
	}

	h2 := newHarness(t)
	h2.cfg.Backfill = "none"
	h2.dev.Punch("1", t0, 1, 0)
	h2.start()
	settle()
	h2.waitPunches(0)
	h2.dev.Punch("3", t0.Add(time.Minute), 1, 0)
	if got := h2.waitPunches(1); got[0].PIN != "3" {
		t.Fatalf("got %+v", got)
	}
}

func TestLogClearedAndRolled(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 5; i++ {
		h.dev.Punch(fmt.Sprint(i), t0.Add(time.Duration(i)*time.Minute), 1, 0)
	}
	h.start()
	h.waitPunches(5)
	h.dev.DropOldest(3)
	h.dev.Punch("5", t0.Add(10*time.Minute), 1, 0)
	h.waitPunches(6)
	h.dev.ClearLog()
	settle()
	h.dev.Punch("6", t0.Add(20*time.Minute), 1, 0)
	h.waitPunches(7)
}

func TestPendingRelayThenActivated(t *testing.T) {
	h := newHarness(t)
	h.upax.Do(func(f *upaxtest.Fake) { f.Status = upax.StatusPending })
	h.dev.Punch("1", t0, 1, 0)
	h.start()
	eventually(t, "relay health pending", func() bool {
		rh, err := LoadRelayHealth(h.cfg.StateDir())
		return err == nil && rh.UPAXStatus == upax.StatusPending && rh.Fingerprint == h.key.Fingerprint()
	})
	settle()
	if n := h.upax.PunchCount(serial); n != 0 {
		t.Fatalf("pending relay uploaded %d punches", n)
	}
	// While pending, the relay still syncs (so UPAX sees it).
	if s, ok := h.upax.LastSync(); !ok || s.HostName != "test-pc" {
		t.Fatalf("no sync while pending: %+v", s)
	}
	h.upax.Do(func(f *upaxtest.Fake) { f.Status = upax.StatusActive })
	h.waitPunches(1)
}

func TestUnregisteredTerminalThenAdded(t *testing.T) {
	h := newHarness(t)
	h.upax.Do(func(f *upaxtest.Fake) { delete(f.Terminals, serial) })
	h.dev.Punch("1", t0, 1, 0)
	h.start()
	eventually(t, "terminal reported", func() bool {
		s, ok := h.upax.LastSync()
		return ok && len(s.Terminals) == 1 && s.Terminals[0].Serial == serial && s.Terminals[0].Reachable &&
			s.Terminals[0].Records == 1 && s.Terminals[0].Connection == upax.ConnPull && s.Terminals[0].Vendor == "zkteco"
	})
	eventually(t, "health unregistered", func() bool {
		th, err := LoadTerminalHealth(TerminalHealthPath(h.cfg.StateDir(), h.cfg.Terminals[0].Addr()))
		return err == nil && th.UPAXStatus == "unregistered" && th.Serial == serial
	})
	if n := h.upax.PunchCount(serial); n != 0 {
		t.Fatalf("unregistered terminal uploaded %d punches", n)
	}
	h.upax.Register(serial, upaxtest.Terminal{})
	h.waitPunches(1)
}

func TestTerminalNotActiveHolds(t *testing.T) {
	h := newHarness(t)
	h.upax.Register(serial, upaxtest.Terminal{Status: upax.StatusPaused})
	h.dev.Punch("1", t0, 1, 0)
	h.start()
	settle()
	if n := h.upax.ReceivedCount(serial); n != 0 {
		t.Fatalf("paused terminal sent %d punches", n)
	}
	h.upax.Register(serial, upaxtest.Terminal{})
	h.waitPunches(1)
}

func TestUploadRefusedThenRetried(t *testing.T) {
	h := newHarness(t)
	h.upax.Do(func(f *upaxtest.Fake) { f.FailPunches = 2 })
	h.dev.Punch("1", t0, 1, 0)
	h.dev.Punch("2", t0.Add(time.Minute), 1, 0)
	h.start()
	time.Sleep(2500 * time.Millisecond) // Retry-After: 1, twice
	h.waitPunches(2)
}

func TestBatchHalvedOn413(t *testing.T) {
	h := newHarness(t)
	h.upax.Do(func(f *upaxtest.Fake) { f.MaxBatch = 3 })
	for i := 0; i < 10; i++ {
		h.dev.Punch(fmt.Sprint(i), t0.Add(time.Duration(i)*time.Minute), 1, 0)
	}
	h.start()
	h.waitPunches(10)
	h.upax.Do(func(f *upaxtest.Fake) {
		for _, b := range f.Batches {
			if b > 3 {
				t.Errorf("accepted a batch of %d", b)
			}
		}
	})
}

func TestRefusedPunchSetAside(t *testing.T) {
	h := newHarness(t)
	h.upax.Do(func(f *upaxtest.Fake) { f.RefusePINs = map[string]bool{"bad": true} })
	for i := 0; i < 9; i++ {
		pin := fmt.Sprint(i)
		if i == 4 {
			pin = "bad"
		}
		h.dev.Punch(pin, t0.Add(time.Duration(i)*time.Minute), 1, 0)
	}
	h.start()
	h.waitPunches(8)
	// It isn't retried, and punches after it keep flowing.
	h.dev.Punch("9", t0.Add(time.Hour), 1, 0)
	h.waitPunches(9)
	eventually(t, "refused punch counted", func() bool {
		th, err := LoadTerminalHealth(TerminalHealthPath(h.cfg.StateDir(), h.cfg.Terminals[0].Addr()))
		return err == nil && th.Refused == 1
	})
}

func TestRefusalsInARowHoldUploads(t *testing.T) {
	h := newHarness(t)
	refuse := map[string]bool{}
	for i := 0; i < maxRefusedInARow+3; i++ {
		refuse[fmt.Sprint(i)] = true
		h.dev.Punch(fmt.Sprint(i), t0.Add(time.Duration(i)*time.Minute), 1, 0)
	}
	h.upax.Do(func(f *upaxtest.Fake) { f.RefusePINs = refuse })
	h.start()
	eventually(t, "refusals capped", func() bool {
		th, err := LoadTerminalHealth(TerminalHealthPath(h.cfg.StateDir(), h.cfg.Terminals[0].Addr()))
		return err == nil && th.Refused == maxRefusedInARow && th.Pending == 3
	})
	time.Sleep(500 * time.Millisecond)
	th, _ := LoadTerminalHealth(TerminalHealthPath(h.cfg.StateDir(), h.cfg.Terminals[0].Addr()))
	if th.Refused != maxRefusedInARow {
		t.Fatalf("set aside %d punches, want at most %d", th.Refused, maxRefusedInARow)
	}
}

func TestUnpaired(t *testing.T) {
	h := newHarness(t)
	h.upax.Do(func(f *upaxtest.Fake) { f.Pub = nil })
	h.dev.Punch("1", t0, 1, 0)
	h.start()
	eventually(t, "relay health unpaired", func() bool {
		rh, err := LoadRelayHealth(h.cfg.StateDir())
		return err == nil && rh.UPAXStatus == statusUnpaired
	})
	// Pairing again (UPAX knows the key again) resumes.
	h.upax.Pair(h.key, upax.StatusActive)
	h.waitPunches(1)
}

func TestRelayClockOffCorrected(t *testing.T) {
	h := newHarness(t)
	h.upax.Do(func(f *upaxtest.Fake) { f.Clock = 10 * time.Minute })
	h.dev.Punch("1", t0, 1, 0)
	h.start()
	h.waitPunches(1)
}

func TestCommands(t *testing.T) {
	h := newHarness(t)
	h.dev.AddUser("9", "Device Admin", 14)
	h.dev.Punch("1", t0, 1, 0)
	h.start()
	h.waitPunches(1)

	for i, line := range []string{
		"INFO",
		"DATA UPDATE USERINFO PIN=42\tName=Mona Ali\tPri=0",
		"DATA UPDATE USERINFO PIN=9\tName=Ahmed Admin\tPri=0",
		"DATA DELETE USERINFO PIN=1",
		"SET OPTIONS Foo=1",
	} {
		h.upax.Queue(serial, upax.Command{Seq: int64(11 + i), Line: line})
	}
	want := []upax.Result{
		{Serial: serial, Seq: 11, Return: 0, Cmd: "INFO"},
		{Serial: serial, Seq: 12, Return: 0, Cmd: "DATA"},
		{Serial: serial, Seq: 13, Return: 0, Cmd: "DATA"},
		{Serial: serial, Seq: 14, Return: 0, Cmd: "DATA"},
		{Serial: serial, Seq: 15, Return: upax.ReturnUnsupported, Cmd: "SET"},
	}
	var got []upax.Result
	eventually(t, "command results", func() bool {
		h.upax.Do(func(f *upaxtest.Fake) { got = append([]upax.Result(nil), f.Results...) })
		return len(got) >= len(want)
	})
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if name, pri, ok := h.dev.User("42"); !ok || name != "Mona Ali" || pri != 0 {
		t.Errorf("user 42: %q %d %v", name, pri, ok)
	}
	if name, pri, _ := h.dev.User("9"); name != "Ahmed Admin" || pri != 14 {
		t.Errorf("admin demoted or not renamed: %q %d", name, pri)
	}
	if _, _, ok := h.dev.User("1"); ok {
		t.Error("user 1 not deleted")
	}
}

func TestParseUserCommand(t *testing.T) {
	op, f, ok := parseUserCommand("DATA UPDATE USERINFO PIN=42\tName=Mona Ali\tPri=0")
	if !ok || op != "UPDATE" || f["PIN"] != "42" || f["Name"] != "Mona Ali" {
		t.Fatalf("%s %v %v", op, f, ok)
	}
	if _, _, ok := parseUserCommand("DATA UPDATE USERINFOX PIN=1"); ok {
		t.Fatal("accepted a different table")
	}
	if _, _, ok := parseUserCommand("DATA DELETE USERINFO"); ok {
		t.Fatal("accepted a delete without PIN")
	}
}

func TestDeviceRecordChangedStartsOver(t *testing.T) {
	h := newHarness(t)
	h.dev.Punch("1", t0, 1, 0)
	h.dev.Punch("2", t0.Add(time.Minute), 1, 0)
	h.start()
	h.waitPunches(2)
	// The terminal is retired in UPAX and added again: a new device record.
	h.upax.Do(func(f *upaxtest.Fake) {
		f.Terminals[serial].DeviceID = "dev-new"
		f.Punches[serial] = nil
	})
	h.waitPunches(2)
	if n := h.upax.ReceivedCount(serial); n != 4 {
		t.Fatalf("received %d, want the log sent again (4)", n)
	}
}

func TestClockSync(t *testing.T) {
	h := newHarness(t)
	h.dev.SetClock(wall(time.Now()).Add(-5 * time.Minute))
	h.upax.Register(serial, upaxtest.Terminal{Timezone: "Africa/Cairo", ClockSync: true})
	h.start()
	cairo, _ := time.LoadLocation("Africa/Cairo")
	eventually(t, "terminal clock set to Cairo time", func() bool {
		return abs(h.dev.Clock().Sub(wall(time.Now().In(cairo)))) < 3*time.Second
	})
	eventually(t, "skew reported", func() bool {
		s, ok := h.upax.LastSync()
		return ok && len(s.Terminals) == 1 && s.Terminals[0].ClockSkewS != nil && abs(time.Duration(*s.Terminals[0].ClockSkewS)*time.Second) < 3*time.Second
	})
}

func TestClockSyncOffOnlyMeasures(t *testing.T) {
	h := newHarness(t)
	off := wall(time.Now()).Add(-5 * time.Minute)
	h.dev.SetClock(off)
	h.upax.Register(serial, upaxtest.Terminal{Timezone: "UTC"})
	h.start()
	eventually(t, "skew reported", func() bool {
		s, ok := h.upax.LastSync()
		return ok && len(s.Terminals) == 1 && s.Terminals[0].ClockSkewS != nil
	})
	settle()
	if d := h.dev.Clock().Sub(wall(time.Now().UTC())); d > -4*time.Minute {
		t.Fatalf("clock changed without clock_sync: off by %v", d)
	}
}

func TestClockSyncSkippedWhenRelayClockOff(t *testing.T) {
	h := newHarness(t)
	h.upax.Do(func(f *upaxtest.Fake) { f.Clock = 2 * time.Minute })
	h.dev.SetClock(wall(time.Now().UTC()).Add(-5 * time.Minute))
	h.upax.Register(serial, upaxtest.Terminal{Timezone: "UTC", ClockSync: true})
	h.start()
	eventually(t, "a sync", func() bool { _, ok := h.upax.LastSync(); return ok })
	settle()
	if d := h.dev.Clock().Sub(wall(time.Now().UTC())); d > -4*time.Minute {
		t.Fatalf("terminal clock set although the relay's clock is off: off by %v", d)
	}
}

func TestManagedCommKey(t *testing.T) {
	h := newHarness(t)
	h.cfg.ManageCommKeys = true
	h.dev.Punch("1", t0, 1, 0)
	h.start()
	h.waitPunches(1)
	k := h.dev.CommKey()
	if k == 0 || k > 999999 {
		t.Fatalf("terminal comm key %d", k)
	}
	saved, err := config.Load(h.cfg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if saved.Terminals[0].Password != k {
		t.Fatalf("config has %d, terminal %d", saved.Terminals[0].Password, k)
	}
	// The relay keeps working with the new key.
	h.dev.Punch("2", t0.Add(time.Minute), 1, 0)
	h.waitPunches(2)
	b, _ := os.ReadFile(TerminalHealthPath(h.cfg.StateDir(), h.cfg.Terminals[0].Addr()))
	if strings.Contains(string(b), strconv.Itoa(int(k))) {
		t.Fatal("health file contains the comm key")
	}
}

func TestManagedCommKeyReplyLost(t *testing.T) {
	h := newHarness(t)
	h.cfg.ManageCommKeys = true
	h.dev.DropRefreshReply = true // the terminal takes the key, the reply never comes
	h.dev.Punch("1", t0, 1, 0)
	h.start()
	h.waitPunches(1)
	k := h.dev.CommKey()
	if k == 0 {
		t.Fatal("terminal has no comm key")
	}
	if saved, _ := config.Load(h.cfg.Path()); saved.Terminals[0].Password != k {
		t.Fatalf("config has %d, terminal %d: the relay lost the key", saved.Terminals[0].Password, k)
	}
}

func TestKeyRotation(t *testing.T) {
	h := newHarness(t)
	h.dev.Punch("1", t0, 1, 0)
	h.start()
	h.waitPunches(1)
	next, err := RotateKey(context.Background(), h.api, h.cfg.KeyPath(), upax.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.cfg.KeyPath() + ".prev"); err != nil {
		t.Fatal("old key not kept")
	}
	// Drop the grace key in the fake: the running relay must have picked
	// up the new key from disk.
	h.upax.Do(func(f *upaxtest.Fake) { f.PrevPub = nil })
	h.dev.Punch("2", t0.Add(time.Minute), 1, 0)
	h.waitPunches(2)
	if k, _ := h.relay.o.Keys.Key(); k.Fingerprint() != next.Fingerprint() {
		t.Fatal("relay still signs with the old key")
	}
}

func TestInterruptedRotationRecovered(t *testing.T) {
	h := newHarness(t)
	// UPAX took the new key, but the relay stopped before swapping files.
	next, _ := identity.Generate()
	_ = next.Save(h.cfg.KeyPath() + ".next")
	h.upax.Do(func(f *upaxtest.Fake) { f.PrevPub, f.Pub = f.Pub, next.Public() })
	h.dev.Punch("1", t0, 1, 0)
	h.start()
	h.waitPunches(1)
	k, _ := identity.Load(h.cfg.KeyPath())
	if k.Fingerprint() != next.Fingerprint() {
		t.Fatal("rotation not finished")
	}
}

// loseKeyReply delivers requests but loses UPAX's answer to a key upload,
// as when the connection drops after UPAX stored the new key.
type loseKeyReply struct{}

func (loseKeyReply) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err == nil && strings.HasSuffix(req.URL.Path, "/key") {
		resp.Body.Close()
		return nil, errors.New("connection reset")
	}
	return resp, err
}

func TestRotationAnswerLost(t *testing.T) {
	h := newHarness(t)
	path := h.cfg.KeyPath()
	api := &upax.Client{Base: h.cfg.UPAX, RelayID: h.cfg.RelayID, Keys: &identity.Source{Path: path}}
	lossy := &upax.Client{Base: api.Base, RelayID: api.RelayID, Keys: api.Keys, HTTP: &http.Client{Transport: loseKeyReply{}}}
	if _, err := RotateKey(context.Background(), lossy, path, upax.SyncRequest{}); err == nil {
		t.Fatal("no error")
	}
	pending, err := identity.Load(path + ".next")
	if err != nil {
		t.Fatal("pending key dropped although UPAX may have it")
	}
	// The next try settles the pending key instead of making another one.
	next, err := RotateKey(context.Background(), api, path, upax.SyncRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var rotated int
	h.upax.Do(func(f *upaxtest.Fake) { rotated = f.Rotated })
	if next.Fingerprint() != pending.Fingerprint() || rotated != 1 {
		t.Fatalf("installed %s, pending was %s; UPAX rotated %d times", next.Fingerprint(), pending.Fingerprint(), rotated)
	}
	if k, _ := identity.Load(path); k.Fingerprint() != pending.Fingerprint() {
		t.Fatal("pending key not installed")
	}
}

func TestRotationRefusedDiscardsPendingKey(t *testing.T) {
	h := newHarness(t)
	path := h.cfg.KeyPath()
	api := &upax.Client{Base: h.cfg.UPAX, RelayID: h.cfg.RelayID, Keys: &identity.Source{Path: path}}
	h.upax.Do(func(f *upaxtest.Fake) { f.RefuseStatus, f.RefuseCode = http.StatusConflict, upax.CodeNotActive })
	if _, err := RotateKey(context.Background(), api, path, upax.SyncRequest{}); err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(path + ".next"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused key kept")
	}
}

func TestHealthHoldsNoSecrets(t *testing.T) {
	h := newHarness(t)
	h.dev.Punch("1", t0, 1, 0)
	h.start()
	h.waitPunches(1)
	seed, _ := os.ReadFile(h.cfg.KeyPath())
	files, _ := filepath.Glob(filepath.Join(h.cfg.StateDir(), "health-*.json"))
	if len(files) != 2 {
		t.Fatalf("health files %v", files)
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), strings.TrimSpace(string(seed))) {
			t.Fatalf("%s contains the key", f)
		}
		// Windows has no mode bits; scripts/windows-check.ps1 checks the ACLs.
		if fi, _ := os.Stat(f); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o644 {
			t.Fatalf("%s mode %v", f, fi.Mode().Perm())
		}
	}
}

func TestPlanFirstRun(t *testing.T) {
	recs := []device.Punch{{PIN: "1", Time: t0}, {PIN: "2", Time: t0.Add(time.Hour)}}
	for backfill, want := range map[string]int{"all": 2, "none": 0, "since:2026-10-28": 2, "since:2026-10-29": 0} {
		st := &State{}
		if idx, _ := plan(recs, st, backfill); len(idx) != want {
			t.Errorf("%s: %d to upload, want %d", backfill, len(idx), want)
		}
	}
}

func TestPlanLogReplacedResendsAll(t *testing.T) {
	st := &State{Initialized: true}
	st.markUploaded(2, device.Punch{PIN: "x", Time: t0})
	recs := []device.Punch{{PIN: "a", Time: t0}, {PIN: "b", Time: t0}}
	if idx, _ := plan(recs, st, "all"); len(idx) != 2 {
		t.Fatalf("got %v, want everything re-sent", idx)
	}
}

// ---- push terminals ------------------------------------------------------------------

func (h *harness) iclock(method, path, body string) (int, string, http.Header) {
	h.t.Helper()
	req, _ := http.NewRequest(method, "http://"+h.ln.Addr().String()+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func pushHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.cfg.Terminals = nil
	h.cfg.Listener.Enabled = true
	h.start()
	return h
}

const pushSerial = "CQZ7232560123"

func TestPushUnregisteredHeld(t *testing.T) {
	h := pushHarness(t)
	code, body, _ := h.iclock("GET", "/iclock/cdata?SN="+pushSerial+"&options=all&pushver=2.4.1", "")
	if code != 200 || !strings.HasPrefix(body, "GET OPTION FROM: "+pushSerial+"\n") || !strings.Contains(body, "TransFlag=TransData AttLog") || strings.Contains(body, "TimeZone=") {
		t.Fatalf("handshake %d:\n%s", code, body)
	}
	eventually(t, "push terminal reported as a claim", func() bool {
		s, ok := h.upax.LastSync()
		return ok && len(s.Terminals) == 1 && s.Terminals[0].Serial == pushSerial && s.Terminals[0].Connection == upax.ConnPush && s.Terminals[0].Reachable
	})
	code, _, hdr := h.iclock("POST", "/iclock/cdata?SN="+pushSerial+"&table=ATTLOG&Stamp=1", "7\t2026-10-28 08:00:00\t0\t1\t0\t0\t0\n")
	if code != 503 || hdr.Get("Retry-After") == "" {
		t.Fatalf("unregistered upload: %d (Retry-After %q), want 503", code, hdr.Get("Retry-After"))
	}
	if p := h.relay.knownPush(pushSerial); p.spool.Pending() != 0 {
		t.Fatal("unregistered punches spooled")
	}
}

func TestPushSpoolUploadAndCommands(t *testing.T) {
	h := pushHarness(t)
	h.upax.Register(pushSerial, upaxtest.Terminal{Timezone: "Asia/Kolkata", ClockSync: true})
	h.iclock("GET", "/iclock/cdata?SN="+pushSerial+"&options=all", "")
	eventually(t, "registered", func() bool {
		ok, _ := h.relay.Accept(pushSerial)
		return ok
	})
	_, body, hdr := h.iclock("GET", "/iclock/cdata?SN="+pushSerial+"&options=all", "")
	if !strings.Contains(body, "TimeZone=330\n") || hdr.Get("Date") == "" {
		t.Fatalf("clock-synced handshake:\n%s", body)
	}

	// Biometric tables are acknowledged and dropped.
	if code, body, _ := h.iclock("POST", "/iclock/cdata?SN="+pushSerial+"&table=BIODATA", "Pin=7\tTmp=SECRETTEMPLATE"); code != 200 || body != "OK" {
		t.Fatalf("biodata: %d %q", code, body)
	}
	if code, body, _ := h.iclock("POST", "/iclock/fdata?SN="+pushSerial+"&table=ATTPHOTO", "binary"); code != 200 || body != "OK" {
		t.Fatalf("fdata: %d %q", code, body)
	}

	// Take UPAX offline: the punches are spooled and acknowledged anyway.
	h.upax.Do(func(f *upaxtest.Fake) { f.FailPunches = 1000 })
	code, body, _ := h.iclock("POST", "/iclock/cdata?SN="+pushSerial+"&table=ATTLOG&Stamp=42",
		"7\t2026-10-28 08:00:00\t0\t1\t0\t0\t0\n8\t2026-10-28 08:01:00\t1\t15\t5\t0\t0\n")
	if code != 200 || body != "OK: 2" {
		t.Fatalf("attlog: %d %q", code, body)
	}
	p := h.relay.knownPush(pushSerial)
	if p.spool.Pending() != 2 {
		t.Fatalf("spooled %d, want 2", p.spool.Pending())
	}
	if _, body, _ := h.iclock("GET", "/iclock/cdata?SN="+pushSerial+"&options=all", ""); !strings.Contains(body, "ATTLOGStamp=42\n") {
		t.Fatalf("stamp not kept:\n%s", body)
	}
	h.upax.Do(func(f *upaxtest.Fake) { f.FailPunches = 0 })
	eventually(t, "spool drained", func() bool { return h.upax.PunchCount(pushSerial) == 2 && p.spool.Pending() == 0 })
	if got := h.upax.GotPunches(pushSerial); got[1] != (upax.Punch{PIN: "8", Local: "2026-10-28 08:01:00", State: 1, Verify: 15, WorkCode: "5"}) {
		t.Fatalf("punch %+v", got[1])
	}

	// Commands are handed out verbatim; results go back up.
	h.upax.Queue(pushSerial, upax.Command{Seq: 5, Line: "DATA UPDATE USERINFO PIN=7\tName=Ali\tPri=0"})
	var cmds string
	eventually(t, "command handed out", func() bool {
		_, cmds, _ = h.iclock("GET", "/iclock/getrequest?SN="+pushSerial, "")
		return cmds != "OK"
	})
	if cmds != "C:5:DATA UPDATE USERINFO PIN=7\tName=Ali\tPri=0\n" {
		t.Fatalf("getrequest %q", cmds)
	}
	h.iclock("POST", "/iclock/devicecmd?SN="+pushSerial, "ID=5&Return=0&CMD=DATA\n")
	eventually(t, "result forwarded", func() bool {
		var n int
		h.upax.Do(func(f *upaxtest.Fake) { n = len(f.Results) })
		return n == 1
	})
}

func TestPushSpoolSurvivesRestart(t *testing.T) {
	h := pushHarness(t)
	h.upax.Register(pushSerial, upaxtest.Terminal{})
	h.iclock("GET", "/iclock/cdata?SN="+pushSerial+"&options=all", "")
	eventually(t, "registered", func() bool { ok, _ := h.relay.Accept(pushSerial); return ok })
	h.upax.Do(func(f *upaxtest.Fake) { f.FailPunches = 1000 })
	h.iclock("POST", "/iclock/cdata?SN="+pushSerial+"&table=ATTLOG&Stamp=1", "7\t2026-10-28 08:00:00\t0\t1\t0\n")
	h.stop()
	h.upax.Do(func(f *upaxtest.Fake) { f.FailPunches = 0 })
	h.start()
	eventually(t, "spool uploaded after restart", func() bool { return h.upax.PunchCount(pushSerial) == 1 })
}

func TestPushStoredDespiteStampSaveFailure(t *testing.T) {
	h := pushHarness(t)
	h.upax.Register(pushSerial, upaxtest.Terminal{})
	h.iclock("GET", "/iclock/cdata?SN="+pushSerial+"&options=all", "")
	eventually(t, "registered", func() bool { ok, _ := h.relay.Accept(pushSerial); return ok })
	h.upax.Do(func(f *upaxtest.Fake) { f.FailPunches = 1000 })
	// meta.json can't be replaced: a directory, with something in it.
	p := h.relay.knownPush(pushSerial)
	_ = os.Remove(p.metaPath())
	if err := os.MkdirAll(filepath.Join(p.metaPath(), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		code, body, _ := h.iclock("POST", "/iclock/cdata?SN="+pushSerial+"&table=ATTLOG&Stamp=9", "7\t2026-10-28 08:00:00\t0\t1\t0\n")
		if code != 200 || body != "OK: 1" {
			t.Fatalf("attlog: %d %q", code, body)
		}
	}
	// One spool entry per upload, not an endless re-send.
	if n := p.spool.Pending(); n != 2 {
		t.Fatalf("spooled %d", n)
	}
}

func TestPushBadSerialRefused(t *testing.T) {
	h := pushHarness(t)
	if code, _, _ := h.iclock("GET", "/iclock/cdata?SN=../../etc&options=all", ""); code != 400 {
		t.Fatalf("bad serial: %d", code)
	}
	if code, _, _ := h.iclock("GET", "/iclock/cdata?options=all", ""); code != 400 {
		t.Fatalf("no serial: %d", code)
	}
}
