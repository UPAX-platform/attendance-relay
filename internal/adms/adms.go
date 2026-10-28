// Package adms speaks the ZKTeco ADMS / "iclock" push protocol, which is
// what UPAX exposes at each device address. The relay uses it to appear to
// the platform exactly like a push-capable terminal.
//
//	GET  {base}/iclock/cdata?SN=..&options=all   handshake, returns options
//	POST {base}/iclock/cdata?SN=..&table=ATTLOG  upload attendance lines
//	GET  {base}/iclock/getrequest?SN=..          heartbeat, returns commands
//	POST {base}/iclock/devicecmd?SN=..           command results
package adms

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Options are returned by the handshake.
type Options struct {
	Delay       time.Duration // heartbeat interval
	ErrorDelay  time.Duration // retry interval after a failure
	ATTLOGStamp int64
	TransFlag   string
	Realtime    bool
	Raw         map[string]string
}

// Command is a server-issued device command ("C:<id>:<cmd>").
type Command struct {
	ID   string
	Text string // e.g. "REBOOT", "CHECK", "DATA QUERY ATTLOG StartTime=..."
}

// Name is the first word of the command, used in replies.
func (c Command) Name() string {
	name, _, _ := strings.Cut(c.Text, " ")
	return name
}

// AttLog is one attendance line in push-protocol terms.
type AttLog struct {
	PIN      string
	Time     time.Time // device-local wall clock
	Status   int       // punch state (check-in/out...)
	Verify   int       // verification mode
	WorkCode string
}

// StatusError is a non-success HTTP response.
type StatusError struct {
	Code       int
	Body       string
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upax: HTTP %d: %s", e.Code, strings.TrimSpace(e.Body))
}

// IsNotFound reports whether the platform rejected the device address
// (wrong or revoked URL) rather than having a transient failure.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// Client talks to one device address on the platform.
type Client struct {
	Base      string // e.g. https://upax.example.com/<device-address-1>
	SN        string
	HTTP      *http.Client
	UserAgent string
}

func (c *Client) url(path string, q url.Values) string {
	q.Set("SN", c.SN)
	return strings.TrimRight(c.Base, "/") + path + "?" + q.Encode()
}

func (c *Client) do(ctx context.Context, method, u string, body string) (string, error) {
	var rd io.Reader
	if body != "" || method == http.MethodPost {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return "", err
	}
	if rd != nil {
		req.Header.Set("Content-Type", "text/plain")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		se := &StatusError{Code: resp.StatusCode, Body: string(b)}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			se.RetryAfter = time.Duration(s) * time.Second
		}
		return "", se
	}
	return string(b), nil
}

// Handshake announces the device and fetches its options.
func (c *Client) Handshake(ctx context.Context) (Options, error) {
	q := url.Values{"options": {"all"}, "pushver": {"2.4.1"}, "language": {"69"}}
	body, err := c.do(ctx, http.MethodGet, c.url("/iclock/cdata", q), "")
	if err != nil {
		return Options{}, err
	}
	o := Options{Delay: 10 * time.Second, ErrorDelay: 30 * time.Second, Raw: map[string]string{}}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		k, v, found := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !found {
			continue
		}
		o.Raw[k] = v
		n, _ := strconv.ParseInt(v, 10, 64)
		switch k {
		case "Delay":
			if n > 0 {
				o.Delay = time.Duration(n) * time.Second
			}
		case "ErrorDelay":
			if n > 0 {
				o.ErrorDelay = time.Duration(n) * time.Second
			}
		case "ATTLOGStamp":
			o.ATTLOGStamp = n
		case "TransFlag":
			o.TransFlag = v
		case "Realtime":
			o.Realtime = n == 1
		}
	}
	return o, nil
}

// FormatAttLog renders records in the tab-separated ATTLOG table format:
// PIN, time, status, verify, workcode, reserved, reserved.
func FormatAttLog(recs []AttLog) string {
	var b strings.Builder
	for _, r := range recs {
		wc := r.WorkCode
		if wc == "" {
			wc = "0"
		}
		fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%s\t0\t0\n", r.PIN, r.Time.Format("2006-01-02 15:04:05"), r.Status, r.Verify, wc)
	}
	return b.String()
}

// PushAttLog uploads attendance lines. stamp is the device-side position of
// the last record in the batch, echoed back by the platform as ATTLOGStamp.
func (c *Client) PushAttLog(ctx context.Context, stamp int64, recs []AttLog) error {
	q := url.Values{"table": {"ATTLOG"}, "Stamp": {strconv.FormatInt(stamp, 10)}}
	_, err := c.do(ctx, http.MethodPost, c.url("/iclock/cdata", q), FormatAttLog(recs))
	return err
}

// GetRequest is the heartbeat. info is the optional INFO= summary
// (firmware,users,fingerprints,records,ip). It returns pending commands.
func (c *Client) GetRequest(ctx context.Context, info string) ([]Command, error) {
	q := url.Values{}
	if info != "" {
		q.Set("INFO", info)
	}
	body, err := c.do(ctx, http.MethodGet, c.url("/iclock/getrequest", q), "")
	if err != nil {
		return nil, err
	}
	var cmds []Command
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		rest, found := strings.CutPrefix(line, "C:")
		if !found {
			continue
		}
		id, text, found := strings.Cut(rest, ":")
		if !found {
			continue
		}
		cmds = append(cmds, Command{ID: id, Text: text})
	}
	return cmds, nil
}

// Result codes for command replies.
const (
	ReturnOK          = 0
	ReturnError       = -1
	ReturnUnsupported = -1002
)

// Reply reports a command result. extra is appended as additional lines
// (used by INFO).
func (c *Client) Reply(ctx context.Context, cmd Command, ret int, extra string) error {
	body := fmt.Sprintf("ID=%s&Return=%d&CMD=%s\n", cmd.ID, ret, cmd.Name())
	body += extra
	_, err := c.do(ctx, http.MethodPost, c.url("/iclock/devicecmd", url.Values{}), body)
	return err
}
