// Package relay connects ZKTeco devices on the LAN to UPAX: it reads new
// punches over the device's SDK protocol and pushes them to the platform
// using the ADMS push protocol, so UPAX sees a normal push-capable device.
package relay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UPAX-platform/zk-relay/internal/adms"
	"github.com/UPAX-platform/zk-relay/internal/config"
	"github.com/UPAX-platform/zk-relay/internal/zk"
)

const (
	batchSize     = 200
	fullSyncEvery = 10 * time.Minute
	deviceTimeout = 15 * time.Second
	// handshakeEvery refreshes the options (Delay, ErrorDelay) and the
	// device's UPAX status on a long-lived session.
	handshakeEvery = time.Hour
	// inactiveCheck is how often a device UPAX hasn't activated (or has
	// paused) re-handshakes to notice that it has been.
	inactiveCheck = time.Minute
	// rejectedRetry spaces out uploads UPAX refused outright (4xx).
	rejectedRetry = 5 * time.Minute
)

// Run starts one worker per configured device and blocks until ctx ends.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger, version string) {
	var wg sync.WaitGroup
	for _, d := range cfg.Devices {
		w := &Worker{
			Device:   d,
			URL:      cfg.URLFor(d),
			Backfill: cfg.Backfill,
			Poll:     time.Duration(cfg.PollInterval),
			StateDir: cfg.StateDir(),
			Log:      log.With("device", d.Label()),
			Version:  version,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Run(ctx)
		}()
	}
	wg.Wait()
}

// Worker relays one device.
type Worker struct {
	Device   config.Device
	URL      string
	Backfill string
	Poll     time.Duration
	StateDir string
	Log      *slog.Logger
	Version  string
	HTTP     *http.Client

	// MinBackoff/MaxBackoff bound the reconnect delay (defaults 5s / 2m).
	MinBackoff, MaxBackoff time.Duration
	// InactiveCheck overrides inactiveCheck (tests).
	InactiveCheck time.Duration

	health *Health
}

// Run keeps a session alive, reconnecting with backoff, until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	minB, maxB := w.MinBackoff, w.MaxBackoff
	if minB <= 0 {
		minB = 5 * time.Second
	}
	if maxB <= 0 {
		maxB = 2 * time.Minute
	}
	w.health = newHealth(w.StateDir, w.Device.Label(), w.Device.Addr(), w.Version)
	w.health.update(func(*Health) {})
	backoff := minB
	for {
		started := time.Now()
		err := w.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = minB
		}
		wait := backoff
		var se *adms.StatusError
		switch {
		case adms.IsNotFound(err):
			wait = max(maxB, 5*time.Minute)
			w.Log.Error("UPAX does not recognise this device address, or the device's serial number differs from the one UPAX has for it; check upax_url and the device in UPAX", "retry_in", wait)
			w.health.update(func(h *Health) { h.UPAXStatus = "rejected" })
		case errors.As(err, &se) && se.RetryAfter > 0:
			wait = max(wait, se.RetryAfter)
			w.Log.Warn("session ended", "err", err, "retry_in", wait)
		default:
			w.Log.Warn("session ended", "err", err, "retry_in", wait)
		}
		if err != nil {
			w.health.fail(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, maxB)
	}
}

type deviceError struct{ err error }

func (e deviceError) Error() string { return "device: " + e.err.Error() }
func (e deviceError) Unwrap() error { return e.err }

type session struct {
	w        *Worker
	zc       *zk.Client
	ac       *adms.Client
	st       *State
	opts     adms.Options
	firmware string
	sizes    zk.Sizes

	lastHandshake time.Time
	handshakeHold time.Time // after a failed re-handshake, don't retry before this
	recheck       bool      // re-handshake on the next poll (an upload got 503)
	infoSent      string    // the INFO summary UPAX last received (infoKey)
	waitingLogged bool

	lastFull   time.Time
	retryAfter time.Time // don't retry uploads before this
	dirty      bool      // an upload failed and must be retried
	// recs/idx are the log and the indexes still to upload, kept after a
	// failed upload so the retry doesn't download the device log again.
	recs []zk.Record
	idx  []int
}

