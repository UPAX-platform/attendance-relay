package sdk

import (
	"bufio"
	"io"
	"math/rand/v2"
	"net"
	"strings"
	"testing"
	"time"
)

// Whatever a terminal sends, decoding must fail cleanly, never panic.

func TestDecodeGarbageNeverPanics(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	c := &Client{}
	for n := 0; n < 300; n++ {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.UintN(256))
		}
		_, _ = DecodePacket(b)
		_ = DecodeTimeHex(b)
		_ = DecodeTime(rng.Uint32())
		for _, size := range []int{-40, -1, 0, attRecordSize16, attRecordSize40, 41, 1 << 20} {
			// userCount 0: the 8-byte layout's PIN lookup needs no device.
			if _, err := c.decodeAttendance(b, size, 0); err != nil {
				t.Fatalf("decode %d bytes as %d: %v", n, size, err)
			}
		}
		_, _ = c.decodeAttendance(append(append([]byte(nil), attCodeInit...), b...), attRecordSize40, 0)
	}
}

// fakeDevice is a TCP peer answering each request with reply (raw bytes,
// envelope included; nil sends nothing).
func fakeDevice(t *testing.T, reply func(req Packet) []byte) *Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		dev, err := ln.Accept()
		if err != nil {
			return
		}
		defer dev.Close()
		r := bufio.NewReader(dev)
		for {
			var hdr [tcpHeaderSize]byte
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				return
			}
			buf := make([]byte, le.Uint32(hdr[4:]))
			if _, err := io.ReadFull(r, buf); err != nil {
				return
			}
			p, _ := DecodePacket(buf)
			if out := reply(p); out != nil {
				if _, err := dev.Write(out); err != nil {
					return
				}
			}
		}
	}()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	return &Client{conn: cli, r: bufio.NewReader(cli), Timeout: 2 * time.Second, reply: initialReplyID}
}

func answer(cmd uint16, data []byte) []byte {
	pkt, _ := EncodePacket(cmd, 0, 0, data)
	return Frame(pkt)
}

func TestBadEnvelopeAndLength(t *testing.T) {
	for name, raw := range map[string][]byte{
		"bad magic":  []byte("HTTP/1.1 200 OK\r\n\r\n"),
		"too short":  {0x50, 0x50, 0x82, 0x7d, 2, 0, 0, 0, 0, 0},
		"huge":       {0x50, 0x50, 0x82, 0x7d, 0xff, 0xff, 0xff, 0xff},
		"truncation": {0x50, 0x50, 0x82, 0x7d, 16, 0, 0, 0, 1, 2},
	} {
		c := fakeDevice(t, func(Packet) []byte { return raw })
		if _, err := c.command(CmdGetTime, nil); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestHugeTableRefused(t *testing.T) {
	c := fakeDevice(t, func(Packet) []byte {
		d := make([]byte, 9)
		le.PutUint32(d[1:], 0xffffffff) // 4 GiB announced
		return answer(CmdAckOK, d)
	})
	if _, err := c.readBuffered(CmdAttLogRRQ, 0, 0); err == nil || !strings.Contains(err.Error(), "table") {
		t.Fatalf("err = %v", err)
	}
}

func TestEndlessChunkRefused(t *testing.T) {
	c := fakeDevice(t, func(p Packet) []byte {
		if p.Cmd != CmdReadBuffer {
			return answer(CmdAckOK, nil)
		}
		// PREPARE_DATA, then data packets that never end.
		out := answer(CmdPrepareData, nil)
		for range 200 {
			out = append(out, answer(CmdData, make([]byte, 1024))...)
		}
		return out
	})
	if _, err := c.readChunk(0, 4096); err == nil {
		t.Fatal("no error")
	}
}

func TestEventFloodEnds(t *testing.T) {
	c := fakeDevice(t, func(p Packet) []byte {
		if p.Cmd != CmdGetTime {
			return nil // the client's event acks
		}
		var out []byte
		for range maxEventsSkipped + 1 {
			out = append(out, answer(CmdRegEvent, nil)...)
		}
		return out
	})
	if _, err := c.command(CmdGetTime, nil); err == nil {
		t.Fatal("no error")
	}
}
