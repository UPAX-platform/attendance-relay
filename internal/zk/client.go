package zk

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Record is one attendance log entry stored on the device.
type Record struct {
	UID    int       // internal device slot
	UserID string    // the PIN / enrolment number shown on the device
	Time   time.Time // device-local wall clock (zone-less, see DecodeTime)
	Verify int       // how the user verified: 0 password, 1 fingerprint, 15 face, ...
	Punch  int       // attendance state: 0 check-in, 1 check-out, 4 OT-in, ...
}

// Sizes is the subset of CMD_GET_FREE_SIZES we care about.
type Sizes struct {
	Users, Fingers, Records int
	UsersCap, RecordsCap    int
}

// Client is a connection to a single device. It is not safe for concurrent use.
type Client struct {
	Addr     string
	Password uint32
	Timeout  time.Duration

	conn    net.Conn
	r       *bufio.Reader
	session uint16
	reply   uint16
}

// Dial connects and authenticates to the device at addr ("host:port").
func Dial(ctx context.Context, addr string, password uint32, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	d := net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c := &Client{Addr: addr, Password: password, Timeout: timeout, conn: conn, r: bufio.NewReaderSize(conn, 64<<10), reply: initialReplyID}

	p, err := c.command(CmdConnect, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("zk connect: %w", err)
	}
	c.session = p.Session
	if p.Cmd == CmdAckUnauth {
		p, err = c.command(CmdAuth, CommKey(password, c.session))
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("zk auth: %w", err)
		}
	}
	if p.Cmd != CmdAckOK {
		conn.Close()
		if p.Cmd == CmdAckUnauth {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("zk connect: unexpected response %d", p.Cmd)
	}
	return c, nil
}

// Close says goodbye to the device and closes the socket.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	_, _ = c.command(CmdExit, nil)
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *Client) send(cmd uint16, data []byte) error {
	pkt, used := EncodePacket(cmd, c.session, c.reply, data)
	c.reply = used
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.Timeout))
	_, err := c.conn.Write(Frame(pkt))
	return err
}

func (c *Client) readPacket() (Packet, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(c.Timeout))
	var hdr [tcpHeaderSize]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return Packet{}, err
	}
	if le.Uint16(hdr[0:]) != tcpMagic1 || le.Uint16(hdr[2:]) != tcpMagic2 {
		return Packet{}, fmt.Errorf("zk: bad TCP envelope % x", hdr)
	}
	n := le.Uint32(hdr[4:])
	if n < packetHeaderSize || n > maxPacketSize {
		return Packet{}, fmt.Errorf("zk: bad packet length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return Packet{}, err
	}
	return DecodePacket(buf)
}

// command sends a request and returns the device's response packet,
// skipping (and acknowledging) any unsolicited real-time event packets.
func (c *Client) command(cmd uint16, data []byte) (Packet, error) {
	if err := c.send(cmd, data); err != nil {
		return Packet{}, err
	}
	for {
		p, err := c.readPacket()
		if err != nil {
			return Packet{}, err
		}
		if p.Cmd == CmdRegEvent {
			c.ackEvent()
			continue
		}
		c.reply = p.Reply
		return p, nil
	}
}

func (c *Client) ackEvent() {
	pkt, _ := EncodePacket(CmdAckOK, c.session, initialReplyID, nil)
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.Timeout))
	_, _ = c.conn.Write(Frame(pkt))
}

func ok(p Packet) bool {
	return p.Cmd == CmdAckOK || p.Cmd == CmdPrepareData || p.Cmd == CmdData
}

func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return strings.TrimSpace(string(b))
}

// Option reads a configuration parameter such as "~SerialNumber".
func (c *Client) Option(name string) (string, error) {
	p, err := c.command(CmdOptionsRRQ, append([]byte(name), 0))
	if err != nil {
		return "", err
	}
	if !ok(p) {
		return "", fmt.Errorf("zk: option %q not available (response %d)", name, p.Cmd)
	}
	v := p.Data
	if i := bytes.IndexByte(v, '='); i >= 0 {
		v = v[i+1:]
	}
	return cString(bytes.ReplaceAll(v, []byte("="), nil)), nil
}

// SerialNumber returns the device serial number, used as the ADMS "SN".
func (c *Client) SerialNumber() (string, error) { return c.Option("~SerialNumber") }

// Firmware returns the firmware version string.
func (c *Client) Firmware() (string, error) {
	p, err := c.command(CmdGetVersion, nil)
	if err != nil {
		return "", err
	}
	if !ok(p) {
		return "", fmt.Errorf("zk: firmware version not available")
	}
	return cString(p.Data), nil
}

// Time returns the device clock.
func (c *Client) Time() (time.Time, error) {
	p, err := c.command(CmdGetTime, nil)
	if err != nil {
		return time.Time{}, err
	}
	if !ok(p) || len(p.Data) < 4 {
		return time.Time{}, fmt.Errorf("zk: can't read time")
	}
	return DecodeTime(le.Uint32(p.Data)), nil
}

// Restart reboots the device. The connection is unusable afterwards.
func (c *Client) Restart() error {
	err := c.send(CmdRestart, nil)
	c.conn.Close()
	c.conn = nil
	return err
}

// Sizes reads user/record counters.
func (c *Client) Sizes() (Sizes, error) {
	p, err := c.command(CmdGetFreeSizes, nil)
	if err != nil {
		return Sizes{}, err
	}
	if !ok(p) || len(p.Data) < 80 {
		return Sizes{}, fmt.Errorf("zk: can't read sizes (response %d, %d bytes)", p.Cmd, len(p.Data))
	}
	f := func(i int) int { return int(int32(le.Uint32(p.Data[i*4:]))) }
	return Sizes{Users: f(4), Fingers: f(6), Records: f(8), UsersCap: f(15), RecordsCap: f(16)}, nil
}

