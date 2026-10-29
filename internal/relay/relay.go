// Package relay connects attendance terminals on the LAN to UPAX. One sync
// loop reports the relay and its terminals to UPAX and learns what UPAX
// wants (statuses, clock settings, commands); one reader per configured
// terminal pulls punches over the vendor's SDK protocol; push-capable
// terminals can instead send to the relay's LAN listener (package push).
// Everything goes to UPAX over the signed relay API (package upax).
package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/config"
	"github.com/UPAX-platform/attendance-relay/internal/device"
	"github.com/UPAX-platform/attendance-relay/internal/device/zkteco/adms"
	"github.com/UPAX-platform/attendance-relay/internal/identity"
	"github.com/UPAX-platform/attendance-relay/internal/push"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
)

// Relay statuses beyond UPAX's own (active, pending, paused).
const (
	statusUnpaired         = "unpaired"          // UPAX doesn't know the key: 404
	statusSignatureInvalid = "signature_invalid" // UPAX refused the signature
	statusHeld             = "held"              // an upload said the relay isn't active; wait for the next sync
)

const (
	maxBatch         = 500
	defaultPoll      = 30 * time.Second
	defaultFullCheck = 10 * time.Minute
	clockThreshold   = 30 * time.Second
	clockSetEvery    = time.Hour
	keyMaxAge        = 365 * 24 * time.Hour
	keyPrevGrace     = 24 * time.Hour
	maxQueuedResults = 1000
	// maxDelay caps the waits UPAX asks for, so a bad value can't park the
	// relay for good.
	maxDelay = time.Hour
)

// Options configure a Relay. Zero test knobs take the defaults.
type Options struct {
	Config   *config.Config
	API      *upax.Client
	Keys     *identity.Source
	Log      *slog.Logger
	Version  string
	HostName string

	MinBackoff       time.Duration // reconnects and network errors: 5 s, doubling
	MaxBackoff       time.Duration // … up to 2 min
	UnpairedRetry    time.Duration // sync retry while UPAX doesn't know the key: 15 min
	SignatureBackoff time.Duration // after a signature or clock refusal: 5 min
	FullCheck        time.Duration // full log check per terminal: 10 min
	SyncEvery        time.Duration // overrides UPAX's poll_seconds (tests)
	ErrorDelay       time.Duration // overrides UPAX's error_delay_seconds (tests)
	// PushListener, if set, serves the LAN listener on it instead of
	// listening on the configured bind address (tests).
	PushListener net.Listener
}

// Relay is the running service.
type Relay struct {
	o       Options
	cfg     *config.Config
	api     *upax.Client
	log     *slog.Logger
	started time.Time
	health  *RelayHealth
	wake    chan struct{}

	mu       sync.Mutex
	ctx      context.Context
	status   string
	poll     time.Duration
	errDelay time.Duration
	readers  []*reader
	push     map[string]*pushTerm
	results  []upax.Result
	needSync bool
	wg       sync.WaitGroup
}

// New prepares a relay.
func New(o Options) *Relay {
	if o.MinBackoff <= 0 {
		o.MinBackoff = 5 * time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 2 * time.Minute
	}
	if o.UnpairedRetry <= 0 {
		o.UnpairedRetry = 15 * time.Minute
	}
	if o.SignatureBackoff <= 0 {
		o.SignatureBackoff = 5 * time.Minute
	}
	if o.FullCheck <= 0 {
		o.FullCheck = defaultFullCheck
	}
	r := &Relay{
		o: o, cfg: o.Config, api: o.API, log: o.Log, started: time.Now(),
		health: newRelayHealth(o.Config.StateDir()),
		wake:   make(chan struct{}, 1),
		poll:   defaultPoll, errDelay: defaultPoll,
		push: map[string]*pushTerm{},
	}
	if o.SyncEvery > 0 {
		r.poll = o.SyncEvery
	}
	if o.ErrorDelay > 0 {
		r.errDelay = o.ErrorDelay
	}
	return r
}

