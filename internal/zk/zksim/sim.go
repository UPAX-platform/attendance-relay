// Package zksim is a small ZKTeco device simulator that speaks the same TCP
// protocol as a real terminal (40-byte attendance records, 72-byte users).
// It is used by tests and by cmd/zksim for trying the relay without hardware.
package zksim

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"time"

	"github.com/UPAX-platform/zk-relay/internal/zk"
)

var le = binary.LittleEndian

type punch struct {
	uid    int
	userID string
	t      time.Time
	verify int
	state  int
}

// Device is a simulated terminal.
type Device struct {
	Serial   string
	Firmware string
	Password uint32 // 0 = no comm key
	// ChunkSize, if > 0, splits buffered data into several CMD_DATA packets
	// to exercise multi-packet reads.
	ChunkSize int
	// BeforeAttLogRead, if set, runs just before the attendance table is
	// snapshotted for a download, e.g. to land a punch mid-read.
	BeforeAttLogRead func()

	mu       sync.Mutex
	punches  []punch
	users    map[string]*simUser
	restarts int
	ln       net.Listener
}

type simUser struct {
	uid  int
	name string
	pri  int
}

// New returns a simulator with no records.
func New(serial string) *Device {
	return &Device{Serial: serial, Firmware: "Ver 6.60 Apr 28 2017", users: map[string]*simUser{}}
}

// AddUser enrols a user, as if done in the device menu.
func (d *Device) AddUser(userID, name string, privilege int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	u := d.userLocked(userID)
	u.name, u.pri = name, privilege
}

// User reports an enrolled user's name and privilege.
func (d *Device) User(userID string) (name string, privilege int, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	u, ok := d.users[userID]
	if !ok {
		return "", 0, false
	}
	return u.name, u.pri, true
}

func (d *Device) userLocked(userID string) *simUser {
	u, ok := d.users[userID]
	if !ok {
		maxUID := 0
		for _, o := range d.users {
			maxUID = max(maxUID, o.uid)
		}
		u = &simUser{uid: maxUID + 1, name: "User " + userID}
		d.users[userID] = u
	}
	return u
}

// Punch appends an attendance record, as if someone used the device.
func (d *Device) Punch(userID string, t time.Time, verify, state int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.punches = append(d.punches, punch{d.userLocked(userID).uid, userID, t, verify, state})
}

// ClearLog wipes the attendance log (like "Delete attendance logs" in the menu).
func (d *Device) ClearLog() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.punches = nil
}

// DropOldest removes the n oldest records (like a device rolling its log).
func (d *Device) DropOldest(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.punches = d.punches[min(n, len(d.punches)):]
}

// Restarts reports how many CMD_RESTART requests were received.
func (d *Device) Restarts() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.restarts
}

// Listen starts serving on addr (use "127.0.0.1:0" for a random port).
func (d *Device) Listen(addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	d.ln = ln
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go d.serve(conn)
		}
	}()
	return ln.Addr().String(), nil
}

// Close stops the listener.
func (d *Device) Close() error {
	if d.ln == nil {
		return nil
	}
	return d.ln.Close()
}

