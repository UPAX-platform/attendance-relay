// Command zksim runs a simulated ZKTeco terminal for trying the relay
// without hardware. Type a user PIN and press Enter to record a punch.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/device/zkteco/zksim"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:4370", "address to listen on")
	serial := flag.String("serial", "SIMK14PRO0001", "serial number to report")
	password := flag.Uint("password", 0, "comm key (0 = none)")
	seed := flag.Int("seed", 3, "number of historic punches to pre-load")
	flag.Parse()

	d := zksim.New(*serial)
	d.Password = uint32(*password)
	base := time.Now().Add(-time.Duration(*seed) * time.Hour)
	for i := 0; i < *seed; i++ {
		d.Punch(fmt.Sprint(100+i), base.Add(time.Duration(i)*time.Hour), 1, i%2)
	}
	a, err := d.Listen(*addr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("simulated device %s listening on %s (%d records)\n", *serial, a, *seed)
	fmt.Println("type a user PIN + Enter to punch; 'clear' wipes the log")
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		switch in := strings.TrimSpace(sc.Text()); in {
		case "":
		case "clear":
			d.ClearLog()
			fmt.Println("log cleared")
		default:
			d.Punch(in, time.Now(), 1, 0)
			fmt.Printf("punch recorded for %s\n", in)
		}
	}
	select {}
}
