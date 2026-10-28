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
		case errors.Is(err, errRebooting):
			wait = max(wait, 30*time.Second)
			w.Log.Info("device is rebooting; reconnecting", "in", wait)
		case adms.IsNotFound(err):
			wait = max(maxB, 5*time.Minute)
			w.Log.Error("UPAX does not recognise this device address; check upax_url", "url", w.URL, "retry_in", wait)
		case errors.As(err, &se) && se.RetryAfter > 0:
			wait = max(wait, se.RetryAfter)
			w.Log.Warn("session ended", "err", err, "retry_in", wait)
		default:
			w.Log.Warn("session ended", "err", err, "retry_in", wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, maxB)
	}
}

var errRebooting = errors.New("device reboot requested by UPAX")

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

	lastFull   time.Time
	retryAfter time.Time // don't retry uploads before this
	dirty      bool      // an upload failed and must be retried
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

	ac := &adms.Client{Base: w.URL, SN: sn, HTTP: w.HTTP, UserAgent: "upax-zk-relay/" + w.Version}
	opts, err := ac.Handshake(ctx)
	if err != nil {
		return fmt.Errorf("UPAX handshake: %w", err)
	}
	st, err := loadState(w.StateDir, sn)
	if err != nil {
		return err
	}
	w.Log.Info("connected", "serial", sn, "firmware", fw, "heartbeat", opts.Delay, "upax_stamp", opts.ATTLOGStamp)

	s := &session{w: w, zc: zc, ac: ac, st: st, opts: opts, firmware: fw}
	if err := s.sync(ctx); err != nil {
		return err
	}

	poll := time.NewTicker(w.Poll)
	defer poll.Stop()
	hb := time.NewTicker(opts.Delay)
	defer hb.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
			sz, err := zc.Sizes()
			if err != nil {
				return deviceError{err}
			}
			due := sz.Records != s.sizes.Records || time.Since(s.lastFull) > fullSyncEvery ||
				(s.dirty && time.Now().After(s.retryAfter))
			if due {
				if err := s.sync(ctx); err != nil {
					return err
				}
			}
		case <-hb.C:
			if err := s.heartbeat(ctx); err != nil {
				return err
			}
		}
	}
}

// sync downloads the device log and uploads anything new. Device errors end
// the session; upload errors are retried on a later poll.
func (s *session) sync(ctx context.Context) error {
	sz, err := s.zc.Sizes()
	if err != nil {
		return deviceError{err}
	}
	recs, err := s.zc.Attendance()
	if err != nil {
		return deviceError{fmt.Errorf("read attendance: %w", err)}
	}
	s.sizes = sz
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
	if err := s.upload(ctx, recs, idx); err != nil {
		s.dirty = true
		wait := s.opts.ErrorDelay
		var se *adms.StatusError
		if errors.As(err, &se) && se.RetryAfter > wait {
			wait = se.RetryAfter
		}
		s.retryAfter = time.Now().Add(wait)
		s.w.Log.Warn("upload to UPAX failed; will retry", "err", err, "pending", len(idx), "retry_in", wait)
		if adms.IsNotFound(err) {
			return err
		}
		return nil
	}
	s.dirty = false
	return nil
}

func (s *session) upload(ctx context.Context, recs []zk.Record, idx []int) error {
	for len(idx) > 0 {
		n := min(batchSize, len(idx))
		batch := idx[:n]
		lines := make([]adms.AttLog, 0, n)
		for _, i := range batch {
			lines = append(lines, toAttLog(recs[i]))
		}
		last := batch[n-1]
		if err := s.ac.PushAttLog(ctx, int64(last+1), lines); err != nil {
			return err
		}
		s.st.markUploaded(last+1, recs[last])
		s.st.Uploaded += int64(n)
		s.st.LastUploadAt = time.Now()
		if err := s.st.save(); err != nil {
			return err
		}
		if n == 1 {
			r := recs[last]
			s.w.Log.Info("punch uploaded", "user", r.UserID, "time", r.Time.Format(time.DateTime), "state", r.Punch)
		} else {
			s.w.Log.Info("uploaded punches", "count", n)
		}
		idx = idx[n:]
	}
	return nil
}

func toAttLog(r zk.Record) adms.AttLog {
	return adms.AttLog{PIN: r.UserID, Time: r.Time, Status: r.Punch, Verify: r.Verify}
}

func (s *session) info() string {
	return fmt.Sprintf("%s,%d,%d,%d,%s", s.firmware, s.sizes.Users, s.sizes.Fingers, s.sizes.Records, s.w.Device.Host)
}

func (s *session) heartbeat(ctx context.Context) error {
	cmds, err := s.ac.GetRequest(ctx, s.info())
	if err != nil {
		if adms.IsNotFound(err) {
			return err
		}
		s.w.Log.Warn("UPAX heartbeat failed", "err", err)
		return nil
	}
	for _, c := range cmds {
		if err := s.handle(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

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
		reply(adms.ReturnOK, "")
		return err
	case "INFO":
		reply(adms.ReturnOK, fmt.Sprintf("~SerialNumber=%s\nFirmVer=%s\nUserCount=%d\nFPCount=%d\nTransactionCount=%d\nIPAddress=%s\n",
			s.ac.SN, s.firmware, s.sizes.Users, s.sizes.Fingers, s.sizes.Records, s.w.Device.Host))
	case "REBOOT":
		reply(adms.ReturnOK, "")
		_ = s.zc.Restart()
		return errRebooting
	case "DATA":
		from, to, ok := parseAttQuery(c.Text)
		if !ok {
			reply(adms.ReturnUnsupported, "")
			return nil
		}
		recs, err := s.zc.Attendance()
		if err != nil {
			reply(adms.ReturnError, "")
			return deviceError{err}
		}
		var lines []adms.AttLog
		for _, r := range recs {
			if !r.Time.Before(from) && !r.Time.After(to) {
				lines = append(lines, toAttLog(r))
			}
		}
		for len(lines) > 0 {
			n := min(batchSize, len(lines))
			if err := s.ac.PushAttLog(ctx, int64(s.st.Count), lines[:n]); err != nil {
				reply(adms.ReturnError, "")
				return nil
			}
			lines = lines[n:]
		}
		reply(adms.ReturnOK, "")
	default:
		reply(adms.ReturnUnsupported, "")
	}
	return nil
}

// parseAttQuery parses "DATA QUERY ATTLOG StartTime=...\tEndTime=...".
func parseAttQuery(text string) (from, to time.Time, ok bool) {
	rest, found := strings.CutPrefix(text, "DATA QUERY ATTLOG")
	if !found {
		return
	}
	for _, part := range strings.Split(rest, "\t") {
		k, v, _ := strings.Cut(strings.TrimSpace(part), "=")
		t, err := time.Parse(time.DateTime, strings.TrimSpace(v))
		if err != nil {
			continue
		}
		switch k {
		case "StartTime":
			from = t
		case "EndTime":
			to = t
		}
	}
	return from, to, !from.IsZero() && !to.IsZero()
}
