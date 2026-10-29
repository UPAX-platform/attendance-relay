package sdk_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/device/zkteco/sdk"
	"github.com/UPAX-platform/attendance-relay/internal/device/zkteco/zksim"
)

func TestChecksumMatchesPyzk(t *testing.T) {
	// CMD_CONNECT, session 0, reply 65534 -> value checked by hand against
	// pyzk's __create_checksum loop: words 1000 + 0 + 0 + 65534 = 66534,
	// wrapped -65535 = 999, ~999 = -1000, +65535 = 64535.
	pkt := []byte{0xe8, 0x03, 0, 0, 0, 0, 0xfe, 0xff}
	if got := sdk.Checksum(pkt); got != 64535 {
		t.Fatalf("checksum = %d, want 64535", got)
	}
	// first packet uses reply id 0 after pyzk's increment/wrap
	enc, used := sdk.EncodePacket(sdk.CmdConnect, 0, 65534, nil)
	if used != 0 {
		t.Fatalf("reply id = %d, want 0", used)
	}
	want := []byte{0xe8, 0x03, 0x17, 0xfc, 0, 0, 0, 0}
	if string(enc) != string(want) {
		t.Fatalf("encoded % x, want % x", enc, want)
	}
}

// TestFrameMatchesPyzk pins the TCP envelope to bytes captured from pyzk
// talking to a real K14 Pro. zksim shares the constants, so only a fixed
// expectation like this one catches a wrong magic.
func TestFrameMatchesPyzk(t *testing.T) {
	enc, _ := sdk.EncodePacket(sdk.CmdConnect, 0, 65534, nil)
	want := []byte{0x50, 0x50, 0x82, 0x7d, 0x08, 0, 0, 0, 0xe8, 0x03, 0x17, 0xfc, 0, 0, 0, 0}
	if got := sdk.Frame(enc); string(got) != string(want) {
		t.Fatalf("frame % x, want % x", got, want)
	}
}

func TestTimeRoundTrip(t *testing.T) {
	in := time.Date(2026, 10, 28, 17, 4, 59, 0, time.UTC)
	if got := sdk.DecodeTime(sdk.EncodeTime(in)); !got.Equal(in) {
		t.Fatalf("round trip %v -> %v", in, got)
	}
}

func dial(t *testing.T, d *zksim.Device, pw uint32) *sdk.Client {
	t.Helper()
	addr, err := d.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	c, err := sdk.Dial(context.Background(), addr, pw, 2*time.Second)
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
	_, err := sdk.Dial(context.Background(), addr, 1, time.Second)
	if !errors.Is(err, sdk.ErrUnauthorized) {
		t.Fatalf("wrong password: err = %v", err)
	}
}

func TestAttendanceRetriesWhenPunchLandsMidRead(t *testing.T) {
	d := zksim.New("RACE")
	base := time.Date(2026, 1, 2, 8, 0, 0, 0, time.UTC)
	d.Punch("1", base, 1, 0)
	once := false
	d.BeforeAttLogRead = func() {
		if !once {
			once = true
			d.Punch("2", base.Add(time.Minute), 1, 0)
		}
	}
	c := dial(t, d, 0)
	recs, err := c.Attendance()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].UserID != "1" || recs[1].UserID != "2" {
		t.Fatalf("got %+v", recs)
	}
}

func TestSetAndDeleteUser(t *testing.T) {
	d := zksim.New("USERS")
	d.AddUser("7", "Old Name", 14)
	c := dial(t, d, 0)

	if err := c.SetUser("42", "Mona Ali", 0); err != nil {
		t.Fatal(err)
	}
	if name, pri, ok := d.User("42"); !ok || name != "Mona Ali" || pri != 0 {
		t.Fatalf("new user = %q %d %v", name, pri, ok)
	}
	// renaming an administrator keeps them an administrator
	if err := c.SetUser("7", "New Name", 0); err != nil {
		t.Fatal(err)
	}
	if name, pri, _ := d.User("7"); name != "New Name" || pri != 14 {
		t.Fatalf("admin = %q %d", name, pri)
	}
	users, err := c.Users()
	if err != nil || len(users) != 2 {
		t.Fatalf("users = %+v, %v", users, err)
	}

	if err := c.DeleteUser("42"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := d.User("42"); ok {
		t.Fatal("user 42 still on the device")
	}
	if err := c.DeleteUser("404"); err != nil {
		t.Fatalf("deleting an unknown PIN: %v", err)
	}
}

func TestSetUserOnEmptyDevice(t *testing.T) {
	d := zksim.New("EMPTY")
	c := dial(t, d, 0)
	if err := c.SetUser("1", "First", 0); err != nil {
		t.Fatal(err)
	}
	if name, _, ok := d.User("1"); !ok || name != "First" {
		t.Fatalf("user = %q %v", name, ok)
	}
}

func TestSetTime(t *testing.T) {
	d := zksim.New("SIM1")
	c := dial(t, d, 0)
	want := time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC)
	if err := c.SetTime(want); err != nil {
		t.Fatal(err)
	}
	got, err := c.Time()
	if err != nil {
		t.Fatal(err)
	}
	if skew := got.Sub(want); skew < 0 || skew > 2*time.Second {
		t.Fatalf("device time %v, want about %v", got, want)
	}
}

func TestSetCommKey(t *testing.T) {
	d := zksim.New("SIM1")
	c := dial(t, d, 0)
	if err := c.SetCommKey(0); err == nil {
		t.Fatal("comm key 0 accepted")
	}
	if err := c.SetCommKey(424242); err != nil {
		t.Fatal(err)
	}
	// This session stays authenticated; new ones need the key.
	if _, err := c.Sizes(); err != nil {
		t.Fatal(err)
	}
	if _, err := sdk.Dial(context.Background(), c.Addr, 0, 2*time.Second); !errors.Is(err, sdk.ErrUnauthorized) {
		t.Fatalf("dial without the new key: %v, want ErrUnauthorized", err)
	}
	c2, err := sdk.Dial(context.Background(), c.Addr, 424242, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c2.Close()
}
