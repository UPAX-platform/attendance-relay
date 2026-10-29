package relay

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/config"
	"github.com/UPAX-platform/attendance-relay/internal/device"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
)

const terminalTimeout = 15 * time.Second

// errReconnect ends a session so the next one starts at once (after the
// relay set a new comm key).
var errReconnect = errors.New("reconnecting with the new comm key")

type deviceError struct{ err error }

func (e deviceError) Error() string { return "terminal: " + e.err.Error() }
func (e deviceError) Unwrap() error { return e.err }

// reader pulls one configured terminal.
type reader struct {
	r      *Relay
	t      config.Terminal
	addr   string
	drv    device.Driver
	log    *slog.Logger
	health *TerminalHealth
	notify chan struct{}

	mu        sync.Mutex
	info      device.Info
	reachable bool
	lastErr   string
	skew      *int
	view      upaxView
	held      bool
	cmds      []upax.Command
	maxSeq    int64
	// keyJustSet: the relay set a comm key on the terminal and the next
	// dial proves it. keyFailed: it didn't take; don't try again this run.
	keyJustSet, keyFailed bool
}

func newReader(r *Relay, t config.Terminal) (*reader, error) {
	drv, err := device.Lookup(t.Vendor)
	if err != nil {
		return nil, err
	}
	rd := &reader{r: r, t: t, addr: t.Addr(), drv: drv, log: r.log.With("terminal", t.Label()), notify: make(chan struct{}, 1)}
	rd.health = newTerminalHealth(TerminalHealthPath(r.cfg.StateDir(), rd.addr))
	rd.health.update(func(h *TerminalHealth) {
		h.Name, h.Addr, h.Vendor, h.Connection = t.Label(), rd.addr, t.VendorName(), upax.ConnPull
	})
	return rd, nil
}

func (rd *reader) serial() string {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	return rd.info.Serial
}

// setView takes the latest sync's word on this terminal.
func (rd *reader) setView(v upaxView, cmds []upax.Command) {
	rd.mu.Lock()
	rd.view, rd.held = v, false
	for _, c := range cmds {
		// UPAX hands commands out in ascending order; one seen is done.
		if c.Seq > rd.maxSeq {
			rd.cmds = append(rd.cmds, c)
			rd.maxSeq = c.Seq
		}
	}
	rd.mu.Unlock()
	rd.health.update(func(h *TerminalHealth) {
		h.Registered, h.DeviceID, h.ClockSync = v.registered, v.deviceID, v.clockSync
		h.UPAXStatus = upaxStatusWord(rd.r.relayStatus(), v, false)
	})
	select {
	case rd.notify <- struct{}{}:
	default:
	}
}

func (rd *reader) hold() {
	rd.mu.Lock()
	rd.held = true
	rd.mu.Unlock()
	rd.r.kick(true)
}

func (rd *reader) gate() (bool, string) {
	rd.mu.Lock()
	v, held := rd.view, rd.held
	rd.mu.Unlock()
	return gate(rd.r.relayStatus(), v, held)
}

func (rd *reader) currentView() upaxView {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	return rd.view
}

func (rd *reader) report() (upax.TerminalReport, bool) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if rd.info.Serial == "" {
		return upax.TerminalReport{}, false
	}
	return upax.TerminalReport{
		Serial: rd.info.Serial, Vendor: rd.t.VendorName(), LANAddr: rd.t.Host, Connection: upax.ConnPull,
		Reachable: rd.reachable, Model: rd.info.Model, Firmware: rd.info.Firmware,
		Users: rd.info.Users, Fingerprints: rd.info.Fingerprints, Records: rd.info.Records,
		ClockSkewS: rd.skew, LastError: rd.lastErr,
	}, true
}

func (rd *reader) fail(err error) {
	rd.mu.Lock()
	rd.lastErr = err.Error()
	rd.mu.Unlock()
	rd.health.fail(err)
}

func (rd *reader) setReachable(ok bool) {
	rd.mu.Lock()
	rd.reachable = ok
	if ok {
		rd.lastErr = ""
	}
	rd.mu.Unlock()
	rd.health.update(func(h *TerminalHealth) {
		h.Reachable = ok
		if ok {
			h.LastContact = time.Now()
		}
	})
}

