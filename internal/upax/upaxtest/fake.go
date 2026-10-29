// Package upaxtest is a fake UPAX relay API for tests. It verifies requests
// the way the platform does (signature profile, ±300 s window, nonce
// replay, Content-Digest, keyid naming the current or previous key) and
// answers with the platform's refusal bodies.
package upaxtest

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/identity"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
	"github.com/UPAX-platform/attendance-relay/internal/upax/sig"
)

// Terminal is a terminal registered to the relay in UPAX.
type Terminal struct {
	DeviceID  string
	Status    string
	Timezone  string
	ClockSync bool
	Commands  []upax.Command
}

// Fake is the server. Lock-free fields are set before use; the rest go
// through Do.
type Fake struct {
	*httptest.Server

	mu sync.Mutex
	// Codes maps a valid pairing code (RLY-XXXX-XXXX-XXXX) to the relay id
	// it pairs; a used code answers RELAY_PAIRING_USED.
	Codes map[string]string
	used  map[string]bool
	// RelayID, Status, Pub, PrevPub: the one relay this fake knows.
	RelayID string
	Status  string
	Pub     ed25519.PublicKey
	PrevPub ed25519.PublicKey
	// Terminals registered to the relay, by serial.
	Terminals map[string]*Terminal
	// Clock is added to the real time: the fake's clock.
	Clock time.Duration
	// MaxBatch answers 413 for larger punch batches (0 = 500).
	MaxBatch int
	// FailPunches answers the next n punch uploads 503.
	FailPunches int
	// RefusePINs answers 422 to any punch batch holding one of these PINs.
	RefusePINs map[string]bool
	// Refuse, if set, answers every /rly request with this status and code.
	RefuseStatus int
	RefuseCode   string

	Syncs    []upax.SyncRequest
	Punches  map[string][]upax.Punch // by serial, deduplicated as UPAX does
	Received map[string]int          // punches received per serial, duplicates included
	Batches  []int                   // sizes of accepted punch batches
	Results  []upax.Result
	Rotated  int
	Rejected []string // refusal codes answered, in order

	nonces map[string]bool
}

// New starts a fake with one unpaired relay id.
func New() *Fake {
	f := &Fake{
		Codes:     map[string]string{},
		used:      map[string]bool{},
		RelayID:   "01q2a0000000000000000000ab",
		Status:    upax.StatusPending,
		Terminals: map[string]*Terminal{},
		Punches:   map[string][]upax.Punch{},
		Received:  map[string]int{},
		nonces:    map[string]bool{},
	}
	f.Server = httptest.NewServer(f)
	return f
}

// Do runs fn under the fake's lock.
func (f *Fake) Do(fn func(f *Fake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// Pair registers a key directly, as if pairing had happened.
func (f *Fake) Pair(k *identity.Key, status string) {
	f.Do(func(f *Fake) { f.Pub, f.Status = k.Public(), status })
}

// Register adds (or updates) a terminal registered to the relay.
func (f *Fake) Register(serial string, t Terminal) {
	f.Do(func(f *Fake) {
		if t.DeviceID == "" {
			t.DeviceID = "dev-" + serial
		}
		if t.Status == "" {
			t.Status = upax.StatusActive
		}
		f.Terminals[serial] = &t
	})
}

// Queue adds a command for a terminal.
func (f *Fake) Queue(serial string, c upax.Command) {
	f.Do(func(f *Fake) { f.Terminals[serial].Commands = append(f.Terminals[serial].Commands, c) })
}

// PunchCount is how many punches UPAX holds for serial.
func (f *Fake) PunchCount(serial string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Punches[serial])
}

// GotPunches returns a copy of serial's punches.
func (f *Fake) GotPunches(serial string) []upax.Punch {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]upax.Punch(nil), f.Punches[serial]...)
}

// ReceivedCount is how many punches arrived for serial, duplicates included.
func (f *Fake) ReceivedCount(serial string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Received[serial]
}

// LastSync returns the latest sync request.
func (f *Fake) LastSync() (upax.SyncRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Syncs) == 0 {
		return upax.SyncRequest{}, false
	}
	return f.Syncs[len(f.Syncs)-1], true
}

func (f *Fake) now() time.Time { return time.Now().Add(f.Clock) }

func (f *Fake) refuse(w http.ResponseWriter, status int, code string) {
	f.Rejected = append(f.Rejected, code)
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
	}
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error_code":%q}`, code)
}