func (w *Worker) session(ctx context.Context) error {
	zc, err := zk.Dial(ctx, w.Device.Addr(), w.Device.Password, deviceTimeout)
	if err != nil {
		return deviceError{err}
	}
	defer zc.Close()

	sn := w.Device.Serial
	if sn == "" {
		if sn, err = zc.SerialNumber(); err != nil {
			return deviceError{fmt.Errorf("read serial number: %w", err)}
		}
		if sn == "" {
			return deviceError{errors.New("device reported an empty serial number; set `serial` in the config")}
		}
	}
	fw, _ := zc.Firmware()
	sz, err := zc.Sizes()
	if err != nil {
		return deviceError{err}
	}
	w.health.update(func(h *Health) { h.Serial = sn })

	ac := &adms.Client{Base: w.URL, SN: sn, HTTP: w.HTTP, UserAgent: "upax-zk-relay/" + w.Version}
	s := &session{w: w, zc: zc, ac: ac, firmware: fw, sizes: sz}
	if err := s.handshake(ctx); err != nil {
		return fmt.Errorf("UPAX handshake: %w", err)
	}
	if s.st, err = loadState(w.StateDir, sn); err != nil {
		return err
	}
	if err := s.checkAddress(); err != nil {
		return err
	}
	w.health.update(func(h *Health) { h.Uploaded = s.st.Uploaded })
	w.Log.Info("connected", "serial", sn, "firmware", fw, "heartbeat", s.opts.Delay, "upax_status", s.status(), "upax_stamp", s.opts.ATTLOGStamp)

	if err := s.syncIfActive(ctx); err != nil {
		return err
	}

	poll := time.NewTicker(w.Poll)
	defer poll.Stop()
	hb := time.NewTicker(s.opts.Delay)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
			delay := s.opts.Delay
			if err := s.poll(ctx); err != nil {
				return err
			}
			if s.opts.Delay != delay {
				hb.Reset(s.opts.Delay)
			}
		case <-hb.C:
			if err := s.heartbeat(ctx); err != nil {
				return err
			}
		}
	}
}

// poll is one device check: re-handshake when due, then sync when the device
// has new records, a full check is due, or a failed upload may be retried.
// A device UPAX hasn't activated is not read at all: its punches stay on it.
func (s *session) poll(ctx context.Context) error {
	wasActive := s.opts.Active()
	every := handshakeEvery
	if !wasActive {
		every = s.w.InactiveCheck
		if every <= 0 {
			every = inactiveCheck
		}
	}
	if (s.recheck || time.Since(s.lastHandshake) >= every) && time.Now().After(s.handshakeHold) {
		if err := s.handshake(ctx); err != nil {
			if adms.IsNotFound(err) {
				return err
			}
			s.handshakeHold = time.Now().Add(s.opts.ErrorDelay)
			s.w.Log.Warn("UPAX handshake failed", "err", err)
			s.w.health.fail(err)
		} else if !wasActive && s.opts.Active() {
			s.w.Log.Info("device activated in UPAX; uploading punches")
			s.waitingLogged = false
			return s.sync(ctx)
		}
	}
	if !s.opts.Active() {
		s.logWaiting()
		return nil
	}

	sz, err := s.zc.Sizes()
	if err != nil {
		return deviceError{err}
	}
	switch {
	case sz.Records != s.sizes.Records || time.Since(s.lastFull) > fullSyncEvery:
		return s.sync(ctx)
	case s.dirty && time.Now().After(s.retryAfter):
		return s.flush(ctx)
	}
	return nil
}

// handshake refreshes the options and the device's UPAX status.
func (s *session) handshake(ctx context.Context) error {
	info := s.info()
	opts, err := s.ac.Handshake(ctx, info)
	if err != nil {
		return err
	}
	s.opts, s.lastHandshake, s.recheck = opts, time.Now(), false
	s.infoSent = s.infoKey()
	s.w.health.update(func(h *Health) { h.UPAXStatus, h.LastHandshake = s.status(), s.lastHandshake })
	return nil
}

func (s *session) status() string {
	if s.opts.Status == "" {
		return "unknown"
	}
	return s.opts.Status
}

func (s *session) logWaiting() {
	if s.waitingLogged {
		return
	}
	s.waitingLogged = true
	if s.opts.Status == adms.StatusPaused {
		s.w.Log.Warn("device is paused in UPAX; punches stay on the device until it is resumed")
	} else {
		s.w.Log.Warn("device is waiting for activation in UPAX (HR > Devices); punches stay on the device until then", "serial", s.ac.SN)
	}
}

