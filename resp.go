package traefikllmgateway

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// respDialTimeout bounds connecting to the server when no connection
// exists yet. respCallTimeout bounds everything that happens on the
// connection during one do/pipeline call: the AUTH/SELECT handshake (on a
// freshly dialled connection) and the call's own command write and reply
// reads. It is computed ONCE per call and applied via a single
// conn.SetDeadline, not reset before each individual read or write — a
// server that accepts the connection and then never answers must cost
// this one call ~respCallTimeout, not respCallTimeout multiplied by every
// handshake and command step it happens to perform (which, with the
// mutex serializing every caller behind one connection, previously turned
// one hung server into a many-times-respCallTimeout stall per request).
const (
	respDialTimeout = 2 * time.Second
	respCallTimeout = 2 * time.Second
)

// respMaxBulkLen caps a RESP2 bulk string's declared length. Without a
// cap, a malformed or malicious reply — "$9223372036854775807\r\n" — makes
// n+2 overflow into a negative slice length and panic in make([]byte,
// n+2); a reply merely claiming a huge-but-valid length would otherwise
// force a huge allocation. 64MiB is generous for anything this client
// actually reads (integer counter values and short simple/error strings).
const respMaxBulkLen = 64 << 20

// respMaxArrayLen caps a RESP2 array's declared element count, for the
// same reason as respMaxBulkLen: this client only ever pipelines a
// handful of commands, so a reply claiming millions of elements is
// malformed or hostile, not legitimate.
const respMaxArrayLen = 1 << 20

// respMaxLineLen caps a single RESP2 header line (the "+", "-", ":", "$",
// or "*" line preceding any body) read by readLine. Without a cap, a peer
// that never sends '\n' makes readLine buffer an unbounded number of
// bytes before erroring.
const respMaxLineLen = 64 << 10

// respErr is a RESP2 error reply ("-message\r\n"). It implements error so a
// caller inspecting a decoded reply can type-assert for it the same way as
// any other error.
type respErr string

// Error implements the error interface.
func (e respErr) Error() string { return string(e) }

// respClient is a minimal stdlib-only RESP2 client for a single Redis-
// compatible server: one mutex-guarded TCP connection, lazily dialled on
// first use and reconnected at most once when a command round trip fails
// for a reason other than a timeout (see pipeline). It exists because the
// plugin runs interpreted under Yaegi with only the Go standard library
// available, so a full Redis client library is not an option.
//
// mu serializes every operation — do and pipeline hold it for their whole
// round trip, including any reconnect — since RESP is not multiplexed:
// interleaving two callers' writes on one connection would corrupt both
// replies.
type respClient struct {
	conn     net.Conn
	r        *bufio.Reader
	addr     string
	password string
	mu       sync.Mutex
	db       int
}

// newRESPClient returns a respClient for addr. It does not connect until
// the first do or pipeline call. password, if non-empty, is sent via AUTH
// on every new connection; db is always sent via SELECT on every new
// connection (including db 0), since a Redis-compatible proxy is not
// guaranteed to default a fresh connection to database 0.
func newRESPClient(addr, password string, db int) *respClient {
	return &respClient{addr: addr, password: password, db: db}
}

// do sends one command and returns its decoded reply. It is equivalent to
// pipeline with a single command.
func (c *respClient) do(args ...string) (any, error) {
	replies, err := c.pipeline([][]string{args})
	if err != nil {
		return nil, err
	}
	return replies[0], nil
}

// pipeline sends every command in cmds on one round trip and returns their
// decoded replies in the same order.
//
// One absolute deadline is computed here, at call entry, and shared by
// both the first attempt and (if it happens) the one retry — not a fresh
// respCallTimeout for each. On a non-timeout I/O error (connection reset,
// EOF from a peer that closed mid-command) it closes the connection and
// retries the whole pipeline once against the same deadline. On a timeout
// specifically, it does not retry: a server that accepted the connection
// and then went silent would just be given the same non-answer a second
// time, doubling the caller's wait for nothing. Either way, a failure
// leaves the connection closed so the next call lazily reconnects fresh
// rather than retrying against a connection already known bad.
func (c *respClient) pipeline(cmds [][]string) ([]any, error) {
	deadline := time.Now().Add(respCallTimeout)

	c.mu.Lock()
	defer c.mu.Unlock()

	replies, err := c.attemptPipelineLocked(cmds, deadline)
	if err == nil {
		return replies, nil
	}
	c.closeLocked()
	if isTimeout(err) {
		return nil, err
	}

	replies, err = c.attemptPipelineLocked(cmds, deadline)
	if err != nil {
		c.closeLocked()
		return nil, err
	}
	return replies, nil
}

