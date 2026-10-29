package upax_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/identity"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
	"github.com/UPAX-platform/attendance-relay/internal/upax/upaxtest"
)

const code = "RLY-7KQ2-M9XD-4TPA"

func paired(t *testing.T) (*upaxtest.Fake, *upax.Client, *identity.Key) {
	t.Helper()
	f := upaxtest.New()
	t.Cleanup(f.Close)
	k, _ := identity.Generate()
	f.Pair(k, upax.StatusActive)
	return f, &upax.Client{Base: f.URL, RelayID: f.RelayID, Keys: upax.StaticKey{K: k}}, k
}

func TestPair(t *testing.T) {
	f := upaxtest.New()
	defer f.Close()
	f.Codes[code] = "01q2a0000000000000000000cd"
	k, _ := identity.Generate()
	c := &upax.Client{Base: f.URL}
	ctx := context.Background()

	if _, err := c.Pair(ctx, "RLY-0000-0000-0000", k, upax.PairRequest{}); !upax.IsStatus(err, 404) {
		t.Fatalf("unknown code: %v, want 404", err)
	}
	out, err := c.Pair(ctx, code, k, upax.PairRequest{HostName: "pc", Version: "2.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if out.RelayID != "01q2a0000000000000000000cd" || out.Status != upax.StatusPending {
		t.Fatalf("pair reply %+v", out)
	}
	other, _ := identity.Generate()
	if _, err := c.Pair(ctx, code, other, upax.PairRequest{}); !upax.IsCode(err, upax.CodePairingUsed) {
		t.Fatalf("reused code: %v, want %s", err, upax.CodePairingUsed)
	}
}

func TestSyncAndPunches(t *testing.T) {
	f, c, _ := paired(t)
	f.Register("SIM1", upaxtest.Terminal{Timezone: "Africa/Cairo", ClockSync: true})
	f.Queue("SIM1", upax.Command{Seq: 7, Line: "INFO"})
	ctx := context.Background()

	out, err := c.Sync(ctx, upax.SyncRequest{Version: "t", Terminals: []upax.TerminalReport{{Serial: "SIM1"}, {Serial: "NEW1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != upax.StatusActive || len(out.Terminals) != 1 || out.Terminals[0].Timezone != "Africa/Cairo" ||
		len(out.Terminals[0].Commands) != 1 || len(out.Unregistered) != 1 || out.Unregistered[0] != "NEW1" {
		t.Fatalf("sync reply %+v", out)
	}
	n, err := c.Punches(ctx, upax.PunchBatch{Serial: "SIM1", Punches: []upax.Punch{{PIN: "1", Local: "2026-10-28 08:00:00"}}})
	if err != nil || n != 1 || f.PunchCount("SIM1") != 1 {
		t.Fatalf("punches: n=%d err=%v", n, err)
	}
	if _, err := c.Punches(ctx, upax.PunchBatch{Serial: "NEW1", Punches: []upax.Punch{{PIN: "1"}}}); !upax.IsCode(err, upax.CodeTerminalNotActive) {
		t.Fatalf("unregistered terminal: %v", err)
	}
	if err := c.Results(ctx, []upax.Result{{Serial: "SIM1", Seq: 7, Cmd: "INFO"}}); err != nil {
		t.Fatal(err)
	}
}

func TestClockSkewCorrected(t *testing.T) {
	f, c, _ := paired(t)
	f.Do(func(f *upaxtest.Fake) { f.Clock = 10 * time.Minute })
	if _, err := c.Sync(context.Background(), upax.SyncRequest{}); err != nil {
		t.Fatalf("sync with a skewed clock: %v", err)
	}
	off, ok := c.ClockOffset()
	if !ok || off < 9*time.Minute || off > 11*time.Minute {
		t.Fatalf("offset %v (measured %v), want ~10m", off, ok)
	}
	if f.Rejected[0] != upax.CodeClockSkew {
		t.Fatalf("first answer %v, want a clock-skew refusal", f.Rejected)
	}
}

func TestClockBeyondCapStaysRefused(t *testing.T) {
	f, c, _ := paired(t)
	f.Do(func(f *upaxtest.Fake) { f.Clock = 3 * time.Hour })
	if _, err := c.Sync(context.Background(), upax.SyncRequest{}); !upax.IsCode(err, upax.CodeClockSkew) {
		t.Fatalf("3h skew: %v, want %s", err, upax.CodeClockSkew)
	}
}

func TestRefusals(t *testing.T) {
	f, c, _ := paired(t)
	ctx := context.Background()

	stranger, _ := identity.Generate()
	s := &upax.Client{Base: f.URL, RelayID: f.RelayID, Keys: upax.StaticKey{K: stranger}}
	if _, err := s.Sync(ctx, upax.SyncRequest{}); !upax.IsStatus(err, 404) {
		t.Fatalf("unknown key: %v, want 404", err)
	}
	f.Do(func(f *upaxtest.Fake) { f.MaxBatch = 1 })
	f.Register("SIM1", upaxtest.Terminal{})
	if _, err := c.Punches(ctx, upax.PunchBatch{Serial: "SIM1", Punches: make([]upax.Punch, 2)}); !upax.IsStatus(err, 413) {
		t.Fatalf("big batch: %v, want 413", err)
	}
	f.Do(func(f *upaxtest.Fake) { f.FailPunches = 1 })
	_, err := c.Punches(ctx, upax.PunchBatch{Serial: "SIM1", Punches: make([]upax.Punch, 1)})
	if e, ok := upax.AsError(err); !ok || e.Status != 503 || e.RetryAfter != time.Second {
		t.Fatalf("busy: %v, want 503 with Retry-After", err)
	}
}

func TestRotateKey(t *testing.T) {
	f, c, _ := paired(t)
	next, _ := identity.Generate()
	ctx := context.Background()
	if err := c.RotateKey(ctx, next); err != nil {
		t.Fatal(err)
	}
	// The old key stays valid during the grace period; the new one works.
	if _, err := c.Sync(ctx, upax.SyncRequest{}); err != nil {
		t.Fatalf("old key in grace: %v", err)
	}
	c.Keys = upax.StaticKey{K: next}
	if _, err := c.Sync(ctx, upax.SyncRequest{}); err != nil {
		t.Fatalf("new key: %v", err)
	}
	if f.Rotated != 1 {
		t.Fatal("rotation not recorded")
	}
}

func TestValidateBase(t *testing.T) {
	for _, ok := range []string{"https://acme.upax.io", "https://acme.upax-sb.io/", "http://localhost:8300", "http://127.0.0.1:8300"} {
		if err := upax.ValidateBase(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "acme.upax.io", "http://acme.upax.io", "https://acme.upax.io/dev/tok", "https://acme.upax.io?x=1", "ftp://x"} {
		if err := upax.ValidateBase(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	_, c, _ := paired(t)
	var hit atomic.Bool
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Store(true) }))
	defer elsewhere.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c.Base = redirect.URL
	_, err := c.Punches(context.Background(), upax.PunchBatch{Serial: "X", Punches: []upax.Punch{{PIN: "1", Local: "2026-10-28 08:00:00"}}})
	if !upax.IsStatus(err, http.StatusTemporaryRedirect) {
		t.Fatalf("err = %v, want the 307 itself", err)
	}
	if hit.Load() {
		t.Fatal("the signed request was re-sent to the redirect target")
	}
}
