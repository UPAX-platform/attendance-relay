package zk_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/UPAX-platform/zk-relay/internal/zk"
	"github.com/UPAX-platform/zk-relay/internal/zk/zksim"
)

func TestChecksumMatchesPyzk(t *testing.T) {
	// CMD_CONNECT, session 0, reply 65534 -> value checked by hand against
	// pyzk's __create_checksum loop: words 1000 + 0 + 0 + 65534 = 66534,
	// wrapped -65535 = 999, ~999 = -1000, +65535 = 64535.
	pkt := []byte{0xe8, 0x03, 0, 0, 0, 0, 0xfe, 0xff}
	if got := zk.Checksum(pkt); got != 64535 {
		t.Fatalf("checksum = %d, want 64535", got)
	}
	// first packet uses reply id 0 after pyzk's increment/wrap
	enc, used := zk.EncodePacket(zk.CmdConnect, 0, 65534, nil)
	if used != 0 {
		t.Fatalf("reply id = %d, want 0", used)
	}
	want := []byte{0xe8, 0x03, 0x17, 0xfc, 0, 0, 0, 0}
	if string(enc) != string(want) {
		t.Fatalf("encoded % x, want % x", enc, want)
	}
}

func TestTimeRoundTrip(t *testing.T) {
	in := time.Date(2026, 10, 28, 17, 4, 59, 0, time.UTC)
	if got := zk.DecodeTime(zk.EncodeTime(in)); !got.Equal(in) {
		t.Fatalf("round trip %v -> %v", in, got)
	}
}

func dial(t *testing.T, d *zksim.Device, pw uint32) *zk.Client {
	t.Helper()
	addr, err := d.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c, err := zk.Dial(context.Background(), addr, pw, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestReadAttendance(t *testing.T) {
	d := zksim.New("K14TEST01")
	d.ChunkSize = 100 // force multi-packet data transfer
	base := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	for i := 0; i < 2000; i++ { // > one 0xFFC0 chunk (40*2000 = 80000 bytes)
		d.Punch(fmt.Sprint(i%37+1), base.Add(time.Duration(i)*time.Minute), 1, i%2)
	}
	c := dial(t, d, 0)

	sn, err := c.SerialNumber()
	if err != nil || sn != "K14TEST01" {
		t.Fatalf("serial = %q, %v", sn, err)
	}
	if fw, err := c.Firmware(); err != nil || fw == "" {
		t.Fatalf("firmware = %q, %v", fw, err)
	}
	recs, err := c.Attendance()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2000 {
		t.Fatalf("got %d records", len(recs))
	}
	r := recs[1999]
	if r.UserID != fmt.Sprint(1999%37+1) || !r.Time.Equal(base.Add(1999*time.Minute)) || r.Punch != 1 || r.Verify != 1 {
		t.Fatalf("bad last record %+v", r)
	}
	// connection still usable after a large read
	if _, err := c.Sizes(); err != nil {
		t.Fatal(err)
	}
}

func TestPassword(t *testing.T) {
	d := zksim.New("PW")
	d.Password = 123456
	c := dial(t, d, 123456)
	if _, err := c.SerialNumber(); err != nil {
		t.Fatal(err)
	}

	addr, _ := d.Listen("127.0.0.1:0")
	_, err := zk.Dial(context.Background(), addr, 1, time.Second)
	if !errors.Is(err, zk.ErrUnauthorized) {
		t.Fatalf("wrong password: err = %v", err)
	}
}