// isTimeout reports whether err is (or wraps) a net.Error that timed out.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// attemptPipelineLocked runs one full attempt of cmds over c's connection,
// connecting first if needed, with every read and write bound by
// deadline. Callers must hold c.mu.
func (c *respClient) attemptPipelineLocked(cmds [][]string, deadline time.Time) ([]any, error) {
	if err := c.ensureConnLocked(deadline); err != nil {
		return nil, err
	}

	var buf strings.Builder
	for _, args := range cmds {
		buf.WriteString(encodeCommand(args))
	}
	if _, err := io.WriteString(c.conn, buf.String()); err != nil {
		return nil, fmt.Errorf("resp: write: %w", err)
	}

	replies := make([]any, len(cmds))
	for i := range cmds {
		v, err := decodeReply(c.r)
		if err != nil {
			return nil, fmt.Errorf("resp: read reply %d/%d: %w", i+1, len(cmds), err)
		}
		replies[i] = v
	}
	return replies, nil
}

// ensureConnLocked dials, authenticates, and selects the database if c has
// no live connection, then (whether freshly dialled or reused) sets
// deadline as the connection's single read/write deadline for the
// remainder of this call — including the handshake, when one runs.
// Callers must hold c.mu.
func (c *respClient) ensureConnLocked(deadline time.Time) error {
	if c.conn != nil {
		if err := c.conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("resp: set deadline: %w", err)
		}
		return nil
	}

	conn, err := net.DialTimeout("tcp", c.addr, respDialTimeout)
	if err != nil {
		return fmt.Errorf("resp: dial %q: %w", c.addr, err)
	}
	c.conn = conn
	c.r = bufio.NewReader(conn)

	if err := c.conn.SetDeadline(deadline); err != nil {
		c.closeLocked()
		return fmt.Errorf("resp: set deadline: %w", err)
	}

	if c.password != "" {
		if err := c.handshakeLocked("AUTH", c.password); err != nil {
			c.closeLocked()
			return err
		}
	}
	if err := c.handshakeLocked("SELECT", strconv.Itoa(c.db)); err != nil {
		c.closeLocked()
		return err
	}
	return nil
}

// handshakeLocked sends one connection-setup command (AUTH or SELECT) and
// requires a non-error reply. Callers must hold c.mu, have a live c.conn,
// and have already set its deadline.
func (c *respClient) handshakeLocked(cmd, arg string) error {
	if _, err := io.WriteString(c.conn, encodeCommand([]string{cmd, arg})); err != nil {
		return fmt.Errorf("resp: %s: write: %w", cmd, err)
	}
	v, err := decodeReply(c.r)
	if err != nil {
		return fmt.Errorf("resp: %s: read reply: %w", cmd, err)
	}
	if e, ok := v.(respErr); ok {
		return fmt.Errorf("resp: %s failed: %w", cmd, e)
	}
	return nil
}

// closeLocked closes and clears c's connection, if any. Callers must hold
// c.mu.
func (c *respClient) closeLocked() {
	if c.conn == nil {
		return
	}
	_ = c.conn.Close()
	c.conn = nil
	c.r = nil
}

// encodeCommand renders args as a RESP2 command array:
// "*N\r\n$len\r\narg\r\n..." for each arg.
func encodeCommand(args []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return b.String()
}

