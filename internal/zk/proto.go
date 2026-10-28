// Package zk implements the client side of the ZKTeco "standalone SDK"
// binary protocol (TCP port 4370) used by K14, K14 Pro, K40, MB-series and
// most other ZKTeco/ZKSoftware terminals.
//
// The wire format is ported from pyzk (https://github.com/fananimi/pyzk),
// including its quirks (checksum arithmetic, reply-id handling), because
// that is what real devices are known to accept.
package zk

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

const (
	CmdDBRRQ          = 7
	CmdUserWRQ        = 8
	CmdUserTempRRQ    = 9
	CmdOptionsRRQ     = 11
	CmdOptionsWRQ     = 12
	CmdDeleteUser     = 18
	CmdAttLogRRQ      = 13
	CmdGetFreeSizes   = 50
	CmdGetTime        = 201
	CmdSetTime        = 202
	CmdRegEvent       = 500
	CmdConnect        = 1000
	CmdExit           = 1001
	CmdEnableDevice   = 1002
	CmdDisableDevice  = 1003
	CmdRestart        = 1004
	CmdRefreshData    = 1013
	CmdGetVersion     = 1100
	CmdAuth           = 1102
	CmdPrepareData    = 1500
	CmdData           = 1501
	CmdFreeData       = 1502
	CmdPrepareBuffer  = 1503
	CmdReadBuffer     = 1504
	CmdAckOK          = 2000
	CmdAckError       = 2001
	CmdAckData        = 2002
	CmdAckRetry       = 2003
	CmdAckRepeat      = 2004
	CmdAckUnauth      = 2005
	CmdAckUnknown     = 0xffff
	FctAttLog         = 1
	FctUser           = 5
	ushrtMax          = 65535
	tcpMagic1         = 0x5050
	tcpMagic2         = 0x7d82 // 32130, pyzk MACHINE_PREPARE_DATA_2
	tcpHeaderSize     = 8
	packetHeaderSize  = 8
	maxPacketSize     = 16 << 20
	maxBufferedChunk  = 0xFFC0
	initialReplyID    = ushrtMax - 1
	attRecordSize40   = 40
	attRecordSize16   = 16
	attRecordSize8    = 8
	userRecordSize28  = 28
	userRecordSize72  = 72
	userRecordSize120 = 120
)

var le = binary.LittleEndian

// ErrUnauthorized is returned when the device rejects the comm key.
var ErrUnauthorized = errors.New("zk: device rejected the communication password (comm key)")

// Packet is one protocol packet (after the TCP envelope is removed).
type Packet struct {
	Cmd     uint16
	Session uint16
	Reply   uint16
	Data    []byte
}

// Checksum reproduces zkemsdk.c / pyzk's checksum, including its unusual
// modulo-65535 arithmetic.
func Checksum(p []byte) uint16 {
	c := 0
	for len(p) > 1 {
		c += int(le.Uint16(p))
		p = p[2:]
		if c > ushrtMax {
			c -= ushrtMax
		}
	}
	if len(p) == 1 {
		c += int(p[0])
	}
	for c > ushrtMax {
		c -= ushrtMax
	}
	c = ^c
	for c < 0 {
		c += ushrtMax
	}
	return uint16(c)
}

// EncodePacket builds a packet the way pyzk does: the checksum is computed
// over the header carrying the *current* reply id, then the reply id is
// incremented and the packet is emitted with the new one. It returns the
// encoded packet and the reply id that was used.
func EncodePacket(cmd, session, replyID uint16, data []byte) ([]byte, uint16) {
	buf := make([]byte, packetHeaderSize+len(data))
	le.PutUint16(buf[0:], cmd)
	le.PutUint16(buf[4:], session)
	le.PutUint16(buf[6:], replyID)
	copy(buf[8:], data)
	cs := Checksum(buf)

	next := int(replyID) + 1
	if next >= ushrtMax {
		next -= ushrtMax
	}
	le.PutUint16(buf[2:], cs)
	le.PutUint16(buf[6:], uint16(next))
	return buf, uint16(next)
}

// DecodePacket parses a packet (without the TCP envelope).
func DecodePacket(b []byte) (Packet, error) {
	if len(b) < packetHeaderSize {
		return Packet{}, fmt.Errorf("zk: short packet (%d bytes)", len(b))
	}
	return Packet{
		Cmd:     le.Uint16(b[0:]),
		Session: le.Uint16(b[4:]),
		Reply:   le.Uint16(b[6:]),
		Data:    b[8:],
	}, nil
}

// Frame wraps a packet in the TCP envelope (magic + length).
func Frame(pkt []byte) []byte {
	out := make([]byte, tcpHeaderSize+len(pkt))
	le.PutUint16(out[0:], tcpMagic1)
	le.PutUint16(out[2:], tcpMagic2)
	le.PutUint32(out[4:], uint32(len(pkt)))
	copy(out[8:], pkt)
	return out
}

// CommKey scrambles the numeric device password with the session id
// (port of MakeKey from commpro.c via pyzk).
func CommKey(password uint32, sessionID uint16) []byte {
	var k uint32
	for i := 0; i < 32; i++ {
		if password&(1<<i) != 0 {
			k = k<<1 | 1
		} else {
			k <<= 1
		}
	}
	k += uint32(sessionID)

	b := make([]byte, 4)
	le.PutUint32(b, k)
	b[0] ^= 'Z'
	b[1] ^= 'K'
	b[2] ^= 'S'
	b[3] ^= 'O'
	// swap the two 16-bit halves
	b = []byte{b[2], b[3], b[0], b[1]}

	const ticks = 50
	B := byte(ticks & 0xff)
	return []byte{b[0] ^ B, b[1] ^ B, B, b[3] ^ B}
}

// DecodeTime decodes the packed 32-bit timestamp used in stored records.
// Device times are local wall-clock times with no zone; the returned value
// carries time.UTC only as a container and must not be converted.
func DecodeTime(t uint32) time.Time {
	sec := int(t % 60)
	t /= 60
	min := int(t % 60)
	t /= 60
	hour := int(t % 24)
	t /= 24
	day := int(t%31) + 1
	t /= 31
	month := int(t%12) + 1
	t /= 12
	year := int(t) + 2000
	return time.Date(year, time.Month(month), day, hour, min, sec, 0, time.UTC)
}

// EncodeTime is the inverse of DecodeTime.
func EncodeTime(t time.Time) uint32 {
	d := ((t.Year()%100)*12*31+(int(t.Month())-1)*31+t.Day()-1)*(24*60*60) +
		(t.Hour()*60+t.Minute())*60 + t.Second()
	return uint32(d)
}

// DecodeTimeHex decodes the 6-byte (YY MM DD hh mm ss) time used by
// real-time events.
func DecodeTimeHex(b []byte) time.Time {
	return time.Date(2000+int(b[0]), time.Month(b[1]), int(b[2]), int(b[3]), int(b[4]), int(b[5]), 0, time.UTC)
}
