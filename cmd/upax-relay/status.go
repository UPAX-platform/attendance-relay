package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kardianos/service"

	"github.com/UPAX-platform/attendance-relay/internal/config"
	"github.com/UPAX-platform/attendance-relay/internal/identity"
	"github.com/UPAX-platform/attendance-relay/internal/relay"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
)

func cmdStatus(args []string) error {
	fs, path := newFlags("status")
	_ = fs.Parse(args)
	s, _, err := newService(*path, "")
	if err != nil {
		return err
	}
	st, err := s.Status()
	running := false
	switch {
	case errors.Is(err, service.ErrNotInstalled):
		fmt.Println("service: not installed")
	case err != nil:
		fmt.Println("service: unknown (", err, ")")
	case st == service.StatusRunning:
		fmt.Println("service: running")
		running = true
	case st == service.StatusStopped:
		fmt.Println("service: stopped")
	default:
		fmt.Println("service: unknown")
	}

	// The config is readable by administrators only (it holds comm keys);
	// the service's health files next to it are not.
	dir := filepath.Dir(*path)
	cfg, err := config.Load(*path)
	switch {
	case err == nil:
		dir = cfg.StateDir()
		fmt.Println("config: ", cfg.Path())
	case errors.Is(err, os.ErrPermission):
		fmt.Println("config: ", *path, "(readable with", adminWord()+")")
	default:
		fmt.Println("config: ", err)
	}
	fmt.Println("log:    ", filepath.Join(dir, "relay.log"))

	if h, err := relay.LoadRelayHealth(dir); err == nil {
		printRelayHealth(h, running)
	} else {
		fmt.Println("\nThe service hasn't run yet.")
	}
	for _, h := range relay.LoadAllTerminalHealth(dir) {
		printTerminalHealth(h)
	}
	return nil
}

func printRelayHealth(h *relay.RelayHealth, running bool) {
	fmt.Println()
	fmt.Println("relay")
	if h.Fingerprint != "" {
		fmt.Println("  fingerprint:", identity.Group(h.Fingerprint))
	}
	fmt.Println("  UPAX:       ", describeRelay(h.UPAXStatus))
	fmt.Println("  last sync:  ", ago(h.LastSync))
	if h.ClockOffsetS != nil && (*h.ClockOffsetS > 30 || *h.ClockOffsetS < -30) {
		fmt.Printf("  clock:       this computer is %ds off UPAX's clock; turn on automatic time\n", -*h.ClockOffsetS)
	}
	if h.Listener != "" {
		fmt.Println("  listener:   ", h.Listener, "(push terminals)")
	}
	if h.LastError != "" && h.LastErrorAt.After(h.LastSync) {
		fmt.Printf("  error:       %s (%s)\n", h.LastError, ago(h.LastErrorAt))
	}
	if running && time.Since(h.UpdatedAt) > 5*time.Minute {
		fmt.Printf("  warning:     no update from the service for %s; see the log\n", time.Since(h.UpdatedAt).Round(time.Second))
	}
}

func printTerminalHealth(h *relay.TerminalHealth) {
	fmt.Println()
	fmt.Print(h.Name)
	if h.Addr != "" && h.Addr != h.Name {
		fmt.Printf(" (%s)", h.Addr)
	}
	fmt.Printf(", %s %s", h.Vendor, h.Connection)
	if h.Serial != "" && h.Serial != h.Name {
		fmt.Printf(", serial %s", h.Serial)
	}
	fmt.Println()
	fmt.Println("  UPAX:     ", describeTerminal(h.UPAXStatus))
	reach := "no"
	if h.Reachable {
		reach = "yes"
	}
	fmt.Printf("  reachable: %s, last contact %s\n", reach, ago(h.LastContact))
	fmt.Printf("  uploaded:  %d punches, last %s\n", h.Uploaded, ago(h.LastUpload))
	if h.Pending > 0 {
		fmt.Printf("  pending:   %d punches\n", h.Pending)
	}
	if h.Spooled > 0 {
		fmt.Printf("  spooled:   %d punches waiting on this computer\n", h.Spooled)
	}
	if h.ClockSkewS != nil {
		sync := "off"
		if h.ClockSync {
			sync = "kept to the UPAX zone"
		}
		fmt.Printf("  clock:     %+ds, sync %s\n", *h.ClockSkewS, sync)
	}
	if h.CommKey == "managed" {
		fmt.Println("  comm key:  managed by the relay (upax-relay show-comm-key)")
	}
	if h.LastError != "" && h.LastErrorAt.After(h.LastContact) {
		fmt.Printf("  error:     %s (%s)\n", h.LastError, ago(h.LastErrorAt))
	}
}

func describeTerminal(status string) string {
	switch status {
	case upax.StatusActive:
		return "active"
	case upax.StatusPending:
		return "waiting for activation in UPAX; punches stay on the terminal until then"
	case upax.StatusPaused:
		return "paused in UPAX; punches stay on the terminal until it is resumed"
	case "unregistered":
		return "not added in UPAX yet: add it from the relay's discovered terminals"
	case "held":
		return "UPAX refused its punches as not active; holding until the next sync"
	case "":
		return "not reported yet"
	}
	return "waiting on the relay (" + status + ")"
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return fmt.Sprintf("%s (%s ago)", t.Local().Format(time.DateTime), time.Since(t).Round(time.Second))
}