// Run runs the relay until ctx ends.
func (r *Relay) Run(ctx context.Context) error {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()

	r.recoverRotation(ctx)
	fp := ""
	if k, err := r.o.Keys.Key(); err == nil {
		fp = k.Fingerprint()
	}
	r.health.update(func(h *RelayHealth) {
		h.RelayID, h.Fingerprint, h.Version = r.cfg.RelayID, fp, r.o.Version
	})

	for _, t := range r.cfg.Terminals {
		rd, err := newReader(r, t)
		if err != nil {
			r.log.Error("terminal skipped", "terminal", t.Label(), "err", err)
			continue
		}
		r.mu.Lock()
		r.readers = append(r.readers, rd)
		r.mu.Unlock()
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			rd.run(ctx)
		}()
	}
	r.loadSpools()
	if r.cfg.Listener.Enabled {
		r.startListener(ctx)
	}
	r.syncLoop(ctx)
	r.wg.Wait()
	return nil
}

func (r *Relay) startListener(ctx context.Context) {
	mux := http.NewServeMux()
	mux.Handle(adms.Prefix, &adms.Handler{Hub: r, Log: r.log.With("listener", "adms")})
	srv := &push.Server{Bind: r.cfg.Listener.Bind, Handler: mux, Log: r.log}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		if r.o.PushListener != nil {
			if err := srv.Serve(ctx, r.o.PushListener); err != nil {
				r.listenerFailed(err, 0)
			}
			return
		}
		// The port may be taken for a while (an old instance still
		// stopping): keep trying rather than give push terminals up.
		backoff := r.o.MinBackoff
		for {
			err := srv.Run(ctx)
			if ctx.Err() != nil {
				return
			}
			r.listenerFailed(err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, r.o.MaxBackoff)
		}
	}()
	r.health.update(func(h *RelayHealth) { h.Listener = r.cfg.Listener.Bind })
	r.log.Info("push listener started; point push terminals' cloud server at this computer", "bind", r.cfg.Listener.Bind)
}

func (r *Relay) listenerFailed(err error, retryIn time.Duration) {
	if err == nil {
		err = errors.New("stopped")
	}
	r.log.Error("push listener stopped; push terminals can't reach the relay", "bind", r.cfg.Listener.Bind, "err", err, "retry_in", retryIn)
	r.health.fail(fmt.Errorf("push listener: %w", err))
}

// recovered turns a panic into *err, so one bad terminal or reply can't
// take the service down; the stack goes to the log. Use it as
// defer recovered(log, "what", &err).
func recovered(log *slog.Logger, what string, err *error) {
	if p := recover(); p != nil {
		log.Error(what+" failed unexpectedly; carrying on", "panic", p, "stack", string(debug.Stack()))
		*err = fmt.Errorf("internal error in %s: %v", what, p)
	}
}