func (rd *reader) setInfo(in device.Info) (serialChanged bool) {
	rd.mu.Lock()
	serialChanged = rd.info.Serial != in.Serial
	rd.info = in
	rd.mu.Unlock()
	rd.health.update(func(h *TerminalHealth) {
		h.Serial, h.Model, h.Firmware, h.LastContact = in.Serial, in.Model, in.Firmware, time.Now()
	})
	return serialChanged
}

// run keeps a session alive, reconnecting with backoff, until ctx ends.
func (rd *reader) run(ctx context.Context) {
	minB, maxB := rd.r.o.MinBackoff, rd.r.o.MaxBackoff
	backoff := minB
	for {
		started := time.Now()
		err := rd.safeSession(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errReconnect) {
			continue
		}
		rd.setReachable(false)
		if time.Since(started) > time.Minute {
			backoff = minB
		}
		wait := backoff
		switch {
		case errors.Is(err, device.ErrUnauthorized):
			wait = maxB
			rd.log.Error("the terminal rejected the comm key in the config; set the right one (Menu › Comm. › PC Connection on the terminal) and restart the relay", "retry_in", wait)
		case err != nil:
			rd.log.Warn("terminal session ended", "err", err, "retry_in", wait)
		}
		if err != nil {
			rd.fail(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, maxB)
	}
}

// safeSession is session, with a panic (a terminal answering something the
// driver didn't expect) ending the session instead of the relay.
func (rd *reader) safeSession(ctx context.Context) (err error) {
	defer recovered(rd.log, "terminal session", &err)
	return rd.session(ctx)
}

// session is one connection to the terminal.
type session struct {
	rd      *reader
	conn    device.Conn
	st      *State
	records int // the record count at the last download (-1 = none yet)

	lastFull   time.Time
	retryAfter time.Time
	dirty      bool
	batch      batching
	// recs/idx are the log and the indexes still to upload, kept after a
	// failed upload so the retry doesn't download the log again.
	recs []device.Punch
	idx  []int

	waiting      string // the hold reason last logged
	clockChecked time.Time
	clockSet     time.Time
	clockView    upaxView
	clockWarned  bool
}

func (rd *reader) dial(ctx context.Context) (device.Conn, error) {
	pw := rd.r.cfg.Password(rd.addr)
	target := rd.t.Target()
	target.Password = pw
	conn, err := rd.drv.Dial(ctx, target, terminalTimeout)
	rd.mu.Lock()
	justSet := rd.keyJustSet
	if err == nil || errors.Is(err, device.ErrUnauthorized) {
		// The terminal answered: the key is settled either way.
		rd.keyJustSet = false
	}
	rd.mu.Unlock()
	if errors.Is(err, device.ErrUnauthorized) && justSet {
		// The terminal didn't take the comm key the relay set: if it still
		// answers without one, go back to that and stop managing it.
		target.Password = 0
		if c, err0 := rd.drv.Dial(ctx, target, terminalTimeout); err0 == nil {
			rd.mu.Lock()
			rd.keyFailed = true
			rd.mu.Unlock()
			if err := rd.r.cfg.SetPassword(rd.addr, 0); err != nil {
				rd.log.Error("could not save the config", "err", err)
			}
			rd.log.Warn("the terminal did not accept the comm key the relay set; it keeps working without one")
			rd.fail(errors.New("the terminal did not accept a managed comm key"))
			return c, nil
		}
	}
	return conn, err
}

func (rd *reader) session(ctx context.Context) error {
	conn, err := rd.dial(ctx)
	if err != nil {
		if errors.Is(err, device.ErrUnauthorized) {
			return err
		}
		return deviceError{err}
	}
	defer conn.Close()

	info, err := conn.Info()
	if err != nil {
		return deviceError{fmt.Errorf("read terminal info: %w", err)}
	}
	if info.Serial == "" {
		return deviceError{errors.New("the terminal reported an empty serial number")}
	}
	if rd.setInfo(info) {
		rd.r.kick(true) // report it to UPAX now rather than at the next sync
	}
	rd.setReachable(true)
	rd.log.Info("connected", "serial", info.Serial, "model", info.Model, "firmware", info.Firmware)

	if err := rd.manageCommKey(conn); err != nil {
		return err
	}

	st, err := loadState(rd.r.cfg.StateDir(), info.Serial)
	if errors.Is(err, errCorruptState) {
		rd.log.Warn(err.Error(), "serial", info.Serial)
	} else if err != nil {
		return err
	}
	rd.health.update(func(h *TerminalHealth) { h.Uploaded = st.Uploaded })
	s := &session{rd: rd, conn: conn, st: st, records: -1}

	poll := time.NewTicker(time.Duration(rd.r.cfg.PollInterval))
	defer poll.Stop()
	for {
		if err := s.tick(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
		case <-rd.notify:
		}
	}
}

// manageCommKey sets a random comm key on a terminal that has none, when
// the relay manages comm keys. The key is saved to the config before the
// terminal gets it, so the relay can always log in again.
func (rd *reader) manageCommKey(conn device.Conn) error {
	rd.mu.Lock()
	failed := rd.keyFailed
	rd.mu.Unlock()
	if failed || !rd.r.cfg.ManagesCommKey(rd.t) || rd.r.cfg.Password(rd.addr) != 0 {
		rd.health.update(func(h *TerminalHealth) {
			if rd.r.cfg.ManagesCommKey(rd.t) && rd.r.cfg.Password(rd.addr) != 0 {
				h.CommKey = "managed"
			}
		})
		return nil
	}
	ks, ok := conn.(device.CommKeySetter)
	if !ok {
		return nil
	}
	lo, hi := ks.CommKeyRange()
	n, err := rand.Int(rand.Reader, big.NewInt(int64(hi-lo)+1))
	if err != nil {
		return err
	}
	key := lo + uint32(n.Int64())
	if err := rd.r.cfg.SetPassword(rd.addr, key); err != nil {
		rd.log.Error("could not save a comm key to the config; leaving the terminal without one", "err", err)
		return nil
	}
	if err := ks.SetCommKey(key); err != nil {
		if device.IsNetErr(err) {
			// The terminal may have taken the key before the connection
			// broke: keep it, and let the next dial try it and no key.
			rd.mu.Lock()
			rd.keyJustSet = true
			rd.mu.Unlock()
			rd.log.Warn("lost the terminal while setting a comm key; reconnecting to see whether it took", "err", err)
			rd.fail(fmt.Errorf("set comm key: %w", err))
			return deviceError{err}
		}
		_ = rd.r.cfg.SetPassword(rd.addr, 0)
		rd.log.Warn("the terminal refused a comm key; it keeps working without one", "err", err)
		rd.fail(fmt.Errorf("set comm key: %w", err))
		return nil
	}
	rd.mu.Lock()
	rd.keyJustSet = true
	rd.mu.Unlock()
	rd.log.Info("set a comm key on the terminal (see it with: upax-relay show-comm-key); reconnecting with it")
	return errReconnect
}

// tick is one poll: commands, clock, then punches if the terminal may upload.
func (s *session) tick(ctx context.Context) error {
	if err := s.commands(); err != nil {
		return err
	}
	if err := s.clock(); err != nil {
		return err
	}
	open, why := s.rd.gate()
	if !open {
		s.logWaiting(why)
		return nil
	}
	if s.waiting != "" {
		s.rd.log.Info("terminal is active in UPAX; uploading punches")
		s.waiting = ""
	}
	if err := s.checkDeviceID(); err != nil {
		return err
	}
	n, err := s.conn.RecordCount()
	if err != nil {
		return deviceError{err}
	}
	s.rd.mu.Lock()
	s.rd.info.Records = n
	s.rd.mu.Unlock()
	s.rd.health.update(func(h *TerminalHealth) { h.LastContact = time.Now() })
	switch {
	case n != s.records || time.Since(s.lastFull) > s.rd.r.o.FullCheck:
		return s.sync(ctx)
	case s.dirty && time.Now().After(s.retryAfter):
		return s.flush(ctx)
	}
	return nil
}

func (s *session) logWaiting(why string) {
	if s.waiting == why {
		return
	}
	s.waiting = why
	serial := s.rd.serial()
	switch why {
	case holdUnregistered:
		s.rd.log.Warn("terminal isn't added in UPAX yet: add it from the relay's discovered terminals in UPAX; punches stay on the terminal until then", "serial", serial)
	case holdTerminal:
		s.rd.log.Warn("terminal isn't active in UPAX (pending or paused); punches stay on the terminal until it is", "serial", serial, "status", s.rd.currentView().status)
	case holdRefused:
		s.rd.log.Warn("UPAX says the terminal isn't active; holding its punches until the next sync", "serial", serial)
	}
}

// checkDeviceID notices the terminal being re-added in UPAX as a new device
// record: its log is uploaded to the new record from the start, as set by
// backfill.
func (s *session) checkDeviceID() error {
	id := s.rd.currentView().deviceID
	switch {
	case id == "" || s.st.DeviceID == id:
		return nil
	case s.st.DeviceID != "" && s.st.Initialized:
		s.rd.log.Info("terminal is a new device record in UPAX; uploading its log again", "backfill", s.rd.r.cfg.Backfill)
		*s.st = State{Serial: s.st.Serial, path: s.st.path}
		s.recs, s.idx, s.dirty = nil, nil, false
		s.records = -1
	}
	s.st.DeviceID = id
	return s.st.save()
}

// sync downloads the log and uploads anything new. Terminal errors end the
// session; upload errors are retried on a later poll.
func (s *session) sync(ctx context.Context) error {
	// Count first: a punch made during the download then shows up as a
	// new record at the next poll instead of waiting for the full check.
	info, err := s.conn.Info()
	if err != nil {
		return deviceError{err}
	}
	recs, err := s.conn.Punches()
	if err != nil {
		return deviceError{fmt.Errorf("read attendance: %w", err)}
	}
	s.rd.setInfo(info)
	s.records = info.Records
	s.lastFull = time.Now()

	wasInit := s.st.Initialized
	idx, note := plan(recs, s.st, s.rd.r.cfg.Backfill)
	if note != "" {
		s.rd.log.Info(note)
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
	punches := make([]device.Punch, len(s.idx))
	for i, j := range s.idx {
		punches[i] = s.recs[j]
	}
	sent := 0
	err := s.rd.r.upload(ctx, s.rd.serial(), punches, &s.batch, func(n int, refused bool) error {
		last := s.idx[sent+n-1]
		s.st.markUploaded(last+1, s.recs[last])
		if refused {
			// The punch stays on the terminal; moving past it is all.
			if err := s.st.save(); err != nil {
				return err
			}
			sent += n
			s.rd.health.update(func(h *TerminalHealth) { h.Refused += int64(n); h.Pending = len(s.idx) - sent })
			return nil
		}
		s.st.Uploaded += int64(n)
		s.st.LastUploadAt = time.Now()
		if err := s.st.save(); err != nil {
			return err
		}
		sent += n
		s.rd.health.update(func(h *TerminalHealth) {
			h.Uploaded, h.LastUpload, h.Pending = s.st.Uploaded, s.st.LastUploadAt, len(s.idx)-sent
		})
		if n == 1 {
			r := s.recs[last]
			s.rd.log.Info("punch uploaded", "user", r.PIN, "time", r.Time.Format(time.DateTime), "state", r.State)
		} else {
			s.rd.log.Info("uploaded punches", "count", n)
		}
		return nil
	})
	s.idx = s.idx[sent:]
	s.rd.health.update(func(h *TerminalHealth) { h.Pending = len(s.idx) })
	if err == nil {
		s.dirty, s.recs = false, nil
		return nil
	}
	s.dirty = true
	out := s.rd.r.uploadFailed(err)
	if out.holdTerminal {
		s.rd.hold()
	}
	s.retryAfter = time.Now().Add(out.wait)
	s.rd.log.Warn("upload to UPAX failed; will retry", "err", err, "pending", len(s.idx), "retry_in", out.wait)
	s.rd.fail(err)
	return nil
}

// commands runs what UPAX queued for this terminal and queues the results.
func (s *session) commands() error {
	s.rd.mu.Lock()
	cmds := s.rd.cmds
	s.rd.cmds = nil
	s.rd.mu.Unlock()
	serial := s.rd.serial()
	for i, c := range cmds {
		s.rd.log.Info("command from UPAX", "seq", c.Seq, "cmd", commandName(c.Line))
		ret, err := runCommand(s.conn, c.Line)
		if err != nil {
			s.rd.log.Warn("command failed", "seq", c.Seq, "err", err)
		}
		if commandName(c.Line) == "INFO" && err == nil {
			if in, ierr := s.conn.Info(); ierr == nil {
				s.rd.setInfo(in)
				s.rd.r.kick(true)
			}
		}
		s.rd.r.addResult(upax.Result{Serial: serial, Seq: c.Seq, Return: ret, Cmd: commandName(c.Line)})
		if err != nil && device.IsNetErr(err) {
			// The rest wait for the next session.
			s.rd.mu.Lock()
			s.rd.cmds = append(cmds[i+1:], s.rd.cmds...)
			s.rd.mu.Unlock()
			return deviceError{err}
		}
	}
	return nil
}

func commandName(line string) string {
	name, _, _ := strings.Cut(line, " ")
	return name
}

// runCommand runs one command in UPAX's command language on a pulled
// terminal. UPAX sends DATA UPDATE USERINFO, DATA DELETE USERINFO and INFO;
// anything else is answered "unsupported".
func runCommand(conn device.Conn, line string) (ret int, err error) {
	switch commandName(line) {
	case "INFO":
		// The terminal summary travels with every sync.
		return upax.ReturnOK, nil
	case "DATA":
		op, f, ok := parseUserCommand(line)
		if !ok {
			return upax.ReturnUnsupported, nil
		}
		if op == "UPDATE" {
			pri, _ := strconv.Atoi(f["Pri"])
			err = conn.SetUser(f["PIN"], f["Name"], pri)
		} else {
			err = conn.DeleteUser(f["PIN"])
		}
		if err != nil {
			return upax.ReturnError, err
		}
		return upax.ReturnOK, nil
	}
	return upax.ReturnUnsupported, nil
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

// clock measures the terminal's clock against its UPAX zone, and pins it
// there when UPAX says so (clock_sync), at most hourly. The relay's own
// clock must agree with UPAX's first: a relay with a wrong clock would
// spread its error.
func (s *session) clock() error {
	v := s.rd.currentView()
	changed := v.clockSync != s.clockView.clockSync || v.timezone != s.clockView.timezone
	if !changed && time.Since(s.clockChecked) < s.rd.r.o.FullCheck {
		return nil
	}
	s.clockView, s.clockChecked = v, time.Now()
	t, err := s.conn.Clock()
	if err != nil {
		if device.IsNetErr(err) {
			return deviceError{err}
		}
		return nil // some models can't tell; nothing to report
	}
	loc, _ := zone(v.timezone)
	now := s.rd.r.api.Now()
	skew := t.Sub(wall(now.In(loc))).Round(time.Second)
	s.setSkew(skew)
	if !v.clockSync || !v.known || !v.registered || abs(skew) <= clockThreshold || time.Since(s.clockSet) < clockSetEvery {
		return nil
	}
	if off, measured := s.rd.r.api.ClockOffset(); !measured || abs(off) > clockThreshold {
		if !s.clockWarned {
			s.clockWarned = true
			s.rd.log.Warn("not setting the terminal clock: this computer's clock disagrees with UPAX; fix it (enable automatic time) first", "offset", off)
			s.rd.fail(errors.New("relay clock is off; terminal clock not set"))
		}
		return nil
	}
	s.clockWarned = false
	if _, ok := zone(v.timezone); !ok {
		s.rd.log.Warn("not setting the terminal clock: UPAX gave a zone this computer doesn't know", "timezone", v.timezone)
		return nil
	}
	target := wall(now.In(loc))
	if err := s.conn.SetClock(target); err != nil {
		s.rd.log.Warn("could not set the terminal clock", "err", err)
		s.rd.fail(fmt.Errorf("set clock: %w", err))
		if device.IsNetErr(err) {
			return deviceError{err}
		}
		return nil
	}
	s.clockSet = time.Now()
	s.rd.log.Info("terminal clock set to the UPAX zone", "timezone", v.timezone, "was", t.Format(time.DateTime), "now", target.Format(time.DateTime))
	at := s.clockSet
	s.rd.health.update(func(h *TerminalHealth) { h.ClockSetAt = &at })
	s.setSkew(0)
	return nil
}

func (s *session) setSkew(d time.Duration) {
	n := int(d / time.Second)
	s.rd.mu.Lock()
	s.rd.skew = &n
	s.rd.mu.Unlock()
	s.rd.health.update(func(h *TerminalHealth) { h.ClockSkewS = &n })
}
