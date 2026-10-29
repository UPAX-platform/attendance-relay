// Command relaycheck runs a fake UPAX (verifying signatures like the real
// one) and a simulated ZKTeco terminal in one process, for end-to-end
// checks of an installed relay (scripts/windows-check.ps1). It writes where
// everything listens to a JSON file and serves a small control API:
//
//	GET  /state     punches received, relay status, rotations
//	POST /punch     record a punch on the terminal (?pin=)
//	POST /drop-prev stop accepting the relay's previous key
//
// It is a test tool: it accepts any relay that pairs with its codes and
// activates it straight away.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/device/zkteco/zksim"
	"github.com/UPAX-platform/attendance-relay/internal/upax"
	"github.com/UPAX-platform/attendance-relay/internal/upax/upaxtest"
)

const serial = "CHECK0000001"

// codes are the pairing codes the fake accepts, each once.
var codes = []string{"RLY-7KQ2-M9XD-4TPA", "RLY-8KQ2-M9XD-4TPA", "RLY-9KQ2-M9XD-4TPA"}

func main() {
	statePath := flag.String("state", "relaycheck.json", "file to write the addresses to")
	seed := flag.Int("seed", 3, "punches stored on the terminal at start")
	flag.Parse()

	f := upaxtest.New()
	for _, c := range codes {
		f.Codes[c] = f.RelayID
	}
	f.Register(serial, upaxtest.Terminal{Timezone: "UTC"})
	// Activate every pairing, as an admin in UPAX would.
	go func() {
		for range time.Tick(200 * time.Millisecond) {
			f.Do(func(f *upaxtest.Fake) {
				if f.Pub != nil && f.Status == upax.StatusPending {
					f.Status = upax.StatusActive
				}
			})
		}
	}()

	d := zksim.New(serial)
	base := time.Now().Add(-time.Duration(*seed) * time.Hour)
	for i := range *seed {
		d.Punch(fmt.Sprint(100+i), base.Add(time.Duration(i)*time.Hour), 1, 0)
	}
	term, err := d.Listen("127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, r *http.Request) {
		var out struct {
			Punches int    `json:"punches"`
			Status  string `json:"status"`
			Rotated int    `json:"rotated"`
		}
		out.Punches = f.PunchCount(serial)
		f.Do(func(f *upaxtest.Fake) { out.Status, out.Rotated = f.Status, f.Rotated })
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /punch", func(w http.ResponseWriter, r *http.Request) {
		d.Punch(r.URL.Query().Get("pin"), time.Now(), 1, 0)
	})
	mux.HandleFunc("POST /drop-prev", func(w http.ResponseWriter, r *http.Request) {
		f.Do(func(f *upaxtest.Fake) { f.PrevPub = nil })
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go func() { log.Fatal(http.Serve(ln, mux)) }()

	b, _ := json.MarshalIndent(map[string]any{
		"upax":     f.URL,
		"terminal": term,
		"control":  "http://" + ln.Addr().String(),
		"codes":    codes,
		"seeded":   *seed,
	}, "", "  ")
	if err := os.WriteFile(*statePath, b, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("fake UPAX %s, terminal %s, control http://%s", f.URL, term, ln.Addr())
	select {}
}