// readBuffered reads a whole data table using the buffered
// PREPARE_BUFFER / READ_BUFFER / FREE_DATA sequence.
func (c *Client) readBuffered(command uint16, fct, ext int32) ([]byte, error) {
	req := make([]byte, 11)
	req[0] = 1
	le.PutUint16(req[1:], command)
	le.PutUint32(req[3:], uint32(fct))
	le.PutUint32(req[7:], uint32(ext))

	p, err := c.command(CmdPrepareBuffer, req)
	if err != nil {
		return nil, err
	}
	switch {
	case p.Cmd == CmdData:
		return p.Data, nil
	case !ok(p) || len(p.Data) < 5:
		return nil, fmt.Errorf("zk: buffered read not supported (response %d)", p.Cmd)
	}
	size := int(le.Uint32(p.Data[1:]))
	out := make([]byte, 0, size)
	for start := 0; start < size; {
		n := min(size-start, maxBufferedChunk)
		chunk, err := c.readChunk(start, n)
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		start += n
	}
	if _, err := c.command(CmdFreeData, nil); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) readChunk(start, size int) ([]byte, error) {
	req := make([]byte, 8)
	le.PutUint32(req[0:], uint32(int32(start)))
	le.PutUint32(req[4:], uint32(int32(size)))
	p, err := c.command(CmdReadBuffer, req)
	if err != nil {
		return nil, err
	}
	switch p.Cmd {
	case CmdData:
		return p.Data, nil
	case CmdPrepareData:
		var out []byte
		for {
			q, err := c.readPacket()
			if err != nil {
				return nil, err
			}
			switch q.Cmd {
			case CmdData:
				out = append(out, q.Data...)
			case CmdAckOK:
				return out, nil
			case CmdRegEvent:
				c.ackEvent()
			default:
				return nil, fmt.Errorf("zk: unexpected packet %d while reading chunk", q.Cmd)
			}
		}
	default:
		return nil, fmt.Errorf("zk: can't read chunk %d:%d (response %d)", start, size, p.Cmd)
	}
}

type user struct {
	uid    int
	userID string
}

func (c *Client) users(count int) ([]user, error) {
	if count == 0 {
		return nil, nil
	}
	data, err := c.readBuffered(CmdUserTempRRQ, FctUser, 0)
	if err != nil {
		return nil, err
	}
	if len(data) <= 4 {
		return nil, nil
	}
	size := int(le.Uint32(data)) / count
	data = data[4:]
	var out []user
	switch size {
	case userRecordSize28:
		for ; len(data) >= size; data = data[size:] {
			out = append(out, user{int(le.Uint16(data)), strconv.FormatUint(uint64(le.Uint32(data[24:])), 10)})
		}
	case userRecordSize120:
		for ; len(data) >= size; data = data[size:] {
			out = append(out, user{int(le.Uint16(data)), cString(data[96:120])})
		}
	default:
		for ; len(data) >= userRecordSize72; data = data[userRecordSize72:] {
			out = append(out, user{int(le.Uint16(data)), cString(data[48:72])})
		}
	}
	return out, nil
}

var attCodeInit = []byte("\xff255\x00\x00\x00\x00\x00")

// Attendance downloads every attendance record stored on the device, oldest first.
func (c *Client) Attendance() ([]Record, error) {
	sz, err := c.Sizes()
	if err != nil {
		return nil, err
	}
	if sz.Records == 0 {
		return nil, nil
	}
	data, err := c.readBuffered(CmdAttLogRRQ, 0, 0)
	if err != nil {
		return nil, err
	}
	if len(data) < 4 {
		return nil, nil
	}
	recSize := int(le.Uint32(data)) / sz.Records
	data = data[4:]

	var out []Record
	switch recSize {
	case attRecordSize8:
		// Old firmware stores only the internal uid; map it to the user PIN.
		users, err := c.users(sz.Users)
		if err != nil {
			return nil, err
		}
		pin := map[int]string{}
		for _, u := range users {
			pin[u.uid] = u.userID
		}
		for ; len(data) >= recSize; data = data[recSize:] {
			uid := int(le.Uint16(data))
			id, found := pin[uid]
			if !found {
				id = strconv.Itoa(uid)
			}
			out = append(out, Record{UID: uid, UserID: id, Verify: int(data[2]), Time: DecodeTime(le.Uint32(data[3:])), Punch: int(data[7])})
		}
	case attRecordSize16:
		for ; len(data) >= recSize; data = data[recSize:] {
			id := le.Uint32(data)
			out = append(out, Record{UID: int(id), UserID: strconv.FormatUint(uint64(id), 10), Time: DecodeTime(le.Uint32(data[4:])), Verify: int(data[8]), Punch: int(data[9])})
		}
	default:
		if recSize < attRecordSize40 {
			recSize = attRecordSize40
		}
		for len(data) >= attRecordSize40 {
			if bytes.HasPrefix(data, attCodeInit) {
				data = data[len(attCodeInit):]
				if len(data) < attRecordSize40 {
					break
				}
			}
			out = append(out, Record{
				UID:    int(le.Uint16(data)),
				UserID: cString(data[2:26]),
				Verify: int(data[26]),
				Time:   DecodeTime(le.Uint32(data[27:])),
				Punch:  int(data[31]),
			})
			data = data[min(recSize, len(data)):]
		}
	}
	return out, nil
}

// IsNetErr reports whether err means the connection is broken and must be re-established.
func IsNetErr(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed)
}
