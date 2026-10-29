// Package upax is the relay's client for the UPAX relay API: pairing, the
// sync heartbeat, punch uploads, command results and key rotation. Every
// request is signed with the relay key (package sig).
package upax

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/identity"
	"github.com/UPAX-platform/attendance-relay/internal/upax/sig"
)

// Error codes the relay acts on (the body's error_code).
const (
	CodeSignatureInvalid  = "RELAY_SIGNATURE_INVALID"
	CodeClockSkew         = "RELAY_CLOCK_SKEW"
	CodeNotActive         = "RELAY_NOT_ACTIVE"
	CodeTerminalNotActive = "RELAY_TERMINAL_NOT_ACTIVE"
	CodePairingUsed       = "RELAY_PAIRING_USED"
)

// Error is a refusal from UPAX.
type Error struct {
	Status     int
	Code       string // error_code from the body, if any
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("UPAX answered %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("UPAX answered %d %s", e.Status, http.StatusText(e.Status))
}

// AsError returns err's *Error, if it is one.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// IsStatus reports whether err is a refusal with the given HTTP status.
func IsStatus(err error, status int) bool {
	e, ok := AsError(err)
	return ok && e.Status == status
}

// IsCode reports whether err is a refusal with the given error_code.
func IsCode(err error, code string) bool {
	e, ok := AsError(err)
	return ok && e.Code == code
}

// KeySource hands out the relay key; identity.Source reloads it from disk.
type KeySource interface {
	Key() (*identity.Key, error)
}

// StaticKey is a KeySource for a key held in memory.
type StaticKey struct{ K *identity.Key }

func (s StaticKey) Key() (*identity.Key, error) { return s.K, nil }

// Client talks to one UPAX public plane origin.
type Client struct {
	Base      string // origin, e.g. https://acme.upax.io (sandbox: acme.upax-sb.io)
	RelayID   string
	Keys      KeySource
	HTTP      *http.Client
	UserAgent string

	mu       sync.Mutex
	offset   time.Duration // UPAX's clock minus ours
	measured bool
}

// maxOffset caps the clock correction: a relay further off than this has a
// broken clock to fix, not to paper over.
const maxOffset = time.Hour

// maxRetryAfter (seconds) bounds a Retry-After, so a bad header can't
// overflow the duration.
const maxRetryAfter = 24 * 60 * 60

var relayIDRe = regexp.MustCompile(`^[0-9a-z]{26}$`)

// ValidateBase checks a UPAX origin: https (http only for local
// development), a host, and no path, query or credentials.
func ValidateBase(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return fmt.Errorf("the UPAX address %q is not a URL like https://<your-org>.upax.io", s)
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("the UPAX address is just the origin (like https://<your-org>.upax.io), without a path; got %q", s)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("the UPAX address must use https:// (plain http is only for localhost); got %q", s)
	}
	return fmt.Errorf("the UPAX address must start with https://; got %q", s)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ValidateRelayID checks a relay id as UPAX issues them.
func ValidateRelayID(id string) error {
	if !relayIDRe.MatchString(id) {
		return fmt.Errorf("relay_id %q is not a UPAX id", id)
	}
	return nil
}

// Now is the current time corrected by the measured offset to UPAX's clock.
func (c *Client) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

// ClockOffset is UPAX's clock minus this machine's, and whether it has been
// measured yet.
func (c *Client) ClockOffset() (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset, c.measured
}

// observe learns the clock offset from a response's Date header. Date has
// one-second resolution (truncated), so differences under 2 s are noise.
func (c *Client) observe(resp *http.Response, sent, received time.Time) {
	d, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return
	}
	mid := sent.Add(received.Sub(sent) / 2)
	obs := d.Add(500 * time.Millisecond).Sub(mid)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.measured = true
	if diff := obs - c.offset; diff > 2*time.Second || diff < -2*time.Second {
		c.offset = max(-maxOffset, min(maxOffset, obs)).Round(time.Second)
	} else if obs < 2*time.Second && obs > -2*time.Second {
		c.offset = 0
	}
}

// Pair registers key with UPAX using a one-time pairing code. The request is
// signed with key itself: proof of possession.
func (c *Client) Pair(ctx context.Context, code string, key *identity.Key, req PairRequest) (PairReply, error) {
	var out PairReply
	req.PublicKey = key.PublicBase64()
	err := c.post(ctx, "/rpair/"+code, key, req, &out)
	if err == nil {
		if err = ValidateRelayID(out.RelayID); err != nil {
			err = fmt.Errorf("UPAX paired the relay but answered with %w", err)
		}
	}
	return out, err
}

// Sync is the heartbeat: it reports the terminals and returns UPAX's view.
func (c *Client) Sync(ctx context.Context, req SyncRequest) (SyncReply, error) {
	var out SyncReply
	if req.Terminals == nil {
		req.Terminals = []TerminalReport{}
	}
	err := c.relayPost(ctx, "sync", req, &out)
	return out, err
}

// Punches uploads one terminal's batch and returns how many UPAX accepted.
func (c *Client) Punches(ctx context.Context, b PunchBatch) (int, error) {
	var out punchReply
	err := c.relayPost(ctx, "punches", b, &out)
	return out.Accepted, err
}

// Results reports command outcomes.
func (c *Client) Results(ctx context.Context, rs []Result) error {
	return c.relayPost(ctx, "results", resultsRequest{Results: rs}, nil)
}

// RotateKey registers next as the relay's key, signed with the current one.
func (c *Client) RotateKey(ctx context.Context, next *identity.Key) error {
	return c.relayPost(ctx, "key", keyRequest{PublicKey: next.PublicBase64()}, nil)
}

func (c *Client) relayPost(ctx context.Context, member string, in, out any) error {
	if err := ValidateRelayID(c.RelayID); err != nil {
		return err
	}
	key, err := c.Keys.Key()
	if err != nil {
		return err
	}
	return c.post(ctx, "/rly/"+c.RelayID+"/"+member, key, in, out)
}

// post sends a signed JSON request. A clock-skew refusal is retried once,
// after the offset has been corrected from the refusal's Date header.
func (c *Client) post(ctx context.Context, path string, key *identity.Key, in, out any) error {
	if err := ValidateBase(c.Base); err != nil {
		return err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	u := strings.TrimRight(c.Base, "/") + path
	for attempt := 0; ; attempt++ {
		err := c.send(ctx, u, key, body, out)
		if attempt == 0 && IsCode(err, CodeClockSkew) {
			continue
		}
		return err
	}
}

func (c *Client) send(ctx context.Context, u string, key *identity.Key, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	nonce, err := sig.Nonce(nil)
	if err != nil {
		return err
	}
	sig.Sign(req, body, key, c.Now(), nonce)

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	sent := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.observe(resp, sent, time.Now())
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode}
		var eb struct {
			Code string `json:"error_code"`
		}
		if json.Unmarshal(b, &eb) == nil {
			e.Code = eb.Code
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			e.RetryAfter = time.Duration(min(s, maxRetryAfter)) * time.Second
		}
		return e
	}
	if out == nil || len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("UPAX answered with something that isn't the relay API (%w); check the UPAX address", err)
	}
	return nil
}
