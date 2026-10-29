package relay

import (
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/upax"
)

// upaxView is what the latest sync said about one terminal.
type upaxView struct {
	known      bool // a sync reply covered the terminal (the relay is active)
	registered bool // UPAX lists it under this relay
	deviceID   string
	status     string
	timezone   string
	clockSync  bool
}

// Reasons a terminal's uploads are held.
const (
	holdRelay        = "relay"        // the relay isn't active (the sync loop logs why)
	holdUnregistered = "unregistered" // not added in UPAX yet
	holdTerminal     = "terminal"     // pending or paused in UPAX
	holdRefused      = "refused"      // an upload said it isn't active
)

// gate decides whether a terminal may upload. Uploads need the relay and
// the terminal to be active in UPAX; until then punches stay where they are
// (on the terminal, or in a push terminal's spool).
func gate(relayStatus string, v upaxView, held bool) (open bool, why string) {
	switch {
	case relayStatus != upax.StatusActive || !v.known:
		return false, holdRelay
	case !v.registered:
		return false, holdUnregistered
	case v.status != upax.StatusActive:
		return false, holdTerminal
	case held:
		return false, holdRefused
	}
	return true, ""
}

// upaxStatusWord is a terminal's state for health files and status.
func upaxStatusWord(relayStatus string, v upaxView, held bool) string {
	switch _, why := gate(relayStatus, v, held); why {
	case holdRelay:
		return "relay_" + relayOr(relayStatus)
	case holdUnregistered:
		return "unregistered"
	case holdRefused:
		return "held"
	}
	return v.status
}

func relayOr(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// wall is t's wall-clock reading carried as UTC, the form terminals use.
func wall(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
}

// zone loads a UPAX timezone; "" or an unknown name gives the relay's own.
func zone(name string) (*time.Location, bool) {
	if name == "" {
		return time.Local, false
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Local, false
	}
	return loc, true
}

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
