// Package adms is the server side of ZKTeco's ADMS ("iclock") push
// protocol, as the relay's LAN listener speaks it to push-capable ZKTeco
// terminals:
//
//	GET  /iclock/cdata?SN=…&options=all     handshake: the relay's options
//	POST /iclock/cdata?SN=…&table=ATTLOG    punches: spooled, then "OK: n"
//	GET  /iclock/getrequest?SN=…            poll: queued commands
//	POST /iclock/devicecmd?SN=…             command results
//
// Every other table and upload (operation log, photos, fingerprint and face
// templates) is answered OK and discarded unread: biometric data never
// leaves the LAN and is never logged.
package adms

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/device"
	"github.com/UPAX-platform/attendance-relay/internal/push"
)

// Vendor is the vendor these terminals report as.
const Vendor = "zkteco"

// Prefix is the path the handler is mounted on.
const Prefix = "/iclock/"

// maxUpload caps one ATTLOG upload; terminals send far less per request.
const maxUpload = 4 << 20

// Handler serves the protocol.
type Handler struct {
	Hub push.Hub
	Log *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Date", h.Hub.Now().UTC().Format(http.TimeFormat))
	w.Header().Set("Content-Type", "text/plain")
	sn := r.URL.Query().Get("SN")
	if !push.ValidSerial(sn) {
		http.Error(w, "unknown device", http.StatusBadRequest)
		return
	}
	lan, _, _ := net.SplitHostPort(r.RemoteAddr)
	switch {
	case r.URL.Path == Prefix+"cdata" && r.Method == http.MethodGet:
		h.Hub.Seen(push.Contact{Serial: sn, Vendor: Vendor, LANAddr: lan, Info: parseInfo(r.URL.Query().Get("INFO"))})
		h.handshake(w, sn)
	case r.URL.Path == Prefix+"cdata" && r.Method == http.MethodPost:
		h.Hub.Seen(push.Contact{Serial: sn, Vendor: Vendor, LANAddr: lan})
		if !strings.EqualFold(r.URL.Query().Get("table"), "ATTLOG") {
			discard(w, r)
			return
		}
		h.attlog(w, r, sn)
	case r.URL.Path == Prefix+"getrequest" && r.Method == http.MethodGet:
		h.Hub.Seen(push.Contact{Serial: sn, Vendor: Vendor, LANAddr: lan, Info: parseInfo(r.URL.Query().Get("INFO"))})
		cmds := h.Hub.Commands(sn)
		if len(cmds) == 0 {
			fmt.Fprint(w, "OK")
			return
		}
		for _, c := range cmds {
			fmt.Fprintf(w, "C:%d:%s\n", c.Seq, c.Line)
		}
	case r.URL.Path == Prefix+"devicecmd" && r.Method == http.MethodPost:
		h.Hub.Seen(push.Contact{Serial: sn, Vendor: Vendor, LANAddr: lan})
		h.results(r, sn)
		fmt.Fprint(w, "OK")
	case r.URL.Path == Prefix+"fdata" || r.URL.Path == Prefix+"ping":
		discard(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) handshake(w http.ResponseWriter, sn string) {
	poll, retry := h.Hub.Delays()
	stamp := h.Hub.Stamp(sn)
	if stamp == "" {
		stamp = "0"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "GET OPTION FROM: %s\n", sn)
	fmt.Fprintf(&b, "ATTLOGStamp=%s\n", stamp)
	// The operation log and photos are never wanted: a high stamp and the
	// TransFlag keep the terminal from sending them.
	b.WriteString("OPERLOGStamp=9999\nATTPHOTOStamp=None\n")
	fmt.Fprintf(&b, "ErrorDelay=%d\nDelay=%d\n", seconds(retry), seconds(poll))
	b.WriteString("TransTimes=00:00;14:05\nTransInterval=1\nTransFlag=TransData AttLog\n")
	if c := h.Hub.Clock(sn); c.Sync && c.Zone != nil {
		fmt.Fprintf(&b, "TimeZone=%s\n", zoneOffset(h.Hub.Now(), c.Zone))
	}
	b.WriteString("Realtime=1\nEncrypt=None\n")
	fmt.Fprint(w, b.String())
}

func (h *Handler) attlog(w http.ResponseWriter, r *http.Request, sn string) {
	if ok, retry := h.Hub.Accept(sn); !ok {
		// Not registered or not active in UPAX: refuse, so the punches stay
		// on the terminal and come again later, as UPAX does for a pending
		// direct device.
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxUpload))
		w.Header().Set("Retry-After", strconv.Itoa(seconds(retry)))
		http.Error(w, "not active", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxUpload+1))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > maxUpload {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	punches, bad := ParseAttLog(string(body))
	if bad > 0 {
		h.Log.Warn("push terminal sent attendance lines the relay can't read; they were skipped", "serial", sn, "lines", bad)
	}
	if err := h.Hub.Store(sn, punches, r.URL.Query().Get("Stamp")); err != nil {
		h.Log.Error("could not spool punches from a push terminal; it will send them again", "serial", sn, "err", err)
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, "OK: %d", len(punches)+bad)
}

func (h *Handler) results(r *http.Request, sn string) {
	sc := bufio.NewScanner(io.LimitReader(r.Body, 1<<20))
	for sc.Scan() {
		q, err := url.ParseQuery(strings.TrimSpace(sc.Text()))
		if err != nil {
			continue
		}
		seq, err := strconv.ParseInt(q.Get("ID"), 10, 64)
		if err != nil {
			continue
		}
		ret, _ := strconv.Atoi(q.Get("Return"))
		h.Hub.Result(sn, seq, ret, q.Get("CMD"))
	}
}

// discard answers OK without reading what was sent (draining it only so
// the connection can be reused).
func discard(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<20))
	fmt.Fprint(w, "OK")
}

