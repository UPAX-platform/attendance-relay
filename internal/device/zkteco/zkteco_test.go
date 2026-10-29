package zkteco_test

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/device"
	_ "github.com/UPAX-platform/attendance-relay/internal/device/zkteco"
	"github.com/UPAX-platform/attendance-relay/internal/device/zkteco/zksim"
)

func TestDriver(t *testing.T) {
	sim := zksim.New("SIM9")
	sim.Password = 77
	addr, err := sim.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sim.Close()
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	drv, err := device.Lookup("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := drv.Dial(ctx, device.Target{Host: host, Port: p}, time.Second); !errors.Is(err, device.ErrUnauthorized) {
		t.Fatalf("no comm key: %v, want device.ErrUnauthorized", err)
	}
	sim.Punch("5", time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC), 15, 1)
	c, err := drv.Dial(ctx, device.Target{Host: host, Port: p, Password: 77}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	in, err := c.Info()
	if err != nil || in.Serial != "SIM9" || in.Records != 1 || in.Model == "" {
		t.Fatalf("info %+v %v", in, err)
	}
	ps, err := c.Punches()
	if err != nil || len(ps) != 1 || ps[0] != (device.Punch{PIN: "5", Time: time.Date(2026, 10, 28, 8, 0, 0, 0, time.UTC), State: 1, Verify: 15, WorkCode: "0"}) {
		t.Fatalf("punches %+v %v", ps, err)
	}
	ks, ok := c.(device.CommKeySetter)
	if !ok {
		t.Fatal("zkteco conn doesn't manage comm keys")
	}
	if lo, hi := ks.CommKeyRange(); lo != 1 || hi != 999999 {
		t.Fatalf("range %d-%d", lo, hi)
	}
}
