package relay

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/atomicfile"
	"github.com/UPAX-platform/attendance-relay/internal/device"
	"github.com/UPAX-platform/attendance-relay/internal/push"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
)

// maxPushTerminals bounds how many serials the LAN listener tracks, so a
// misbehaving LAN client can't grow the relay without limit.
const maxPushTerminals = 256

// pushTerm is a push-capable terminal sending to the LAN listener.
type pushTerm struct {
	r      *Relay
	serial string
	spool  *push.Spool
	health *TerminalHealth
	notify chan struct{}

	mu       sync.Mutex
	meta     pushMeta
	lanAddr  string
	lastSeen time.Time
	info     device.Info
	view     upaxView
	held     bool
	cmds     []upax.Command
	maxSeq   int64
	skew     *int
	batch    int
	waiting  string
}

// pushMeta is spool-<serial>/meta.json: what survives a restart.
type pushMeta struct {
	Serial   string `json:"serial"`
	Vendor   string `json:"vendor"`
	Stamp    string `json:"stamp,omitempty"`
	Uploaded int64  `json:"uploaded_total"`
}

func (p *pushTerm) metaPath() string { return filepath.Join(p.spool.Dir(), "meta.json") }

func (p *pushTerm) saveMeta() error {
	b, _ := json.MarshalIndent(p.meta, "", "  ")
	return atomicfile.Write(p.metaPath(), b, 0o600)
}

// loadSpools picks up push terminals from earlier runs, so what they left
// in the spool is uploaded even before they next call.
func (r *Relay) loadSpools() {
	dirs, _ := filepath.Glob(filepath.Join(r.cfg.StateDir(), "spool-*"))
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "meta.json"))
		if err != nil {
			continue
		}
		var m pushMeta
		if json.Unmarshal(b, &m) != nil || !push.ValidSerial(m.Serial) {
			continue
		}
		if _, err := r.pushTerminal(m.Serial, m.Vendor, &m); err != nil {
			r.log.Error("could not open a push terminal's spool", "dir", d, "err", err)
		}
	}
}

// pushTerminal returns the terminal for serial, creating it on first sight.
func (r *Relay) pushTerminal(serial, vendor string, meta *pushMeta) (*pushTerm, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.push[serial]; p != nil {
		return p, nil
	}
	if len(r.push) >= maxPushTerminals {
		return nil, nil
	}
	sp, err := push.OpenSpool(push.SpoolDir(r.cfg.StateDir(), serial))
	if err != nil {
		return nil, err
	}
	p := &pushTerm{r: r, serial: serial, spool: sp, notify: make(chan struct{}, 1)}
	if meta != nil {
		p.meta = *meta
	} else {
		p.meta = pushMeta{Serial: serial, Vendor: vendor}
		if err := p.saveMeta(); err != nil {
			return nil, err
		}
	}
	p.health = newTerminalHealth(PushHealthPath(r.cfg.StateDir(), serial))
	p.health.update(func(h *TerminalHealth) {
		h.Name, h.Serial, h.Vendor, h.Connection = serial, serial, p.meta.Vendor, upax.ConnPush
		h.Uploaded, h.Spooled = p.meta.Uploaded, sp.Pending()
	})
	r.push[serial] = p
	if ctx := r.ctx; ctx != nil && ctx.Err() == nil {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			p.uploader(ctx)
		}()
	}
	return p, nil
}

func (r *Relay) knownPush(serial string) *pushTerm {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.push[serial]
}

// ---- push.Hub -------------------------------------------------------------------