// checkAddress notices the device being pointed at a different UPAX address.
// A new address whose UPAX record has no punches yet (stamp 0) after this
// relay uploaded to the old one is a new device record: start over, so the
// backfill setting applies to it. Otherwise it's the same record under a new
// token and the position carries over.
func (s *session) checkAddress() error {
	id := addressID(s.w.URL)
	switch {
	case s.st.Address == id:
		return nil
	case s.st.Address != "" && s.st.Initialized && s.opts.ATTLOGStamp == 0:
		s.w.Log.Info("UPAX address changed to a new device record; starting over", "backfill", s.w.Backfill)
		*s.st = State{Serial: s.st.Serial, path: s.st.path}
	case s.st.Address != "":
		s.w.Log.Info("UPAX address changed; continuing from the last uploaded punch")
	}
	s.st.Address = id
	return s.st.save()
}

func (s *session) syncIfActive(ctx context.Context) error {
	if !s.opts.Active() {
		s.logWaiting()
		return nil
	}
	return s.sync(ctx)
}

// sync downloads the device log and uploads anything new. Device errors end
// the session; upload errors are retried on a later poll.
func (s *session) sync(ctx context.Context) error {
	recs, err := s.zc.Attendance()
	if err != nil {
		return deviceError{fmt.Errorf("read attendance: %w", err)}
	}
	if s.sizes, err = s.zc.Sizes(); err != nil {
		return deviceError{err}
	}
	s.lastFull = time.Now()

	wasInit := s.st.Initialized
	idx, note := plan(recs, s.st, s.opts.ATTLOGStamp, s.w.Backfill)
	if note != "" {
		s.w.Log.Info(note)
	}
	if !wasInit {
		if err := s.st.save(); err != nil {
			return err
		}
	}
	s.recs, s.idx = recs, idx
	return s.flush(ctx)
}

// flush uploads s.idx. On failure the rest is kept for the retry.
func (s *session) flush(ctx context.Context) error {
	rest, err := s.upload(ctx, s.recs, s.idx)
	s.idx = rest
	s.w.health.update(func(h *Health) { h.Pending = len(rest) })
	if err == nil {
		s.dirty, s.recs = false, nil
		return nil
	}
	s.dirty = true
	wait := s.opts.ErrorDelay
	var se *adms.StatusError
	switch {
	case adms.IsRejected(err):
		wait = max(wait, rejectedRetry)
	case errors.As(err, &se) && se.RetryAfter > wait:
		wait = se.RetryAfter
	}
	if adms.IsUnavailable(err) {
		// UPAX is busy, or the device was paused: the handshake says which.
		s.recheck = true
	}
	s.retryAfter = time.Now().Add(wait)
	s.w.Log.Warn("upload to UPAX failed; will retry", "err", err, "pending", len(rest), "retry_in", wait)
	s.w.health.fail(err)
	if adms.IsNotFound(err) {
		return err
	}
	return nil
}

// upload sends idx in batches and returns what is left unsent.
func (s *session) upload(ctx context.Context, recs []zk.Record, idx []int) ([]int, error) {
	for len(idx) > 0 {
		n := min(batchSize, len(idx))
		batch := idx[:n]
		lines := make([]adms.AttLog, 0, n)
		for _, i := range batch {
			lines = append(lines, toAttLog(recs[i]))
		}
		last := batch[n-1]
		if err := s.ac.PushAttLog(ctx, int64(last+1), lines); err != nil {
			return idx, err
		}
		s.st.markUploaded(last+1, recs[last])
		s.st.Uploaded += int64(n)
		s.st.LastUploadAt = time.Now()
		if err := s.st.save(); err != nil {
			return idx, err
		}
		s.w.health.update(func(h *Health) { h.Uploaded, h.LastUpload, h.Pending = s.st.Uploaded, s.st.LastUploadAt, len(idx)-n })
		if n == 1 {
			r := recs[last]
			s.w.Log.Info("punch uploaded", "user", r.UserID, "time", r.Time.Format(time.DateTime), "state", r.Punch)
		} else {
			s.w.Log.Info("uploaded punches", "count", n)
		}
		idx = idx[n:]
	}
	return nil, nil
}