func (d *Device) serve(conn net.Conn) {
	defer conn.Close()
	const session = 0x3a2b
	authed := d.Password == 0
	var pending []byte // data prepared by CMD_PREPARE_BUFFER

	write := func(cmd uint16, reply uint16, data []byte) error {
		buf := make([]byte, 8+len(data))
		le.PutUint16(buf[0:], cmd)
		le.PutUint16(buf[4:], session)
		le.PutUint16(buf[6:], reply)
		copy(buf[8:], data)
		le.PutUint16(buf[2:], zk.Checksum(buf))
		_, err := conn.Write(zk.Frame(buf))
		return err
	}

	for {
		var hdr [8]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return
		}
		body := make([]byte, le.Uint32(hdr[4:]))
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		p, err := zk.DecodePacket(body)
		if err != nil {
			return
		}
		r := p.Reply

		if !authed && p.Cmd != zk.CmdConnect && p.Cmd != zk.CmdAuth {
			_ = write(zk.CmdAckUnauth, r, nil)
			continue
		}

		switch p.Cmd {
		case zk.CmdConnect:
			if authed {
				err = write(zk.CmdAckOK, r, nil)
			} else {
				err = write(zk.CmdAckUnauth, r, nil)
			}
		case zk.CmdAuth:
			if string(p.Data) == string(zk.CommKey(d.Password, session)) {
				authed = true
				err = write(zk.CmdAckOK, r, nil)
			} else {
				err = write(zk.CmdAckUnauth, r, nil)
			}
		case zk.CmdExit:
			_ = write(zk.CmdAckOK, r, nil)
			return
		case zk.CmdGetVersion:
			err = write(zk.CmdAckOK, r, append([]byte(d.Firmware), 0))
		case zk.CmdOptionsRRQ:
			name := string(p.Data)
			if i := len(name) - 1; i >= 0 && name[i] == 0 {
				name = name[:i]
			}
			val := map[string]string{"~SerialNumber": d.Serial, "~Platform": "ZMM220_TFT", "~DeviceName": "K14 Pro (sim)"}[name]
			err = write(zk.CmdAckOK, r, append([]byte(name+"="+val), 0))
		case zk.CmdGetTime:
			b := make([]byte, 4)
			le.PutUint32(b, zk.EncodeTime(time.Now()))
			err = write(zk.CmdAckOK, r, b)
		case zk.CmdGetFreeSizes:
			d.mu.Lock()
			b := make([]byte, 92)
			le.PutUint32(b[4*4:], uint32(len(d.users)))
			le.PutUint32(b[8*4:], uint32(len(d.punches)))
			le.PutUint32(b[15*4:], 3000)
			le.PutUint32(b[16*4:], 100000)
			d.mu.Unlock()
			err = write(zk.CmdAckOK, r, b)
		case zk.CmdPrepareBuffer:
			if len(p.Data) < 11 {
				err = write(zk.CmdAckError, r, nil)
				break
			}
			switch le.Uint16(p.Data[1:]) {
			case zk.CmdAttLogRRQ:
				if d.BeforeAttLogRead != nil {
					d.BeforeAttLogRead()
				}
				pending = d.attData()
			case zk.CmdUserTempRRQ:
				pending = d.userData()
			default:
				err = write(zk.CmdAckError, r, nil)
				continue
			}
			b := make([]byte, 9)
			le.PutUint32(b[1:], uint32(len(pending)))
			err = write(zk.CmdAckOK, r, b)
		case zk.CmdReadBuffer:
			start, size := int(le.Uint32(p.Data)), int(le.Uint32(p.Data[4:]))
			if start+size > len(pending) {
				err = write(zk.CmdAckError, r, nil)
				break
			}
			chunk := pending[start : start+size]
			sz := make([]byte, 8)
			le.PutUint32(sz, uint32(size))
			if err = write(zk.CmdPrepareData, r, sz); err != nil {
				break
			}
			step := d.ChunkSize
			if step <= 0 {
				step = len(chunk)
			}
			for len(chunk) > 0 && err == nil {
				n := min(step, len(chunk))
				err = write(zk.CmdData, r, chunk[:n])
				chunk = chunk[n:]
			}
			if err == nil {
				err = write(zk.CmdAckOK, r, nil)
			}
		case zk.CmdFreeData:
			pending = nil
			err = write(zk.CmdAckOK, r, nil)
		case zk.CmdUserWRQ:
			if len(p.Data) != 72 {
				err = write(zk.CmdAckError, r, nil)
				break
			}
			d.writeUser(p.Data)
			err = write(zk.CmdAckOK, r, nil)
		case zk.CmdDeleteUser:
			if len(p.Data) < 2 {
				err = write(zk.CmdAckError, r, nil)
				break
			}
			d.deleteUser(int(le.Uint16(p.Data)))
			err = write(zk.CmdAckOK, r, nil)
		case zk.CmdRestart:
			d.mu.Lock()
			d.restarts++
			d.mu.Unlock()
			_ = write(zk.CmdAckOK, r, nil)
			return
		case zk.CmdEnableDevice, zk.CmdDisableDevice, zk.CmdRegEvent, zk.CmdRefreshData, 60, 62:
			err = write(zk.CmdAckOK, r, nil)
		default:
			err = write(zk.CmdAckUnknown, r, nil)
		}
		if err != nil {
			return
		}
	}
}

func (d *Device) attData() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]byte, 4, 4+40*len(d.punches))
	le.PutUint32(out, uint32(40*len(d.punches)))
	for _, p := range d.punches {
		rec := make([]byte, 40)
		le.PutUint16(rec, uint16(p.uid))
		copy(rec[2:26], p.userID)
		rec[26] = byte(p.verify)
		le.PutUint32(rec[27:], zk.EncodeTime(p.t))
		rec[31] = byte(p.state)
		out = append(out, rec...)
	}
	return out
}

func (d *Device) userData() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]byte, 4, 4+72*len(d.users))
	le.PutUint32(out, uint32(72*len(d.users)))
	for id, u := range d.users {
		rec := make([]byte, 72)
		le.PutUint16(rec, uint16(u.uid))
		rec[2] = byte(u.pri)
		copy(rec[11:35], u.name)
		copy(rec[48:72], id)
		out = append(out, rec...)
	}
	return out
}

// writeUser stores a 72-byte CMD_USER_WRQ record, keyed by its PIN.
func (d *Device) writeUser(rec []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	uid, id := int(le.Uint16(rec)), cstr(rec[48:72])
	for other, u := range d.users {
		if u.uid == uid && other != id {
			delete(d.users, other)
		}
	}
	d.users[id] = &simUser{uid: uid, name: cstr(rec[11:35]), pri: int(rec[2])}
}

func (d *Device) deleteUser(uid int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, u := range d.users {
		if u.uid == uid {
			delete(d.users, id)
		}
	}
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