// ParseAttLog reads ATTLOG lines: PIN, time, state, verify, workcode, … tab
// separated. It returns the punches and the number of unreadable lines.
func ParseAttLog(body string) (punches []device.Punch, bad int) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			bad++
			continue
		}
		t, err := time.Parse(time.DateTime, strings.TrimSpace(f[1]))
		pin := strings.TrimSpace(f[0])
		if err != nil || pin == "" {
			bad++
			continue
		}
		p := device.Punch{PIN: pin, Time: t, WorkCode: "0"}
		if len(f) > 2 {
			p.State, _ = strconv.Atoi(strings.TrimSpace(f[2]))
		}
		if len(f) > 3 {
			p.Verify, _ = strconv.Atoi(strings.TrimSpace(f[3]))
		}
		if len(f) > 4 && strings.TrimSpace(f[4]) != "" {
			p.WorkCode = strings.TrimSpace(f[4])
		}
		punches = append(punches, p)
	}
	return punches, bad
}

// parseInfo reads the INFO summary: firmware,users,fingerprints,records,ip,…
func parseInfo(s string) *device.Info {
	if s == "" {
		return nil
	}
	f := strings.Split(s, ",")
	in := &device.Info{Firmware: strings.TrimSpace(f[0])}
	n := func(i int) int {
		if i >= len(f) {
			return 0
		}
		v, _ := strconv.Atoi(strings.TrimSpace(f[i]))
		return v
	}
	in.Users, in.Fingerprints, in.Records = n(1), n(2), n(3)
	return in
}

// zoneOffset is the zone's UTC offset as ADMS TimeZone: whole hours, or
// minutes when the zone has a fractional offset (+5:30 → 330). Which form a
// model honours is a hardware acceptance item.
func zoneOffset(now time.Time, loc *time.Location) string {
	_, off := now.In(loc).Zone()
	if off%3600 == 0 {
		return strconv.Itoa(off / 3600)
	}
	return strconv.Itoa(off / 60)
}

func seconds(d time.Duration) int { return max(1, int(d/time.Second)) }