func toAttLog(r zk.Record) adms.AttLog {
	return adms.AttLog{PIN: r.UserID, Time: r.Time, Status: r.Punch, Verify: r.Verify}
}

// info is the classic getrequest INFO summary:
// firmware,users,fingerprints,records,ip.
func (s *session) info() string {
	return fmt.Sprintf("%s,%d,%d,%d,%s", s.firmware, s.sizes.Users, s.sizes.Fingers, s.sizes.Records, s.w.Device.Host)
}

// infoKey is the part of the summary worth re-reporting. The record count is
// left out: it changes with every punch, and UPAX records every heartbeat
// that carries INFO.
func (s *session) infoKey() string {
	return fmt.Sprintf("%s,%d,%d", s.firmware, s.sizes.Users, s.sizes.Fingers)
}

func (s *session) heartbeat(ctx context.Context) error {
	info := ""
	if s.infoKey() != s.infoSent {
		info = s.info()
	}
	cmds, err := s.ac.GetRequest(ctx, info)
	if err != nil {
		if adms.IsNotFound(err) {
			return err
		}
		s.w.Log.Warn("UPAX heartbeat failed", "err", err)
		s.w.health.fail(err)
		return nil
	}
	if info != "" {
		s.infoSent = s.infoKey()
	}
	s.w.health.update(func(h *Health) { h.LastHeartbeat = time.Now() })
	for _, c := range cmds {
		if err := s.handle(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

// handle runs one command from UPAX. UPAX only sends DATA UPDATE USERINFO,
// DATA DELETE USERINFO and INFO; CHECK is kept for other ADMS servers.
func (s *session) handle(ctx context.Context, c adms.Command) error {
	s.w.Log.Info("command from UPAX", "id", c.ID, "cmd", c.Text)
	reply := func(ret int, extra string) {
		if err := s.ac.Reply(ctx, c, ret, extra); err != nil {
			s.w.Log.Warn("failed to report command result", "id", c.ID, "err", err)
		}
	}
	switch c.Name() {
	case "CHECK":
		err := s.sync(ctx)
		if err != nil || s.dirty {
			reply(adms.ReturnError, "")
		} else {
			reply(adms.ReturnOK, "")
		}
		return err
	case "INFO":
		if sz, err := s.zc.Sizes(); err == nil {
			s.sizes = sz
		}
		reply(adms.ReturnOK, fmt.Sprintf("~SerialNumber=%s\nFirmVer=%s\nUserCount=%d\nFPCount=%d\nTransactionCount=%d\nIPAddress=%s\n",
			s.ac.SN, s.firmware, s.sizes.Users, s.sizes.Fingers, s.sizes.Records, s.w.Device.Host))
	case "DATA":
		op, f, ok := parseUserCommand(c.Text)
		if !ok {
			reply(adms.ReturnUnsupported, "")
			return nil
		}
		var err error
		if op == "UPDATE" {
			pri, _ := strconv.Atoi(f["Pri"])
			err = s.zc.SetUser(f["PIN"], f["Name"], pri)
		} else {
			err = s.zc.DeleteUser(f["PIN"])
		}
		if err != nil {
			s.w.Log.Warn("command failed", "id", c.ID, "err", err)
			reply(adms.ReturnError, "")
			if zk.IsNetErr(err) {
				return deviceError{err}
			}
			return nil
		}
		reply(adms.ReturnOK, "")
	default:
		reply(adms.ReturnUnsupported, "")
	}
	return nil
}

// parseUserCommand parses "DATA UPDATE USERINFO PIN=..\tName=..\tPri=.." and
// "DATA DELETE USERINFO PIN=..". op is UPDATE or DELETE.
func parseUserCommand(text string) (op string, fields map[string]string, ok bool) {
	for _, o := range []string{"UPDATE", "DELETE"} {
		rest, found := strings.CutPrefix(text, "DATA "+o+" USERINFO")
		if !found || (rest != "" && rest[0] != ' ' && rest[0] != '\t') {
			continue
		}
		fields = map[string]string{}
		for _, part := range strings.Split(rest, "\t") {
			if k, v, found := strings.Cut(strings.TrimSpace(part), "="); found {
				fields[k] = v
			}
		}
		return o, fields, fields["PIN"] != ""
	}
	return "", nil, false
}
