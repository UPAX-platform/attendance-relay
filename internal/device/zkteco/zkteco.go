// Package zkteco is the ZKTeco driver: terminals pulled over the standalone
// SDK protocol (package sdk) on TCP 4370. Push-capable ZKTeco terminals talk
// ADMS to the relay's LAN listener instead (package adms).
package zkteco

import (
	"context"
	"errors"
	"time"

	"github.com/UPAX-platform/attendance-relay/internal/device"
	"github.com/UPAX-platform/attendance-relay/internal/device/zkteco/sdk"
)

// Vendor is the name terminals use in the config (vendor: zkteco).
const Vendor = "zkteco"

func init() { device.Register(Vendor, Driver{}) }

// Driver implements device.Driver.
type Driver struct{}

func (Driver) DefaultPort() int { return 4370 }

func (d Driver) Dial(ctx context.Context, t device.Target, timeout time.Duration) (device.Conn, error) {
	c, err := sdk.Dial(ctx, t.Addr(d.DefaultPort()), t.Password, timeout)
	if errors.Is(err, sdk.ErrUnauthorized) {
		return nil, device.ErrUnauthorized
	}
	if err != nil {
		return nil, err
	}
	return &conn{c: c}, nil
}

type conn struct{ c *sdk.Client }

func (c *conn) Info() (device.Info, error) {
	sn, err := c.c.SerialNumber()
	if err != nil {
		return device.Info{}, err
	}
	sz, err := c.c.Sizes()
	if err != nil {
		return device.Info{}, err
	}
	fw, _ := c.c.Firmware()
	model, _ := c.c.DeviceName()
	return device.Info{Serial: sn, Model: model, Firmware: fw, Users: sz.Users, Fingerprints: sz.Fingers, Records: sz.Records}, nil
}

func (c *conn) RecordCount() (int, error) {
	sz, err := c.c.Sizes()
	return sz.Records, err
}

func (c *conn) Punches() ([]device.Punch, error) {
	recs, err := c.c.Attendance()
	if err != nil {
		return nil, err
	}
	out := make([]device.Punch, len(recs))
	for i, r := range recs {
		out[i] = device.Punch{PIN: r.UserID, Time: r.Time, State: r.Punch, Verify: r.Verify, WorkCode: "0"}
	}
	return out, nil
}

func (c *conn) Clock() (time.Time, error)  { return c.c.Time() }
func (c *conn) SetClock(t time.Time) error { return c.c.SetTime(t) }
func (c *conn) SetUser(pin, name string, privilege int) error {
	return c.c.SetUser(pin, name, privilege)
}
func (c *conn) DeleteUser(pin string) error    { return c.c.DeleteUser(pin) }
func (c *conn) SetCommKey(key uint32) error    { return c.c.SetCommKey(key) }
func (c *conn) CommKeyRange() (uint32, uint32) { return 1, 999999 }
func (c *conn) Close() error                   { return c.c.Close() }