// decodeReply reads and decodes one RESP2 reply from r:
//
//   - "+simple\r\n"  -> string
//   - "-error\r\n"   -> respErr (implements error; not returned as err)
//   - ":123\r\n"     -> int64
//   - "$len\r\n...\r\n" -> []byte, or nil for a null bulk ("$-1\r\n")
//   - "*n\r\n..."    -> []any of n decoded elements, or nil for a null
//     array ("*-1\r\n")
//
// The returned error is non-nil only for a transport or protocol failure
// (short read, malformed line, unknown type byte, an out-of-range or
// mistrailed bulk/array) — a well-formed RESP error reply decodes
// successfully to a respErr value, not to a non-nil error, so a caller
// pipelining several commands can still read every reply after one of
// them fails server-side.
func decodeReply(r *bufio.Reader) (any, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	if line == "" {
		return nil, fmt.Errorf("resp: empty reply line")
	}

	prefix, rest := line[0], line[1:]
	switch prefix {
	case '+':
		return rest, nil
	case '-':
		return respErr(rest), nil
	case ':':
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("resp: malformed integer reply %q: %w", rest, err)
		}
		return n, nil
	case '$':
		return decodeBulk(r, rest)
	case '*':
		return decodeArray(r, rest)
	default:
		return nil, fmt.Errorf("resp: unknown reply type %q", prefix)
	}
}

// decodeBulk reads a bulk string body given its already-parsed "$" length
// field lenField, returning nil for a null bulk ("$-1"). It rejects a
// negative length other than -1 and a length exceeding respMaxBulkLen
// before allocating — n+2 would otherwise be able to overflow into a
// negative slice length for a maliciously huge declared length, panicking
// make([]byte, n+2) — and verifies the two bytes following the payload
// are the expected CRLF trailer.
func decodeBulk(r *bufio.Reader, lenField string) (any, error) {
	n, err := strconv.Atoi(lenField)
	if err != nil {
		return nil, fmt.Errorf("resp: malformed bulk length %q: %w", lenField, err)
	}
	if n == -1 {
		return nil, nil
	}
	if n < -1 || n > respMaxBulkLen {
		return nil, fmt.Errorf("resp: bulk length %d out of range (max %d)", n, respMaxBulkLen)
	}

	buf := make([]byte, n+2) // payload plus trailing CRLF; n is bounded above, so n+2 cannot overflow
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("resp: read bulk body: %w", err)
	}
	if buf[n] != '\r' || buf[n+1] != '\n' {
		return nil, fmt.Errorf("resp: bulk reply missing CRLF trailer")
	}
	return buf[:n], nil
}

// decodeArray reads n elements given the already-parsed "*" length field
// lenField, returning nil for a null array ("*-1"). It rejects a negative
// length other than -1 and a length exceeding respMaxArrayLen before
// allocating, for the same reason as decodeBulk's cap.
func decodeArray(r *bufio.Reader, lenField string) (any, error) {
	n, err := strconv.Atoi(lenField)
	if err != nil {
		return nil, fmt.Errorf("resp: malformed array length %q: %w", lenField, err)
	}
	if n == -1 {
		return nil, nil
	}
	if n < -1 || n > respMaxArrayLen {
		return nil, fmt.Errorf("resp: array length %d out of range (max %d)", n, respMaxArrayLen)
	}

	arr := make([]any, n)
	for i := 0; i < n; i++ {
		v, err := decodeReply(r)
		if err != nil {
			return nil, fmt.Errorf("resp: array element %d/%d: %w", i+1, n, err)
		}
		arr[i] = v
	}
	return arr, nil
}

// readLine reads one CRLF-terminated line from r, with the trailing CRLF
// (or bare LF) stripped. It reads a byte at a time so it can bail out
// after respMaxLineLen bytes without ever finding '\n' — a peer that
// never terminates a line must not make this buffer an unbounded number
// of bytes first and only reject the result afterward.
func readLine(r *bufio.Reader) (string, error) {
	buf := make([]byte, 0, 64)
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", fmt.Errorf("resp: read line: %w", err)
		}
		if b == '\n' {
			return strings.TrimSuffix(string(buf), "\r"), nil
		}
		if len(buf) >= respMaxLineLen {
			return "", fmt.Errorf("resp: reply line exceeds %d bytes", respMaxLineLen)
		}
		buf = append(buf, b)
	}
}