func (f *Fake) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Date", f.now().UTC().Format(http.TimeFormat))
	if r.Method != http.MethodPost || r.URL.RawQuery != "" {
		f.refuse(w, 404, "NOT_FOUND")
		return
	}
	body, _ := io.ReadAll(r.Body)
	p, err := sig.Parse(r.Header)
	if err != nil {
		f.refuse(w, 401, upax.CodeSignatureInvalid)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	var pub ed25519.PublicKey
	switch {
	case len(parts) == 2 && parts[0] == "rpair":
		id, ok := f.Codes[parts[1]]
		if !ok {
			f.refuse(w, 404, "NOT_FOUND")
			return
		}
		var pr upax.PairRequest
		if json.Unmarshal(body, &pr) != nil {
			f.refuse(w, 400, "BAD_REQUEST")
			return
		}
		pk, _ := base64.StdEncoding.DecodeString(pr.PublicKey)
		if len(pk) != ed25519.PublicKeySize || p.KeyID != identity.Fingerprint(pk) {
			f.refuse(w, 401, upax.CodeSignatureInvalid)
			return
		}
		if !f.verify(w, r, p, pk, body) {
			return
		}
		if f.used[parts[1]] {
			f.refuse(w, 409, upax.CodePairingUsed)
			return
		}
		f.used[parts[1]] = true
		f.RelayID, f.Pub, f.PrevPub, f.Status = id, pk, nil, upax.StatusPending
		f.reply(w, 202, upax.PairReply{RelayID: id, Status: f.Status, PollSeconds: 1})
		return
	case len(parts) == 3 && parts[0] == "rly" && parts[1] == f.RelayID && f.Pub != nil:
		switch p.KeyID {
		case identity.Fingerprint(f.Pub):
			pub = f.Pub
		case fingerprintOr(f.PrevPub):
			pub = f.PrevPub
		default:
			f.refuse(w, 404, "NOT_FOUND")
			return
		}
	default:
		f.refuse(w, 404, "NOT_FOUND")
		return
	}
	if !f.verify(w, r, p, pub, body) {
		return
	}
	if f.RefuseStatus != 0 {
		f.refuse(w, f.RefuseStatus, f.RefuseCode)
		return
	}

	switch parts[2] {
	case "sync":
		var req upax.SyncRequest
		if json.Unmarshal(body, &req) != nil {
			f.refuse(w, 400, "BAD_REQUEST")
			return
		}
		f.Syncs = append(f.Syncs, req)
		out := upax.SyncReply{Status: f.Status, PollSeconds: 1, ErrorDelaySeconds: 1, Terminals: []upax.TerminalState{}, Unregistered: []string{}}
		if f.Status == upax.StatusActive {
			for _, t := range req.Terminals {
				reg, ok := f.Terminals[t.Serial]
				if !ok {
					out.Unregistered = append(out.Unregistered, t.Serial)
					continue
				}
				out.Terminals = append(out.Terminals, upax.TerminalState{Serial: t.Serial, DeviceID: reg.DeviceID, Status: reg.Status, Timezone: reg.Timezone, ClockSync: reg.ClockSync, Commands: reg.Commands})
				reg.Commands = nil
			}
		}
		f.reply(w, 200, out)
	case "punches":
		var b upax.PunchBatch
		if json.Unmarshal(body, &b) != nil {
			f.refuse(w, 400, "BAD_REQUEST")
			return
		}
		limit := f.MaxBatch
		if limit == 0 {
			limit = 500
		}
		switch t := f.Terminals[b.Serial]; {
		case len(b.Punches) > limit:
			f.refuse(w, 413, "PAYLOAD_TOO_LARGE")
		case f.Status != upax.StatusActive:
			f.refuse(w, 409, upax.CodeNotActive)
		case t == nil || t.Status != upax.StatusActive:
			f.refuse(w, 409, upax.CodeTerminalNotActive)
		case f.FailPunches > 0:
			f.FailPunches--
			f.refuse(w, 503, "SERVICE_UNAVAILABLE")
		case slices.ContainsFunc(b.Punches, func(x upax.Punch) bool { return f.RefusePINs[x.PIN] }):
			f.refuse(w, 422, "VALIDATION_ERROR")
		default:
			// UPAX dedupes on terminal + PIN + local time + state.
			have := map[upax.Punch]bool{}
			for _, x := range f.Punches[b.Serial] {
				have[x] = true
			}
			f.Received[b.Serial] += len(b.Punches)
			f.Batches = append(f.Batches, len(b.Punches))
			for _, x := range b.Punches {
				if !have[x] {
					f.Punches[b.Serial] = append(f.Punches[b.Serial], x)
					have[x] = true
				}
			}
			f.reply(w, 200, map[string]int{"accepted": len(b.Punches)})
		}
	case "results":
		var rr struct {
			Results []upax.Result `json:"results"`
		}
		if json.Unmarshal(body, &rr) != nil {
			f.refuse(w, 400, "BAD_REQUEST")
			return
		}
		f.Results = append(f.Results, rr.Results...)
		f.reply(w, 200, map[string]int{"accepted": len(rr.Results)})
	case "key":
		var kr struct {
			PublicKey string `json:"public_key"`
		}
		pk, err := base64.StdEncoding.DecodeString(func() string { _ = json.Unmarshal(body, &kr); return kr.PublicKey }())
		if err != nil || len(pk) != ed25519.PublicKeySize {
			f.refuse(w, 400, "BAD_REQUEST")
			return
		}
		f.PrevPub, f.Pub = f.Pub, pk
		f.Rotated++
		f.reply(w, 200, map[string]string{"fingerprint": identity.Fingerprint(pk)})
	default:
		f.refuse(w, 404, "NOT_FOUND")
	}
}

func fingerprintOr(pub ed25519.PublicKey) string {
	if pub == nil {
		return "-"
	}
	return identity.Fingerprint(pub)
}

func (f *Fake) verify(w http.ResponseWriter, r *http.Request, p *sig.Parsed, pub ed25519.PublicKey, body []byte) bool {
	authority := strings.ToLower(r.Host)
	switch err := p.Verify(pub, r.Method, authority, r.URL.EscapedPath(), f.now()); err {
	case nil:
	case sig.ErrClockSkew:
		f.refuse(w, 401, upax.CodeClockSkew)
		return false
	default:
		f.refuse(w, 401, upax.CodeSignatureInvalid)
		return false
	}
	if f.nonces[p.Nonce] || p.VerifyDigest(body) != nil {
		f.refuse(w, 401, upax.CodeSignatureInvalid)
		return false
	}
	f.nonces[p.Nonce] = true
	return true
}
