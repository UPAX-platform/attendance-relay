// Package push is the relay's LAN server for push-capable terminals (opt-in,
// listener.enabled). Terminals are pointed at the relay instead of at UPAX;
// the relay spools what they send and uploads it with its own key, so the
// terminals never hold a UPAX credential and never reach the internet.
//
// The server is vendor-neutral: it accepts connections from private
// addresses only, and routes requests to the vendor protocol handlers
// mounted on it (zkteco/adms on /iclock/). Handlers talk to the relay
// through Hub.
package push

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/device"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
)

// Contact is what a terminal told the relay about itself.
type Contact struct {
	Serial  string
	Vendor  string
	LANAddr string
	// Info is set when the terminal sent its summary (firmware, counters).
	Info *device.Info
}

// Clock is how a terminal's clock should be steered.
type Clock struct {
	// Sync is on when UPAX pins the terminal's clock to its zone.
	Sync bool
	// Zone is the terminal's UPAX zone (nil until UPAX gives one).
	Zone *time.Location
}

// Hub is the relay as protocol handlers see it.
type Hub interface {
	// Seen records a contact; it creates the terminal on first sight (an
	// unregistered claim until an admin adds it in UPAX).
	Seen(c Contact)
	// Accept reports whether serial's punches may be taken now. When not,
	// the handler refuses so the punches stay on the terminal, and asks it
	// to retry after the returned delay.
	Accept(serial string) (ok bool, retryAfter time.Duration)
	// Store durably spools punches; the handler acknowledges them to the
	// terminal only after it returns nil. stamp is the protocol's upload
	// position, returned by Stamp later.
	Store(serial string, punches []device.Punch, stamp string) error
	// Stamp is the last stored upload position for serial ("" = none).
	Stamp(serial string) string
	// Commands takes serial's queued commands, in order.
	Commands(serial string) []upax.Command
	// Result reports a command's outcome.
	Result(serial string, seq int64, ret int, cmd string)
	// Clock is serial's clock steering.
	Clock(serial string) Clock
	// Now is the current time, corrected to UPAX's clock.
	Now() time.Time
	// Delays are how often terminals should poll and retry.
	Delays() (poll, retry time.Duration)
}

var serialRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// ValidSerial reports whether s is acceptable as a serial number.
func ValidSerial(s string) bool { return serialRe.MatchString(s) }

// Server is the LAN listener.
type Server struct {
	Bind    string
	Handler http.Handler // the vendor handlers, by path
	Log     *slog.Logger
}

// Run serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Bind)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve serves on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(s.Log.Handler(), slog.LevelDebug),
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(privateOnly{Listener: ln, log: s.Log})
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// privateOnly drops connections from anything but private, link-local and
// loopback addresses, without a reply: the listener is for the LAN.
type privateOnly struct {
	net.Listener
	log *slog.Logger
}

func (l privateOnly) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if IsPrivate(c.RemoteAddr()) {
			return c, nil
		}
		l.log.Warn("push listener: dropped a connection from a non-private address", "remote", c.RemoteAddr().String())
		c.Close()
	}
}

// IsPrivate reports whether a is RFC 1918 / ULA, link-local or loopback.
func IsPrivate(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip := ta.IP
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback()
}