// kick wakes the sync loop: results to send, or a terminal to report.
func (r *Relay) kick(needSync bool) {
	if needSync {
		r.mu.Lock()
		r.needSync = true
		r.mu.Unlock()
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// relayStatus is the relay's status in UPAX (or a local hold).
func (r *Relay) relayStatus() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Relay) delays() (poll, errDelay time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.poll, r.errDelay
}

// holdRelay is an upload's 409 RELAY_NOT_ACTIVE: stop uploading until the
// next sync says otherwise.
func (r *Relay) holdRelay() {
	r.mu.Lock()
	if r.status == upax.StatusActive {
		r.status = statusHeld
	}
	r.mu.Unlock()
	r.kick(true)
}

// ---- sync loop ----------------------------------------------------------------

func (r *Relay) syncLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	backoff := r.o.MinBackoff
	var holdUntil, lastSync time.Time
	for {
		woken := false
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-r.wake:
			woken = true
		}
		if woken {
			r.flushResults(ctx)
			r.mu.Lock()
			due := r.needSync && time.Now().After(holdUntil) && time.Since(lastSync) >= time.Second
			if due {
				r.needSync = false
			}
			r.mu.Unlock()
			if !due {
				continue
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		lastSync = time.Now()
		wait, failed := r.safeSync(ctx)
		switch {
		case !failed:
			backoff = r.o.MinBackoff
			holdUntil = time.Time{}
		case wait == 0:
			wait = backoff
			backoff = min(backoff*2, r.o.MaxBackoff)
			holdUntil = time.Now().Add(wait)
		default:
			holdUntil = time.Now().Add(wait)
		}
		timer.Reset(wait)
	}
}

// safeSync is syncOnce, with a panic taken as a failed sync.
func (r *Relay) safeSync(ctx context.Context) (wait time.Duration, failed bool) {
	var err error
	defer func() {
		if err != nil {
			wait, failed = 0, true
		}
	}()
	defer recovered(r.log, "sync", &err)
	return r.syncOnce(ctx)
}

// syncOnce runs one sync and returns when to run the next. failed with
// wait 0 means a network error: the caller backs off.
func (r *Relay) syncOnce(ctx context.Context) (wait time.Duration, failed bool) {
	req := upax.SyncRequest{
		Version:   r.o.Version,
		HostName:  r.o.HostName,
		UptimeS:   int64(time.Since(r.started) / time.Second),
		Terminals: r.reports(),
	}
	rep, err := r.api.Sync(ctx, req)
	if ctx.Err() != nil {
		return time.Second, true
	}
	if err != nil {
		return r.syncFailed(ctx, err), true
	}
	r.apply(rep)
	r.flushResults(ctx)
	r.maintainKey(ctx)
	poll, _ := r.delays()
	return poll, false
}

func (r *Relay) syncFailed(ctx context.Context, err error) time.Duration {
	_, errDelay := r.delays()
	e, isAPI := upax.AsError(err)
	set := func(status string) (changed bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		changed = r.status != status
		r.status = status
		return changed
	}
	r.health.fail(err)
	switch {
	case isAPI && e.Status == http.StatusNotFound:
		if set(statusUnpaired) {
			r.log.Error("UPAX doesn't know this relay (unpaired or retired in UPAX); punches stay on the terminals. Pair it again with: upax-relay pair", "retry_in", r.o.UnpairedRetry)
		}
		r.health.update(func(h *RelayHealth) { h.UPAXStatus = statusUnpaired })
		return r.o.UnpairedRetry
	case isAPI && e.Code == upax.CodeClockSkew:
		off, _ := r.api.ClockOffset()
		r.log.Error("UPAX refused the relay's requests because this computer's clock is off by more than an hour; fix the clock (enable automatic time)", "measured_offset", off, "retry_in", r.o.SignatureBackoff)
		return r.o.SignatureBackoff
	case isAPI && e.Status == http.StatusUnauthorized:
		// A rotation UPAX took but the relay didn't finish installing.
		r.recoverRotation(ctx)
		set(statusSignatureInvalid)
		r.log.Error("UPAX refused the relay's signature; check that the relay's fingerprint matches the one in UPAX, or pair again", "fingerprint", identity.Group(r.fingerprint()), "code", e.Code, "retry_in", r.o.SignatureBackoff)
		r.health.update(func(h *RelayHealth) { h.UPAXStatus = statusSignatureInvalid })
		return r.o.SignatureBackoff
	case isAPI && e.RetryAfter > 0:
		wait := min(e.RetryAfter, maxDelay)
		r.log.Warn("UPAX is busy", "status", e.Status, "retry_in", wait)
		return wait
	case isAPI:
		r.log.Warn("sync refused by UPAX", "err", err, "retry_in", errDelay)
		return errDelay
	case errors.Is(err, identity.ErrNoKey):
		r.log.Error(err.Error())
		return r.o.UnpairedRetry
	}
	r.log.Warn("could not reach UPAX", "err", err)
	return 0
}

func (r *Relay) fingerprint() string {
	if k, err := r.o.Keys.Key(); err == nil {
		return k.Fingerprint()
	}
	return ""
}

func seconds(n int, def time.Duration) time.Duration {
	if n <= 0 {
		return def
	}
	return time.Duration(min(n, int(maxDelay/time.Second))) * time.Second
}

// apply hands the sync reply out: the relay's status, and each terminal's
// UPAX state and commands.
func (r *Relay) apply(rep upax.SyncReply) {
	r.mu.Lock()
	prev := r.status
	r.status = rep.Status
	r.poll = seconds(rep.PollSeconds, defaultPoll)
	r.errDelay = seconds(rep.ErrorDelaySeconds, defaultPoll)
	if r.o.SyncEvery > 0 {
		r.poll = r.o.SyncEvery
	}
	if r.o.ErrorDelay > 0 {
		r.errDelay = r.o.ErrorDelay
	}
	readers := append([]*reader(nil), r.readers...)
	pushed := make([]*pushTerm, 0, len(r.push))
	for _, p := range r.push {
		pushed = append(pushed, p)
	}
	r.mu.Unlock()

	if prev != rep.Status && !(prev == statusHeld && rep.Status == upax.StatusActive) {
		switch rep.Status {
		case upax.StatusActive:
			r.log.Info("relay is active in UPAX")
		case upax.StatusPending:
			r.log.Warn("relay is waiting for activation in UPAX (HR › Devices › Relays): check that the fingerprint there matches this one, then activate", "fingerprint", identity.Group(r.fingerprint()))
		case upax.StatusPaused:
			r.log.Warn("relay is paused in UPAX; punches stay on the terminals until it is resumed")
		default:
			r.log.Warn("UPAX reports an unknown relay status; uploads are held", "status", rep.Status)
		}
	}
	off, measured := r.api.ClockOffset()
	r.health.update(func(h *RelayHealth) {
		h.UPAXStatus, h.LastSync = rep.Status, time.Now()
		if measured {
			s := int(off / time.Second)
			h.ClockOffsetS = &s
		}
	})

	states := map[string]upax.TerminalState{}
	for _, t := range rep.Terminals {
		states[t.Serial] = t
	}
	// UPAX lists terminals only for an active relay: otherwise their state
	// is unknown, not "unregistered".
	known := rep.Status == upax.StatusActive
	view := func(serial string) (upaxView, []upax.Command) {
		st, ok := states[serial]
		v := upaxView{known: known, registered: ok}
		if ok {
			v.deviceID, v.status, v.timezone, v.clockSync = st.DeviceID, st.Status, st.Timezone, st.ClockSync
		}
		return v, st.Commands
	}
	for _, rd := range readers {
		if s := rd.serial(); s != "" {
			rd.setView(view(s))
		}
	}
	for _, p := range pushed {
		p.setView(view(p.serial))
	}
}

func (r *Relay) reports() []upax.TerminalReport {
	r.mu.Lock()
	readers := append([]*reader(nil), r.readers...)
	pushed := make([]*pushTerm, 0, len(r.push))
	for _, p := range r.push {
		pushed = append(pushed, p)
	}
	r.mu.Unlock()
	out := []upax.TerminalReport{}
	for _, rd := range readers {
		if rep, ok := rd.report(); ok {
			out = append(out, rep)
		}
	}
	for _, p := range pushed {
		out = append(out, p.report())
	}
	return out
}

// ---- results ------------------------------------------------------------------

func (r *Relay) addResult(res upax.Result) {
	r.mu.Lock()
	if len(r.results) >= maxQueuedResults {
		r.results = r.results[1:]
	}
	r.results = append(r.results, res)
	r.mu.Unlock()
	r.kick(false)
}

func (r *Relay) flushResults(ctx context.Context) {
	var err error
	defer recovered(r.log, "command results", &err)
	r.mu.Lock()
	rs := r.results
	r.results = nil
	r.mu.Unlock()
	if len(rs) == 0 {
		return
	}
	if err := r.api.Results(ctx, rs); err != nil {
		r.log.Warn("could not report command results to UPAX; will retry", "count", len(rs), "err", err)
		r.mu.Lock()
		r.results = append(rs, r.results...)
		if n := len(r.results); n > maxQueuedResults {
			r.results = r.results[n-maxQueuedResults:]
		}
		r.mu.Unlock()
	}
}

// ---- uploads ------------------------------------------------------------------

// uploadOutcome tells a terminal what to do after a failed upload.
type uploadOutcome struct {
	wait         time.Duration // don't retry before this
	holdTerminal bool          // UPAX says this terminal isn't active
}

// upload sends punches for serial in batches, halving the batch on 413.
// done is called after each accepted batch with the number of punches it
// held.
func (r *Relay) upload(ctx context.Context, serial string, punches []device.Punch, batch *int, done func(n int) error) error {
	if *batch <= 0 || *batch > maxBatch {
		*batch = maxBatch
	}
	for len(punches) > 0 {
		n := min(*batch, len(punches))
		b := upax.PunchBatch{Serial: serial, Punches: make([]upax.Punch, n)}
		for i, p := range punches[:n] {
			wc := p.WorkCode
			if wc == "" {
				wc = "0"
			}
			b.Punches[i] = upax.Punch{PIN: p.PIN, Local: p.Time.Format(time.DateTime), State: p.State, Verify: p.Verify, WorkCode: wc}
		}
		accepted, err := r.api.Punches(ctx, b)
		if upax.IsStatus(err, http.StatusRequestEntityTooLarge) && *batch > 1 {
			*batch = max(1, n/2)
			r.log.Info("UPAX found the batch too large; halving it", "serial", serial, "batch", *batch)
			continue
		}
		if err != nil {
			return err
		}
		if accepted != n {
			r.log.Warn("UPAX accepted a different number of punches than sent", "serial", serial, "sent", n, "accepted", accepted)
		}
		if err := done(n); err != nil {
			return err
		}
		punches = punches[n:]
	}
	return nil
}

// uploadFailed decides what a failed upload means.
func (r *Relay) uploadFailed(err error) uploadOutcome {
	_, errDelay := r.delays()
	e, isAPI := upax.AsError(err)
	switch {
	case !isAPI:
		return uploadOutcome{wait: errDelay}
	case e.Code == upax.CodeTerminalNotActive:
		return uploadOutcome{wait: errDelay, holdTerminal: true}
	case e.Code == upax.CodeNotActive:
		r.holdRelay()
		return uploadOutcome{wait: errDelay}
	case e.Status == http.StatusNotFound || e.Status == http.StatusUnauthorized:
		// The sync loop owns these: let it find out and report.
		r.kick(true)
		return uploadOutcome{wait: max(errDelay, r.o.SignatureBackoff)}
	case e.RetryAfter > 0:
		return uploadOutcome{wait: min(max(e.RetryAfter, time.Second), maxDelay)}
	}
	return uploadOutcome{wait: errDelay}
}

// ---- key upkeep -------------------------------------------------------------------

// maintainKey drops the previous key after its grace period and rotates the
// key once a year.
func (r *Relay) maintainKey(ctx context.Context) {
	path := r.cfg.KeyPath()
	if fi, err := os.Stat(path + ".prev"); err == nil && time.Since(fi.ModTime()) > keyPrevGrace {
		_ = os.Remove(path + ".prev")
	}
	k, err := r.o.Keys.Key()
	if err != nil || time.Since(k.Created) < keyMaxAge || r.relayStatus() != upax.StatusActive {
		return
	}
	r.log.Info("the relay key is a year old; rotating it")
	next, err := RotateKey(ctx, r.api, path)
	if err != nil {
		r.log.Warn("key rotation failed; will retry at a later sync", "err", err)
		return
	}
	r.health.update(func(h *RelayHealth) { h.Fingerprint = next.Fingerprint() })
	r.log.Info("relay key rotated", "fingerprint", identity.Group(next.Fingerprint()))
}

// RotateKey replaces the relay key at path: it registers a new key with
// UPAX (signed with the current one), then swaps the files, keeping the old
// key as path.prev. The new key is written to path.next first, so a crash
// between UPAX's answer and the swap is recovered at the next start.
func RotateKey(ctx context.Context, api *upax.Client, path string) (*identity.Key, error) {
	cur, err := identity.Load(path)
	if err != nil {
		return nil, err
	}
	next, err := identity.Generate()
	if err != nil {
		return nil, err
	}
	if err := next.Save(path + ".next"); err != nil {
		return nil, err
	}
	if err := api.RotateKey(ctx, next); err != nil {
		os.Remove(path + ".next")
		return nil, err
	}
	// UPAX has the new key now: install it even if the old one can't be
	// kept (.prev is only a fallback).
	_ = cur.Save(path + ".prev")
	if err := os.Rename(path+".next", path); err != nil {
		return nil, err
	}
	return next, nil
}

// recoverRotation finishes a rotation interrupted after UPAX answered: if
// the half-installed new key works, it becomes the key.
func (r *Relay) recoverRotation(ctx context.Context) {
	path := r.cfg.KeyPath()
	next, err := identity.Load(path + ".next")
	if err != nil {
		return
	}
	probe := &upax.Client{Base: r.api.Base, RelayID: r.api.RelayID, Keys: upax.StaticKey{K: next}, HTTP: r.api.HTTP, UserAgent: r.api.UserAgent}
	if _, err := probe.Sync(ctx, upax.SyncRequest{Version: r.o.Version, HostName: r.o.HostName}); err != nil {
		if _, refused := upax.AsError(err); refused {
			r.log.Info("discarding an unfinished key rotation", "err", err)
			os.Remove(path + ".next")
		}
		return
	}
	if cur, err := identity.Load(path); err == nil {
		_ = cur.Save(path + ".prev")
	}
	if err := os.Rename(path+".next", path); err == nil {
		r.log.Info("finished an interrupted key rotation", "fingerprint", identity.Group(next.Fingerprint()))
	}
}
