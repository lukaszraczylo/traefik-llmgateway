package traefikllmgateway

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// respDialTimeout, respReadTimeout, and respWriteTimeout bound every
// network operation respClient performs: connecting, writing a pipeline,
// and reading each reply. A hung or unreachable Redis-compatible server
// must never block a request indefinitely — the limiter's fail-open/
// fail-closed handling (limits.go) depends on a store call returning
// within a bounded time.
const (
	respDialTimeout  = 2 * time.Second
	respReadTimeout  = 2 * time.Second
	respWriteTimeout = 2 * time.Second
)

// respErr is a RESP2 error reply ("-message\r\n"). It implements error so a
// caller inspecting a decoded reply can type-assert for it the same way as
// any other error.
type respErr string

// Error implements the error interface.
func (e respErr) Error() string { return string(e) }

// respClient is a minimal stdlib-only RESP2 client for a single Redis-
// compatible server: one mutex-guarded TCP connection, lazily dialled on
// first use and reconnected at most once when a command round trip fails.
// It exists because the plugin runs interpreted under Yaegi with only the
// Go standard library available, so a full Redis client library is not an
// option.
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
// decoded replies in the same order. On any I/O error — including a
// failure to (re)connect — it closes the current connection, reconnects
// once, and retries the whole pipeline from scratch; a second failure is
// returned to the caller with the connection left closed, so the next call
// lazily reconnects fresh rather than retrying against a connection
// already known bad.
func (c *respClient) pipeline(cmds [][]string) ([]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	replies, err := c.attemptPipelineLocked(cmds)
	if err == nil {
		return replies, nil
	}

	c.closeLocked()
	replies, err = c.attemptPipelineLocked(cmds)
	if err != nil {
		c.closeLocked()
		return nil, err
	}
	return replies, nil
}

// attemptPipelineLocked runs one full attempt of cmds over c's connection,
// connecting first if needed. Callers must hold c.mu.
func (c *respClient) attemptPipelineLocked(cmds [][]string) ([]any, error) {
	if err := c.ensureConnLocked(); err != nil {
		return nil, err
	}

	var buf strings.Builder
	for _, args := range cmds {
		buf.WriteString(encodeCommand(args))
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(respWriteTimeout)); err != nil {
		return nil, fmt.Errorf("resp: set write deadline: %w", err)
	}
	if _, err := io.WriteString(c.conn, buf.String()); err != nil {
		return nil, fmt.Errorf("resp: write: %w", err)
	}

	replies := make([]any, len(cmds))
	for i := range cmds {
		if err := c.conn.SetReadDeadline(time.Now().Add(respReadTimeout)); err != nil {
			return nil, fmt.Errorf("resp: set read deadline: %w", err)
		}
		v, err := decodeReply(c.r)
		if err != nil {
			return nil, fmt.Errorf("resp: read reply %d/%d: %w", i+1, len(cmds), err)
		}
		replies[i] = v
	}
	return replies, nil
}

// ensureConnLocked dials, authenticates, and selects the database if c has
// no live connection. Callers must hold c.mu.
func (c *respClient) ensureConnLocked() error {
	if c.conn != nil {
		return nil
	}

	conn, err := net.DialTimeout("tcp", c.addr, respDialTimeout)
	if err != nil {
		return fmt.Errorf("resp: dial %q: %w", c.addr, err)
	}
	c.conn = conn
	c.r = bufio.NewReader(conn)

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
// requires a non-error reply. Callers must hold c.mu and have a live
// c.conn.
func (c *respClient) handshakeLocked(cmd, arg string) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(respWriteTimeout)); err != nil {
		return fmt.Errorf("resp: set write deadline: %w", err)
	}
	if _, err := io.WriteString(c.conn, encodeCommand([]string{cmd, arg})); err != nil {
		return fmt.Errorf("resp: %s: write: %w", cmd, err)
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(respReadTimeout)); err != nil {
		return fmt.Errorf("resp: set read deadline: %w", err)
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
// (short read, malformed line, unknown type byte) — a well-formed RESP
// error reply decodes successfully to a respErr value, not to a non-nil
// error, so a caller pipelining several commands can still read every
// reply after one of them fails server-side.
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
// field lenField, returning nil for a null bulk ("$-1").
func decodeBulk(r *bufio.Reader, lenField string) (any, error) {
	n, err := strconv.Atoi(lenField)
	if err != nil {
		return nil, fmt.Errorf("resp: malformed bulk length %q: %w", lenField, err)
	}
	if n < 0 {
		return nil, nil
	}
	buf := make([]byte, n+2) // payload plus trailing CRLF
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("resp: read bulk body: %w", err)
	}
	return buf[:n], nil
}

// decodeArray reads n elements given the already-parsed "*" length field
// lenField, returning nil for a null array ("*-1").
func decodeArray(r *bufio.Reader, lenField string) (any, error) {
	n, err := strconv.Atoi(lenField)
	if err != nil {
		return nil, fmt.Errorf("resp: malformed array length %q: %w", lenField, err)
	}
	if n < 0 {
		return nil, nil
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
// (or bare LF) stripped.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("resp: read line: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}
