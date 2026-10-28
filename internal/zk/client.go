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
	"unicode/utf8"
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

// User is one enrolled user as stored on the device. Raw keeps the whole
// record so a rewrite preserves the fields the relay does not manage
// (password, card, group).
type User struct {
	UID       int
	UserID    string // the PIN
	Name      string
	Privilege int // 0 user, 14 administrator
	Raw       []byte
}

// Users downloads every user record.
func (c *Client) Users() ([]User, error) {
	sz, err := c.Sizes()
	if err != nil {
		return nil, err
	}
	us, _, err := c.users(sz.Users)
	return us, err
}

// users returns the user records and their size on the wire (0 when the
// device has none, so the size can't be measured).
func (c *Client) users(count int) ([]User, int, error) {
	if count == 0 {
		return nil, 0, nil
	}
	data, err := c.readBuffered(CmdUserTempRRQ, FctUser, 0)
	if err != nil {
		return nil, 0, err
	}
	if len(data) <= 4 {
		return nil, 0, nil
	}
	size := int(le.Uint32(data)) / count
	if size != userRecordSize28 && size != userRecordSize120 {
		size = userRecordSize72
	}
	data = data[4:]
	var out []User
	for ; len(data) >= size; data = data[size:] {
		out = append(out, decodeUser(data[:size]))
	}
	return out, size, nil
}

func decodeUser(b []byte) User {
	u := User{UID: int(le.Uint16(b)), Privilege: int(b[2]), Raw: append([]byte(nil), b...)}
	switch len(b) {
	case userRecordSize28:
		u.Name = cString(b[8:16])
		u.UserID = strconv.FormatUint(uint64(le.Uint32(b[24:])), 10)
	case userRecordSize120:
		u.Name = cString(b[11:35])
		u.UserID = cString(b[96:120])
	default:
		u.Name = cString(b[11:35])
		u.UserID = cString(b[48:72])
	}
	return u
}

// SetUser creates or updates the user with the given PIN, keeping any
// password, card and group already stored for them, and their name when
// name is empty. An existing
// administrator is never demoted: losing the only admin would open the
// device menu to everyone.
func (c *Client) SetUser(pin, name string, privilege int) error {
	sz, err := c.Sizes()
	if err != nil {
		return err
	}
	users, size, err := c.users(sz.Users)
	if err != nil {
		return err
	}
	if size == 0 {
		if size, err = c.userRecordSize(); err != nil {
			return err
		}
	}
	if size == userRecordSize120 {
		return errors.New("zk: writing users is not supported on this device's user format")
	}
	var rec []byte
	maxUID := 0
	for _, u := range users {
		maxUID = max(maxUID, u.UID)
		if u.UserID == pin && len(u.Raw) == size {
			rec = append([]byte(nil), u.Raw...)
			if u.Privilege > privilege {
				privilege = u.Privilege
			}
		}
	}
	if rec == nil {
		if maxUID+1 > ushrtMax {
			return errors.New("zk: no free user slot")
		}
		rec = make([]byte, size)
		le.PutUint16(rec, uint16(maxUID+1))
		if err := putUserID(rec, pin); err != nil {
			return err
		}
	}
	rec[2] = byte(privilege)
	if name != "" {
		putName(rec, name)
	}
	// CMD_USER_WRQ takes the stored record layout as is (pyzk set_user).
	p, err := c.command(CmdUserWRQ, rec)
	if err != nil {
		return err
	}
	if p.Cmd != CmdAckOK {
		return fmt.Errorf("zk: device refused to save user %s (response %d)", pin, p.Cmd)
	}
	return c.refresh()
}

// DeleteUser removes the user with the given PIN. A PIN the device doesn't
// hold is not an error.
func (c *Client) DeleteUser(pin string) error {
	users, err := c.Users()
	if err != nil {
		return err
	}
	for _, u := range users {
		if u.UserID != pin {
			continue
		}
		b := make([]byte, 2)
		le.PutUint16(b, uint16(u.UID))
		p, err := c.command(CmdDeleteUser, b)
		if err != nil {
			return err
		}
		if p.Cmd != CmdAckOK {
			return fmt.Errorf("zk: device refused to delete user %s (response %d)", pin, p.Cmd)
		}
		return c.refresh()
	}
	return nil
}

func (c *Client) refresh() error {
	p, err := c.command(CmdRefreshData, nil)
	if err == nil && p.Cmd != CmdAckOK {
		err = fmt.Errorf("zk: refresh failed (response %d)", p.Cmd)
	}
	return err
}

// userRecordSize infers the user record layout of a device with no users
// from its attendance records: 40-byte records go with 72-byte users,
// the older 8/16-byte ones with 28-byte users.
func (c *Client) userRecordSize() (int, error) {
	recs, size, err := c.attendance()
	if err != nil {
		return 0, err
	}
	if len(recs) > 0 && size != attRecordSize40 {
		return userRecordSize28, nil
	}
	return userRecordSize72, nil
}

func putUserID(rec []byte, pin string) error {
	switch len(rec) {
	case userRecordSize28:
		n, err := strconv.ParseUint(pin, 10, 32)
		if err != nil {
			return fmt.Errorf("zk: this device only takes numeric PINs, got %q", pin)
		}
		le.PutUint32(rec[24:], uint32(n))
	case userRecordSize120:
		putString(rec[96:120], pin)
	default:
		putString(rec[48:72], pin)
	}
	return nil
}

func putName(rec []byte, name string) {
	if len(rec) == userRecordSize28 {
		putString(rec[8:16], name)
		return
	}
	putString(rec[11:35], name)
}

// putString writes s NUL-padded into field, truncated on a UTF-8 boundary
// to leave a NUL.
func putString(field []byte, s string) {
	clear(field)
	for len(s) > len(field)-1 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	copy(field, s)
}

var attCodeInit = []byte("\xff255\x00\x00\x00\x00\x00")

// Attendance downloads every attendance record stored on the device, oldest first.
func (c *Client) Attendance() ([]Record, error) {
	recs, _, err := c.attendance()
	return recs, err
}

// attendance also returns the record size. The size is the table length
// divided by the record counter, so a punch landing between the two reads
// would skew it: the counter is read again afterwards and the download
// retried until both reads agree.
func (c *Client) attendance() ([]Record, int, error) {
	for attempt := 0; ; attempt++ {
		before, err := c.Sizes()
		if err != nil {
			return nil, 0, err
		}
		if before.Records == 0 {
			return nil, 0, nil
		}
		data, err := c.readBuffered(CmdAttLogRRQ, 0, 0)
		if err != nil {
			return nil, 0, err
		}
		after, err := c.Sizes()
		if err != nil {
			return nil, 0, err
		}
		if after.Records != before.Records && attempt < 3 {
			continue
		}
		if len(data) < 4 {
			return nil, 0, nil
		}
		recSize := int(le.Uint32(data)) / before.Records
		recs, err := c.decodeAttendance(data[4:], recSize, before.Users)
		return recs, recSize, err
	}
}

func (c *Client) decodeAttendance(data []byte, recSize, userCount int) ([]Record, error) {
	var out []Record
	switch recSize {
	case attRecordSize8:
		// Old firmware stores only the internal uid; map it to the user PIN.
		users, _, err := c.users(userCount)
		if err != nil {
			return nil, err
		}
		pin := map[int]string{}
		for _, u := range users {
			pin[u.UID] = u.UserID
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