func (r *Relay) Seen(c push.Contact) {
	isNew := r.knownPush(c.Serial) == nil
	p, err := r.pushTerminal(c.Serial, c.Vendor, nil)
	if err != nil {
		r.log.Error("could not track a push terminal", "serial", c.Serial, "err", err)
		return
	}
	if p == nil {
		return // over the limit
	}
	p.mu.Lock()
	p.lanAddr, p.lastSeen = c.LANAddr, time.Now()
	if c.Info != nil {
		p.info = *c.Info
	}
	in := p.info
	p.mu.Unlock()
	p.health.update(func(h *TerminalHealth) {
		h.Addr, h.Reachable, h.LastContact, h.Firmware = c.LANAddr, true, time.Now(), in.Firmware
	})
	if isNew {
		r.log.Info("push terminal connected to the relay", "serial", c.Serial, "lan_addr", c.LANAddr)
		r.kick(true)
	}
}

func (r *Relay) Accept(serial string) (bool, time.Duration) {
	_, errDelay := r.delays()
	p := r.knownPush(serial)
	if p == nil {
		return false, errDelay
	}
	open, why := p.gate()
	if !open {
		p.logWaiting(why)
	}
	return open, errDelay
}

func (r *Relay) Store(serial string, punches []device.Punch, stamp string) error {
	p := r.knownPush(serial)
	if p == nil {
		return os.ErrNotExist
	}
	if err := p.spool.Append(punches); err != nil {
		return err
	}
	p.mu.Lock()
	if stamp != "" {
		p.meta.Stamp = stamp
	}
	err := p.saveMeta()
	p.mu.Unlock()
	p.estimateSkew(punches)
	p.health.update(func(h *TerminalHealth) { h.Spooled = p.spool.Pending() })
	select {
	case p.notify <- struct{}{}:
	default:
	}
	return err
}

func (r *Relay) Stamp(serial string) string {
	if p := r.knownPush(serial); p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.meta.Stamp
	}
	return ""
}

func (r *Relay) Commands(serial string) []upax.Command {
	p := r.knownPush(serial)
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cmds := p.cmds
	p.cmds = nil
	return cmds
}

func (r *Relay) Result(serial string, seq int64, ret int, cmd string) {
	r.addResult(upax.Result{Serial: serial, Seq: seq, Return: ret, Cmd: cmd})
}

func (r *Relay) Clock(serial string) push.Clock {
	p := r.knownPush(serial)
	if p == nil {
		return push.Clock{}
	}
	v := p.currentView()
	loc, ok := zone(v.timezone)
	off, measured := r.api.ClockOffset()
	// A relay whose own clock is off doesn't steer terminal clocks.
	if !v.clockSync || !ok || !measured || abs(off) > clockThreshold {
		return push.Clock{}
	}
	return push.Clock{Sync: true, Zone: loc}
}

func (r *Relay) Now() time.Time { return r.api.Now() }

func (r *Relay) Delays() (poll, retry time.Duration) { return r.delays() }

// ---- terminal --------------------------------------------------------------------

func (p *pushTerm) setView(v upaxView, cmds []upax.Command) {
	p.mu.Lock()
	p.view, p.held = v, false
	for _, c := range cmds {
		if c.Seq > p.maxSeq {
			p.cmds = append(p.cmds, c)
			p.maxSeq = c.Seq
		}
	}
	p.mu.Unlock()
	p.health.update(func(h *TerminalHealth) {
		h.Registered, h.DeviceID, h.ClockSync = v.registered, v.deviceID, v.clockSync
		h.UPAXStatus = upaxStatusWord(p.r.relayStatus(), v, false)
	})
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

func (p *pushTerm) currentView() upaxView {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.view
}

func (p *pushTerm) gate() (bool, string) {
	p.mu.Lock()
	v, held := p.view, p.held
	p.mu.Unlock()
	return gate(p.r.relayStatus(), v, held)
}

func (p *pushTerm) logWaiting(why string) {
	p.mu.Lock()
	if p.waiting == why {
		p.mu.Unlock()
		return
	}
	p.waiting = why
	p.mu.Unlock()
	switch why {
	case holdUnregistered:
		p.r.log.Warn("push terminal isn't added in UPAX yet: add it from the relay's discovered terminals; its punches stay on it until then", "serial", p.serial)
	case holdTerminal:
		p.r.log.Warn("push terminal isn't active in UPAX; its punches stay on it until it is", "serial", p.serial)
	}
}

func (p *pushTerm) report() upax.TerminalReport {
	poll, _ := p.r.delays()
	p.mu.Lock()
	defer p.mu.Unlock()
	reachable := !p.lastSeen.IsZero() && time.Since(p.lastSeen) < max(3*poll, 2*time.Minute)
	return upax.TerminalReport{
		Serial: p.serial, Vendor: p.meta.Vendor, LANAddr: p.lanAddr, Connection: upax.ConnPush,
		Reachable: reachable, Firmware: p.info.Firmware,
		Users: p.info.Users, Fingerprints: p.info.Fingerprints, Records: p.info.Records,
		ClockSkewS: p.skew, ClockSkewEstimated: p.skew != nil,
	}
}

// estimateSkew guesses a push terminal's clock error from a real-time
// upload: its newest punch was made moments before it arrived.
func (p *pushTerm) estimateSkew(punches []device.Punch) {
	if len(punches) == 0 || len(punches) > 3 {
		return // a backlog says nothing about the clock
	}
	newest := punches[0].Time
	for _, x := range punches[1:] {
		if x.Time.After(newest) {
			newest = x.Time
		}
	}
	loc, _ := zone(p.currentView().timezone)
	skew := newest.Sub(wall(p.r.api.Now().In(loc))).Round(time.Second)
	if abs(skew) > 12*time.Hour {
		return
	}
	n := int(skew / time.Second)
	p.mu.Lock()
	p.skew = &n
	p.mu.Unlock()
	p.health.update(func(h *TerminalHealth) { h.ClockSkewS = &n })
}

// uploader sends the spool to UPAX whenever the terminal may upload.
func (p *pushTerm) uploader(ctx context.Context) {
	tick := time.NewTicker(time.Duration(p.r.cfg.PollInterval))
	defer tick.Stop()
	var retryAfter time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-p.notify:
		}
		if open, _ := p.gate(); !open || time.Now().Before(retryAfter) || p.spool.Pending() == 0 {
			continue
		}
		if wait, err := p.safeDrain(ctx); err != nil {
			retryAfter = time.Now().Add(wait)
			p.r.log.Warn("upload of spooled punches failed; will retry", "serial", p.serial, "err", err, "spooled", p.spool.Pending(), "retry_in", wait)
			p.health.fail(err)
		}
	}
}

func (p *pushTerm) safeDrain(ctx context.Context) (wait time.Duration, err error) {
	defer func() {
		if err != nil && wait <= 0 {
			wait = time.Minute
		}
	}()
	defer recovered(p.r.log.With("serial", p.serial), "push upload", &err)
	return p.drain(ctx)
}

func (p *pushTerm) drain(ctx context.Context) (time.Duration, error) {
	for {
		name, punches, ok, err := p.spool.Oldest()
		if err != nil {
			return time.Minute, err
		}
		if !ok {
			return 0, nil
		}
		err = p.r.upload(ctx, p.serial, punches, &p.batch, func(n int) error {
			if err := p.spool.Done(name, n); err != nil {
				return err
			}
			p.mu.Lock()
			p.meta.Uploaded += int64(n)
			_ = p.saveMeta()
			up := p.meta.Uploaded
			p.mu.Unlock()
			p.health.update(func(h *TerminalHealth) {
				h.Uploaded, h.LastUpload, h.Spooled = up, time.Now(), p.spool.Pending()
			})
			p.r.log.Info("uploaded punches from a push terminal", "serial", p.serial, "count", n)
			return nil
		})
		if err != nil {
			out := p.r.uploadFailed(err)
			if out.holdTerminal {
				p.mu.Lock()
				p.held = true
				p.mu.Unlock()
				p.r.kick(true)
			}
			return out.wait, err
		}
	}
}
