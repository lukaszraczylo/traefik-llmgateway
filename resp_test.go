package traefikllmgateway

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEncodeCommand pins the exact RESP2 wire format every command must
// use: "*N\r\n$len\r\narg\r\n..." — a fake or real server parses this
// framing byte-for-byte, so a drift here breaks every other test silently.
func TestEncodeCommand(t *testing.T) {
	cases := []struct {
		name string
		want string
		args []string
	}{
		{name: "single arg", args: []string{"GET"}, want: "*1\r\n$3\r\nGET\r\n"},
		{name: "multiple args", args: []string{"SET", "k", "v"}, want: "*3\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n"},
		{name: "empty arg", args: []string{"SELECT", ""}, want: "*2\r\n$6\r\nSELECT\r\n$0\r\n\r\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := encodeCommand(c.args); got != c.want {
				t.Errorf("encodeCommand(%v) = %q, want %q", c.args, got, c.want)
			}
		})
	}
}

// TestDecodeReply covers every RESP2 type respClient must decode: simple
// string, error, integer, bulk string (including the null bulk "$-1"), and
// a single top-level array (including the null array "*-1"). Every call
// passes depth 0 — a top-level reply, matching every real call site in
// resp.go (attemptPipelineOn, handshakeOn, and decodeArray's own
// element loop, which passes depth+1).
func TestDecodeReply(t *testing.T) {
	t.Run("simple string", func(t *testing.T) {
		v, err := decodeReply(newReader("+OK\r\n"), 0)
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		if v != "OK" {
			t.Errorf("v = %#v, want %q", v, "OK")
		}
	})

	t.Run("error reply decodes to respErr, not a decode error", func(t *testing.T) {
		v, err := decodeReply(newReader("-ERR wrong number of arguments\r\n"), 0)
		if err != nil {
			t.Fatalf("decodeReply returned an error for a well-formed RESP error reply: %v", err)
		}
		e, ok := v.(respErr)
		if !ok {
			t.Fatalf("v = %#v (%T), want respErr", v, v)
		}
		if e.Error() != "ERR wrong number of arguments" {
			t.Errorf("e.Error() = %q, want %q", e.Error(), "ERR wrong number of arguments")
		}
	})

	t.Run("integer", func(t *testing.T) {
		v, err := decodeReply(newReader(":42\r\n"), 0)
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		if v != int64(42) {
			t.Errorf("v = %#v, want int64(42)", v)
		}
	})

	t.Run("negative integer", func(t *testing.T) {
		v, err := decodeReply(newReader(":-7\r\n"), 0)
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		if v != int64(-7) {
			t.Errorf("v = %#v, want int64(-7)", v)
		}
	})

	t.Run("bulk string", func(t *testing.T) {
		v, err := decodeReply(newReader("$5\r\nhello\r\n"), 0)
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		b, ok := v.([]byte)
		if !ok || !bytes.Equal(b, []byte("hello")) {
			t.Errorf("v = %#v, want []byte(\"hello\")", v)
		}
	})

	t.Run("null bulk (missing key)", func(t *testing.T) {
		v, err := decodeReply(newReader("$-1\r\n"), 0)
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		if v != nil {
			t.Errorf("v = %#v, want nil", v)
		}
	})

	t.Run("array (flat, not nested)", func(t *testing.T) {
		v, err := decodeReply(newReader("*2\r\n:5\r\n:1\r\n"), 0)
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		arr, ok := v.([]any)
		if !ok || len(arr) != 2 || arr[0] != int64(5) || arr[1] != int64(1) {
			t.Errorf("v = %#v, want []any{int64(5), int64(1)}", v)
		}
	})

	t.Run("null array", func(t *testing.T) {
		v, err := decodeReply(newReader("*-1\r\n"), 0)
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		if v != nil {
			t.Errorf("v = %#v, want nil", v)
		}
	})

	t.Run("malformed type byte is a decode error", func(t *testing.T) {
		if _, err := decodeReply(newReader("?nope\r\n"), 0); err == nil {
			t.Fatal("want error for an unknown reply type byte")
		}
	})

	// The following subtests are review item 3 (round 1): decodeBulk/
	// decodeArray must reject an out-of-range wire length before
	// allocating, and decodeBulk must validate its CRLF trailer.

	t.Run("oversized bulk length is a protocol error, not a panic", func(t *testing.T) {
		// n+2 (9223372036854775807+2) overflows int64 into a negative
		// slice length; make([]byte, n+2) with that unguarded would
		// panic. Reaching this line without panicking already proves the
		// fix; the error check confirms it fails cleanly too.
		if _, err := decodeReply(newReader("$9223372036854775807\r\n"), 0); err == nil {
			t.Fatal("want an error for a bulk length exceeding respMaxBulkLen")
		}
	})

	t.Run("bulk length just over the cap is a protocol error", func(t *testing.T) {
		if _, err := decodeReply(newReader(fmt.Sprintf("$%d\r\n", respMaxBulkLen+1)), 0); err == nil {
			t.Fatal("want an error for a bulk length one over respMaxBulkLen")
		}
	})

	t.Run("huge array length is a protocol error", func(t *testing.T) {
		if _, err := decodeReply(newReader("*1000000000\r\n"), 0); err == nil {
			t.Fatal("want an error for an array length exceeding respMaxArrayLen")
		}
	})

	t.Run("bulk with wrong trailer is a protocol error", func(t *testing.T) {
		if _, err := decodeReply(newReader("$5\r\nhelloXX"), 0); err == nil {
			t.Fatal("want an error when a bulk reply's trailing bytes are not CRLF")
		}
	})

	t.Run("reply line exceeding the cap is a protocol error, not unbounded buffering", func(t *testing.T) {
		// No trailing "\r\n" at all: without the byte-by-byte cap check,
		// readLine would keep buffering every byte offered until the
		// reader errors (e.g. EOF) rather than bailing out early.
		huge := strings.Repeat("x", respMaxLineLen+10)
		if _, err := decodeReply(newReader("+"+huge), 0); err == nil {
			t.Fatal("want an error for a reply line exceeding respMaxLineLen")
		}
	})

	// The following subtests are review item 1 (round 3): decodeReply
	// must reject an array nested inside another array's elements — the
	// only way a reply could otherwise recurse — and the array cap must
	// be the new, much lower respMaxArrayLen (1024), since no command
	// this client sends ever gets an array reply back at all.

	t.Run("nested array reply is a protocol error, not unbounded recursion", func(t *testing.T) {
		// A one-element array whose element is itself a one-element array.
		// Repeated, this pattern is what would otherwise recurse to Go's
		// stack limit (a fatal, unrecoverable error) from only a few
		// megabytes of wire input; one level already exercises the guard,
		// since decodeReply rejects any '*' at depth > 0 outright.
		if _, err := decodeReply(newReader("*1\r\n*1\r\n:1\r\n"), 0); err == nil {
			t.Fatal("want an error for an array nested inside another array")
		}
	})

	t.Run("array length just over the new (1024) cap is a protocol error", func(t *testing.T) {
		if _, err := decodeReply(newReader(fmt.Sprintf("*%d\r\n", respMaxArrayLen+1)), 0); err == nil {
			t.Fatal("want an error for an array length one over respMaxArrayLen")
		}
	})
}

// newReader wraps s in a *bufio.Reader, matching what respClient reads
// from its connection.
func newReader(s string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(s))
}

// respStep is one entry in a fakeRESPServer script.
type respStep struct {
	// wantArgs, if non-nil, asserts the next command read equals this
	// exactly before replying.
	wantArgs []string
	// reply is written back verbatim; ignored when closeConn is true.
	reply []byte
	// closeConn closes the connection instead of replying, simulating a
	// server that drops mid-command. The next step (if any) accepts a new
	// connection.
	closeConn bool
}

// runFakeRESPServer executes script against ln in a background goroutine.
// It accepts a new connection whenever the previous one ends (either
// because a step set closeConn, or because the client itself closed it),
// so a script can simulate a server that drops a connection mid-command
// and expect the client to reconnect for the remaining steps. Mismatches
// are reported via t.Errorf, which is safe to call from a goroutine.
func runFakeRESPServer(t *testing.T, ln net.Listener, script []respStep) {
	t.Helper()
	go func() {
		var conn net.Conn
		var r *bufio.Reader
		for _, step := range script {
			if conn == nil {
				c, err := ln.Accept()
				if err != nil {
					return // listener closed by t.Cleanup; test is finishing
				}
				conn = c
				r = bufio.NewReader(conn)
			}

			args, err := readRESPCommand(r)
			if err != nil {
				t.Errorf("fake RESP server: read command: %v", err)
				return
			}
			if step.wantArgs != nil && !equalStrSlices(args, step.wantArgs) {
				t.Errorf("fake RESP server: got command %v, want %v", args, step.wantArgs)
			}

			if step.closeConn {
				_ = conn.Close()
				conn, r = nil, nil
				continue
			}
			if _, err := conn.Write(step.reply); err != nil {
				t.Errorf("fake RESP server: write reply: %v", err)
				return
			}
		}
	}()
}

// readRESPCommand reads one RESP2 command array ("*N\r\n$len\r\narg\r\n...")
// from r — the same framing encodeCommand produces — and returns its args.
func readRESPCommand(r *bufio.Reader) ([]string, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, &strconvLikeError{"want array header, got " + strconv.Quote(line)}
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, n)
	for i := 0; i < n; i++ {
		lenLine, err := readLine(r)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(lenLine, "$") {
			return nil, &strconvLikeError{"want bulk header, got " + strconv.Quote(lenLine)}
		}
		l, err := strconv.Atoi(lenLine[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, l+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:l])
	}
	return args, nil
}

// strconvLikeError is a trivial error string type for readRESPCommand's
// framing-mismatch messages.
type strconvLikeError struct{ msg string }

func (e *strconvLikeError) Error() string { return e.msg }

// equalStrSlices reports whether a and b hold the same strings in the same
// order.
func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// newFakeListener returns a TCP listener on 127.0.0.1 with an OS-assigned
// port, closed automatically at test cleanup.
func newFakeListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// newHungListener returns a listener whose accepted connections read (and
// discard) anything the client sends but never reply — simulating a
// server that accepted the TCP connection and then went silent (a
// black-holed network path, a wedged process), as opposed to actively
// refusing or resetting. Used by review item 1's tests to prove a call's
// latency is bounded by respCallTimeout rather than left to block
// indefinitely. Each accepted connection's reader goroutine exits once
// the connection is closed — by the client's own closeConn after its
// deadline fires, or by the listener's t.Cleanup at test end.
func newHungListener(t *testing.T) net.Listener {
	t.Helper()
	ln := newFakeListener(t)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln
}

// TestRESPClient_ConnectSendsAuthThenSelect is the brief's Step-1
// connection-setup case: a configured password sends AUTH before SELECT,
// both requiring a non-error reply, and SELECT is sent with the
// configured db even when db is 0 — a fresh connection to a Redis-
// compatible proxy is not guaranteed to already sit on database 0.
func TestRESPClient_ConnectSendsAuthThenSelect(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"AUTH", "pw"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "missing-key"}, reply: []byte("$-1\r\n")},
	})

	c := newRESPClient(ln.Addr().String(), "pw", 0)
	v, err := c.do("GET", "missing-key")
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if v != nil {
		t.Errorf("v = %#v, want nil (null bulk for a missing key)", v)
	}
}

// TestRESPClient_NoPasswordSkipsAuth asserts an empty password never sends
// AUTH — the first command on the wire is SELECT.
func TestRESPClient_NoPasswordSkipsAuth(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "3"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "k"}, reply: []byte("$1\r\nv\r\n")},
	})

	c := newRESPClient(ln.Addr().String(), "", 3)
	v, err := c.do("GET", "k")
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if b, ok := v.([]byte); !ok || string(b) != "v" {
		t.Errorf("v = %#v, want []byte(\"v\")", v)
	}
}

// TestRESPClient_ReconnectsOnceOnMidCommandClose is the brief's Step-1
// reconnect case: the server drops the connection after reading a command
// but before replying (simulating a network failure mid-round-trip). The
// client must reconnect once — replaying the connection handshake — and
// retry the command, succeeding on the second attempt.
func TestRESPClient_ReconnectsOnceOnMidCommandClose(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		// First connection: handshake succeeds, then GET is read but the
		// server closes without ever writing a reply.
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "k"}, closeConn: true},
		// Second connection: the client must redo the handshake, then
		// resend GET and this time get a reply.
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "k"}, reply: []byte("$2\r\nv2\r\n")},
	})

	c := newRESPClient(ln.Addr().String(), "", 0)
	v, err := c.do("GET", "k")
	if err != nil {
		t.Fatalf("do: %v (want the client to reconnect once and succeed)", err)
	}
	if b, ok := v.([]byte); !ok || string(b) != "v2" {
		t.Errorf("v = %#v, want []byte(\"v2\")", v)
	}
}

// TestRESPClient_ReconnectFailsTwice_ReturnsError asserts a client gives
// up (rather than retrying forever) once the retried attempt also fails:
// dialing a dead address never succeeds, so both the first attempt and the
// one reconnect attempt fail, and do returns an error.
func TestRESPClient_ReconnectFailsTwice_ReturnsError(t *testing.T) {
	ln := newFakeListener(t)
	deadAddr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	} // nothing listens at deadAddr from here on

	c := newRESPClient(deadAddr, "", 0)
	if _, err := c.do("GET", "k"); err == nil {
		t.Fatal("want an error dialing a dead address")
	}
}

// TestRESPClient_HungServer_BoundedByCallDeadline is review item 1: a
// server that accepts the connection and then never replies must not
// block a call past respCallTimeout, and — because that failure is a
// timeout, not a reset/refusal — must not be retried, which would double
// the wait for the exact same non-answer.
func TestRESPClient_HungServer_BoundedByCallDeadline(t *testing.T) {
	ln := newHungListener(t)
	c := newRESPClient(ln.Addr().String(), "", 0)

	start := time.Now()
	_, err := c.do("GET", "k")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error from a server that never replies")
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("elapsed = %v, want < 3s (one bounded call deadline, no retry-doubling on timeout)", elapsed)
	}
}

// --- setEx / getBytes: the response cache's RESP primitives (task 2) ---

// TestRESPClient_SetEx_SendsSETWithEXAndWholeSeconds asserts setEx frames
// the command as "SET key val EX seconds", rounding a sub-second ttl up to
// the next whole second — Redis's EX argument is whole seconds only,
// matching redisStore.incrBy's own EXPIRE rounding.
func TestRESPClient_SetEx_SendsSETWithEXAndWholeSeconds(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"SET", "llmgw:cache:abc", "hello", "EX", "5"}, reply: []byte("+OK\r\n")},
	})

	c := newRESPClient(ln.Addr().String(), "", 0)
	if err := c.setEx("llmgw:cache:abc", []byte("hello"), 4500*time.Millisecond); err != nil {
		t.Fatalf("setEx: %v", err)
	}
}

// TestRESPClient_SetEx_FloorsSubSecondTTLAtOneSecond asserts a ttl under
// one second is never sent as EX 0 (Redis would delete the key
// immediately).
func TestRESPClient_SetEx_FloorsSubSecondTTLAtOneSecond(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"SET", "k", "v", "EX", "1"}, reply: []byte("+OK\r\n")},
	})

	c := newRESPClient(ln.Addr().String(), "", 0)
	if err := c.setEx("k", []byte("v"), 200*time.Millisecond); err != nil {
		t.Fatalf("setEx: %v", err)
	}
}

// TestRESPClient_SetEx_ServerErrorReplyIsReturned asserts a RESP error
// reply to SET surfaces as a Go error, not a silently-successful call.
func TestRESPClient_SetEx_ServerErrorReplyIsReturned(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"SET", "k", "v", "EX", "5"}, reply: []byte("-ERR oom\r\n")},
	})

	c := newRESPClient(ln.Addr().String(), "", 0)
	if err := c.setEx("k", []byte("v"), 5*time.Second); err == nil {
		t.Fatal("want an error for a RESP error reply to SET")
	}
}

// TestRESPClient_GetBytes_HitReturnsBody covers the cache-hit case: a
// bulk reply decodes to the stored bytes with found=true.
func TestRESPClient_GetBytes_HitReturnsBody(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "k"}, reply: []byte("$5\r\nhello\r\n")},
	})

	c := newRESPClient(ln.Addr().String(), "", 0)
	b, found, err := c.getBytes("k")
	if err != nil {
		t.Fatalf("getBytes: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	if string(b) != "hello" {
		t.Errorf("b = %q, want %q", b, "hello")
	}
}

// TestRESPClient_GetBytes_MissReturnsFoundFalse covers the null-bulk
// ("$-1") case: a missing key is a miss, not an error.
func TestRESPClient_GetBytes_MissReturnsFoundFalse(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "missing"}, reply: []byte("$-1\r\n")},
	})

	c := newRESPClient(ln.Addr().String(), "", 0)
	b, found, err := c.getBytes("missing")
	if err != nil {
		t.Fatalf("getBytes: %v", err)
	}
	if found {
		t.Fatal("found = true, want false for a null bulk reply")
	}
	if b != nil {
		t.Errorf("b = %#v, want nil", b)
	}
}

// TestRESPClient_GetBytes_ServerErrorReplyIsReturned asserts a RESP error
// reply to GET surfaces as a Go error, not a false miss.
func TestRESPClient_GetBytes_ServerErrorReplyIsReturned(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "k"}, reply: []byte("-ERR busy\r\n")},
	})

	c := newRESPClient(ln.Addr().String(), "", 0)
	if _, _, err := c.getBytes("k"); err == nil {
		t.Fatal("want an error for a RESP error reply to GET")
	}
}

// TestRESPClient_GetBytes_DownServer_ReturnsError asserts getBytes
// surfaces a transport failure (dead address) as an error, matching do's
// own contract — the response cache (cache.go) relies on this to treat a
// Redis outage as a miss rather than block or panic.
func TestRESPClient_GetBytes_DownServer_ReturnsError(t *testing.T) {
	ln := newFakeListener(t)
	deadAddr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c := newRESPClient(deadAddr, "", 0)
	if _, _, err := c.getBytes("k"); err == nil {
		t.Fatal("want an error dialing a dead address")
	}
}

// --- fakeConn: a net.Conn whose Write/SetDeadline fail on command, for
// the handful of respClient error branches a real TCP connection cannot be
// coaxed into deterministically (a write failing on an otherwise-live
// connection, SetDeadline failing on a reused connection) ---

// fakeAddr is a trivial net.Addr for fakeConn's LocalAddr/RemoteAddr.
type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// fakeConn implements net.Conn, returning writeErr from Write and
// setDeadlineErr from SetDeadline when set, so a test can drive respClient
// methods directly against a connection already known bad — bypassing
// ensureConnOn's real net.DialTimeout, which always succeeds against a
// live listener and so cannot itself be made to fail this way.
type fakeConn struct {
	writeErr       error
	setDeadlineErr error
}

func (fakeConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c fakeConn) Write(b []byte) (int, error) {
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(b), nil
}

func (fakeConn) Close() error         { return nil }
func (fakeConn) LocalAddr() net.Addr  { return fakeAddr{} }
func (fakeConn) RemoteAddr() net.Addr { return fakeAddr{} }

func (c fakeConn) SetDeadline(time.Time) error {
	if c.setDeadlineErr != nil {
		return c.setDeadlineErr
	}
	return nil
}

func (fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (fakeConn) SetWriteDeadline(time.Time) error { return nil }

// closeCountingConn wraps fakeConn, incrementing *count on every Close
// call — used to prove a method actually closed a connection, not just
// forgot about (or merely drained a channel referencing) it.
type closeCountingConn struct {
	fakeConn
	count *int
}

func (c *closeCountingConn) Close() error {
	*c.count++
	return c.fakeConn.Close()
}

// --- dialTimeoutFor / respDeadlineExceededErr ---

// TestDialTimeoutFor covers dialTimeoutFor's clamp: a deadline farther away
// than respDialTimeout gets the fixed respDialTimeout, never the (larger)
// remaining time — only a near deadline gets clamped down to the smaller
// remaining value.
func TestDialTimeoutFor(t *testing.T) {
	cases := []struct {
		check func(t *testing.T, got time.Duration)
		name  string
		delta time.Duration
	}{
		{
			name:  "deadline farther away than respDialTimeout clamps to respDialTimeout",
			delta: 10 * time.Second,
			check: func(t *testing.T, got time.Duration) {
				assert.Equal(t, respDialTimeout, got)
			},
		},
		{
			name:  "deadline closer than respDialTimeout returns the remaining time",
			delta: 50 * time.Millisecond,
			check: func(t *testing.T, got time.Duration) {
				assert.Greater(t, got, time.Duration(0))
				assert.Less(t, got, respDialTimeout)
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.check(t, dialTimeoutFor(time.Now().Add(c.delta)))
		})
	}
}

// TestRespDeadlineExceededErr pins the three methods that let pipeline's
// isTimeout check treat an already-elapsed call deadline exactly like any
// other timed-out I/O error.
func TestRespDeadlineExceededErr(t *testing.T) {
	var err respDeadlineExceededErr
	assert.Equal(t, "resp: call deadline already elapsed", err.Error())
	assert.True(t, err.Timeout())
	assert.True(t, err.Temporary())
}

// --- ensureConnOn / attemptPipelineOn / handshakeOn error branches only
// reachable via a pre-set connection, not a real dial ---

// TestRESPClient_EnsureConnOn_Errors covers ensureConnOn's two error
// returns that never touch the network: a reused connection whose
// SetDeadline itself fails, and a deadline that has already elapsed before
// a fresh dial would even start. Each case builds its own *respConn slot
// directly — under the pool, ensureConnOn takes the slot as a parameter
// rather than reading shared client state, so there is no c.mu precondition
// to honor here any more.
func TestRESPClient_EnsureConnOn_Errors(t *testing.T) {
	cases := []struct {
		deadline        time.Time
		pc              func() *respConn
		name            string
		addr            string
		wantErrContains string
	}{
		{
			name: "reused connection: SetDeadline failure surfaces",
			pc: func() *respConn {
				return &respConn{conn: fakeConn{setDeadlineErr: errors.New("stub: set deadline failed")}}
			},
			deadline:        time.Now().Add(time.Second),
			wantErrContains: "set deadline",
		},
		{
			name:            "no connection yet: an already-elapsed deadline never dials",
			pc:              func() *respConn { return &respConn{} },
			addr:            "127.0.0.1:1",
			deadline:        time.Now().Add(-time.Second),
			wantErrContains: "call deadline already elapsed",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := &respClient{addr: c.addr}
			err := client.ensureConnOn(c.pc(), c.deadline)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErrContains)
		})
	}
}

// TestRESPClient_WriteError_BrokenPipe covers the identical broken-pipe
// shape shared by attemptPipelineOn's command write and handshakeOn's own
// write: both fail deterministically on an already-connected socket that
// refuses a write — distinct from a dial failure, which never reaches
// either code path.
func TestRESPClient_WriteError_BrokenPipe(t *testing.T) {
	cases := []struct {
		run             func(c *respClient, pc *respConn) error
		name            string
		wantErrContains string
	}{
		{
			name: "attemptPipelineOn: command write fails",
			run: func(c *respClient, pc *respConn) error {
				_, err := c.attemptPipelineOn(pc, [][]string{{"GET", "k"}}, time.Now().Add(time.Second))
				return err
			},
			wantErrContains: "write",
		},
		{
			name: "handshakeOn: AUTH write fails",
			run: func(c *respClient, pc *respConn) error {
				return c.handshakeOn(pc, "AUTH", "pw")
			},
			wantErrContains: "AUTH",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := &respClient{}
			pc := &respConn{
				conn: fakeConn{writeErr: errors.New("stub: broken pipe")},
				r:    bufio.NewReader(strings.NewReader("")),
			}
			err := c.run(client, pc)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErrContains)
		})
	}
}

// TestRESPClient_AuthHandshakeFailure_ErrorReplySurfaces covers
// ensureConnOn's AUTH-failure branch and handshakeOn's respErr branch
// together: a RESP error reply to AUTH must fail the call, not be
// silently treated as success. It acquires one pooled slot and calls
// attemptPipelineOn directly (one attempt, one scripted connection)
// rather than the public do/pipeline, which would retry once more against
// a second connection the fakeRESPServer fixture — built for a
// server-initiated closeConn, not a client-initiated close on handshake
// failure — cannot script cleanly.
func TestRESPClient_AuthHandshakeFailure_ErrorReplySurfaces(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"AUTH", "wrong-pw"}, reply: []byte("-ERR invalid password\r\n")},
	})

	c := newRESPClient(ln.Addr().String(), "wrong-pw", 0)
	deadline := time.Now().Add(2 * time.Second)
	pc, err := c.acquire(deadline)
	require.NoError(t, err)
	_, err = c.attemptPipelineOn(pc, [][]string{{"GET", "k"}}, deadline)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AUTH failed")
}

// --- getBatch ---

// TestRESPClient_GetBatch covers every reply shape getBatch's decode loop
// handles: a mix of hits and a missing key (RESP null bulk -> 0) on
// success, and the three ways a single bad reply fails the whole batch —
// a RESP error reply, an unexpected (non-bulk) reply type, and a bulk
// value that is not a base-10 integer.
func TestRESPClient_GetBatch(t *testing.T) {
	cases := []struct {
		name          string
		wantErrSubstr string
		keys          []string
		steps         []respStep
		want          []int64
		wantErr       bool
	}{
		{
			name: "mixed hits and a missing key",
			keys: []string{"a", "b", "c"},
			steps: []respStep{
				{wantArgs: []string{"GET", "a"}, reply: []byte("$3\r\n123\r\n")},
				{wantArgs: []string{"GET", "b"}, reply: []byte("$-1\r\n")},
				{wantArgs: []string{"GET", "c"}, reply: []byte("$3\r\n456\r\n")},
			},
			want: []int64{123, 0, 456},
		},
		{
			name: "a RESP error reply fails the whole batch",
			keys: []string{"a", "b"},
			steps: []respStep{
				{wantArgs: []string{"GET", "a"}, reply: []byte("$1\r\n1\r\n")},
				{wantArgs: []string{"GET", "b"}, reply: []byte("-ERR busy\r\n")},
			},
			wantErr:       true,
			wantErrSubstr: "ERR busy",
		},
		{
			name: "an unexpected reply type fails the whole batch",
			keys: []string{"a"},
			steps: []respStep{
				{wantArgs: []string{"GET", "a"}, reply: []byte(":5\r\n")},
			},
			wantErr:       true,
			wantErrSubstr: "unexpected reply type",
		},
		{
			name: "a non-integer value fails the whole batch",
			keys: []string{"a"},
			steps: []respStep{
				{wantArgs: []string{"GET", "a"}, reply: []byte("$3\r\nabc\r\n")},
			},
			wantErr:       true,
			wantErrSubstr: "non-integer value",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ln := newFakeListener(t)
			steps := append([]respStep{{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")}}, c.steps...)
			runFakeRESPServer(t, ln, steps)

			client := newRESPClient(ln.Addr().String(), "", 0)
			got, err := client.getBatch(c.keys)
			if c.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.wantErrSubstr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// --- connection pool (finding 1, perf audit 2026-08-2x): a single
// mutex-guarded connection was THE throughput ceiling under Yaegi
// interpretation; respClient now pools respPoolMin..respPoolMax
// connections (GOMAXPROCS-clamped by default, or an explicit
// RedisConfig.PoolSize override — llmgateway.go's buildRedisClient) ---

// TestDefaultRespPoolSize_ClampsToRange asserts defaultRespPoolSize never
// returns a value outside [respPoolMin, respPoolMax], regardless of the
// test host's actual GOMAXPROCS — the exact value is environment-
// dependent, but the clamp is not.
func TestDefaultRespPoolSize_ClampsToRange(t *testing.T) {
	got := defaultRespPoolSize()
	if got < respPoolMin || got > respPoolMax {
		t.Errorf("defaultRespPoolSize() = %d, want in [%d, %d]", got, respPoolMin, respPoolMax)
	}
}

// TestClampPoolSize covers every branch of the clamp defaultRespPoolSize
// applies to the host's real GOMAXPROCS: below respPoolMin, above
// respPoolMax, and already inside the range (passed through unchanged).
func TestClampPoolSize(t *testing.T) {
	cases := []struct {
		name string
		n    int
		want int
	}{
		{name: "below min clamps up", n: 1, want: respPoolMin},
		{name: "zero clamps up", n: 0, want: respPoolMin},
		{name: "negative clamps up", n: -3, want: respPoolMin},
		{name: "above max clamps down", n: 64, want: respPoolMax},
		{name: "exactly min passes through", n: respPoolMin, want: respPoolMin},
		{name: "exactly max passes through", n: respPoolMax, want: respPoolMax},
		{name: "inside range passes through unchanged", n: 6, want: 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampPoolSize(c.n); got != c.want {
				t.Errorf("clampPoolSize(%d) = %d, want %d", c.n, got, c.want)
			}
		})
	}
}

// TestRESPClient_Acquire_WaitsForASlotThenSucceeds exercises acquire's
// blocking-wait path (the pool is momentarily exhausted, not the
// non-blocking fast path a free slot satisfies immediately): with a
// pool of 1, a second acquire call blocks until the first caller's slot
// is released, then succeeds well within its deadline.
func TestRESPClient_Acquire_WaitsForASlotThenSucceeds(t *testing.T) {
	c := newRESPClientPool("127.0.0.1:0", "", 0, 1)
	held, err := c.acquire(time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("acquire (1st): %v", err)
	}

	released := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		c.free <- held
		close(released)
	}()

	start := time.Now()
	pc, err := c.acquire(time.Now().Add(2 * time.Second))
	elapsed := time.Since(start)
	<-released
	if err != nil {
		t.Fatalf("acquire (2nd, blocking): %v", err)
	}
	if pc != held {
		t.Errorf("acquire (2nd) returned a different slot than the one released, want the same *respConn")
	}
	if elapsed < 15*time.Millisecond {
		t.Errorf("elapsed = %v, want it to have actually waited for the release (~20ms)", elapsed)
	}
	if elapsed >= 2*time.Second {
		t.Errorf("elapsed = %v, want well under the 2s deadline", elapsed)
	}
}

// TestRESPClient_Acquire_TimesOutWhenPoolExhausted covers acquire's other
// blocking-wait outcome: no slot is ever released before the deadline, so
// acquire returns respDeadlineExceededErr rather than blocking forever.
func TestRESPClient_Acquire_TimesOutWhenPoolExhausted(t *testing.T) {
	c := newRESPClientPool("127.0.0.1:0", "", 0, 1)
	if _, err := c.acquire(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("acquire (1st, drains the only slot): %v", err)
	}

	start := time.Now()
	_, err := c.acquire(time.Now().Add(50 * time.Millisecond))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want an error: the pool's only slot is held and never released")
	}
	var wantErr respDeadlineExceededErr
	if !errors.As(err, &wantErr) {
		t.Errorf("err = %v (%T), want respDeadlineExceededErr", err, err)
	}
	if elapsed < 40*time.Millisecond || elapsed >= time.Second {
		t.Errorf("elapsed = %v, want roughly the 50ms deadline, not immediate and not unbounded", elapsed)
	}
}

// TestNewRESPClientPool_SizesPoolExactly asserts newRESPClientPool builds
// exactly poolSize free slots (each an empty, not-yet-dialled *respConn),
// and that a poolSize of 0 or below is clamped up to 1 rather than
// producing a pool no acquire could ever succeed against.
func TestNewRESPClientPool_SizesPoolExactly(t *testing.T) {
	cases := []struct {
		name         string
		poolSize     int
		wantPoolSize int
	}{
		{name: "explicit size", poolSize: 3, wantPoolSize: 3},
		{name: "zero clamps to 1", poolSize: 0, wantPoolSize: 1},
		{name: "negative clamps to 1", poolSize: -5, wantPoolSize: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := newRESPClientPool("127.0.0.1:0", "", 0, c.poolSize)
			if got := len(client.free); got != c.wantPoolSize {
				t.Errorf("len(free) = %d, want %d", got, c.wantPoolSize)
			}
			if got := cap(client.free); got != c.wantPoolSize {
				t.Errorf("cap(free) = %d, want %d", got, c.wantPoolSize)
			}
			pc := <-client.free
			if pc == nil || pc.conn != nil {
				t.Errorf("pool slot = %+v, want a non-nil *respConn with a nil conn (not yet dialled)", pc)
			}
		})
	}
}

// TestRESPClient_PoolSlotReturnedAfterError proves a slot always comes
// back onto c.free after a failed call — the physical connection is
// discarded (closeConn), never the SLOT itself — so a pool never shrinks
// after an error the way it would if a broken slot were dropped instead
// of returned.
func TestRESPClient_PoolSlotReturnedAfterError(t *testing.T) {
	ln := newFakeListener(t)
	deadAddr := ln.Addr().String()
	require.NoError(t, ln.Close()) // nothing listens at deadAddr from here on

	c := newRESPClientPool(deadAddr, "", 0, 1)
	if _, err := c.do("GET", "k"); err == nil {
		t.Fatal("want an error dialing a dead address")
	}
	if got := len(c.free); got != 1 {
		t.Fatalf("len(free) after a failed call = %d, want 1 (the slot must return to the pool)", got)
	}
}

// TestRESPClient_PoolServesConcurrentCallersWithoutSerializing is the
// core regression for finding 1: a pool-of-2 client issues two concurrent
// GETs against a listener that withholds connection 1's reply until
// connection 2 has been fully accepted and served. A single shared
// connection (the pre-fix design) would deadlock here — the second
// caller could never even dial while do() held the only connection open
// — so both calls succeeding proves they ran on two separate connections
// at once, not serialized behind one mutex.
func TestRESPClient_PoolServesConcurrentCallersWithoutSerializing(t *testing.T) {
	ln := newFakeListener(t)
	conn1Read := make(chan struct{})

	// Any read/write error below is deliberately swallowed rather than
	// failing the test directly from this goroutine (t.Fatal/t.Errorf
	// from a non-test goroutine after the test could return is unsafe): a
	// script mismatch here instead surfaces as a do() error the two
	// client goroutines below report via t.Errorf, or as the test timing
	// out waiting on conn1Read/wg — either way, a real protocol failure
	// still fails the test, just one step later.
	go func() {
		c1, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		r1 := bufio.NewReader(c1)
		_, _ = readRESPCommand(r1) // SELECT
		_, _ = c1.Write([]byte("+OK\r\n"))
		_, _ = readRESPCommand(r1) // GET
		close(conn1Read)           // conn 1's GET has been read; its reply is withheld

		c2, acceptErr2 := ln.Accept()
		if acceptErr2 != nil {
			return
		}
		r2 := bufio.NewReader(c2)
		_, _ = readRESPCommand(r2) // SELECT
		_, _ = c2.Write([]byte("+OK\r\n"))
		_, _ = readRESPCommand(r2) // GET
		_, _ = c2.Write([]byte("$1\r\n2\r\n"))

		_, _ = c1.Write([]byte("$1\r\n1\r\n")) // release conn 1's reply last
	}()

	c := newRESPClientPool(ln.Addr().String(), "", 0, 2)
	var wg sync.WaitGroup
	results := make([]string, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		v, err := c.do("GET", "a")
		if err != nil {
			t.Errorf("do(a): %v", err)
			return
		}
		results[0] = string(v.([]byte)) //nolint:forcetypeassert // test-controlled reply, always a bulk string
	}()
	go func() {
		defer wg.Done()
		<-conn1Read // wait until conn 1's GET has been read server-side
		v, err := c.do("GET", "b")
		if err != nil {
			t.Errorf("do(b): %v", err)
			return
		}
		results[1] = string(v.([]byte)) //nolint:forcetypeassert // test-controlled reply, always a bulk string
	}()
	wg.Wait()
	// assert.Equal, not ElementsMatch: results[0]/[1] are written by
	// INDEX (which goroutine, not completion order), so the expected
	// values are positionally deterministic — "a" always reads conn 1's
	// scripted reply ("1"), "b" always reads conn 2's ("2"). An
	// order-insensitive match would still pass under the exact cross-talk
	// bug this test targets (e.g. two callers corrupting/swapping each
	// other's reply via a shared connection), since {"1","2"} and
	// {"2","1"} compare equal under ElementsMatch but not under Equal.
	assert.Equal(t, []string{"1", "2"}, results)
}

// TestRESPClient_Close_DrainsAndClosesIdleConnections is the review fix
// (Should-Fix 4, 2026-08-2x): Close must actually close every idle
// pooled connection (not merely forget about it) and leave the pool
// empty.
func TestRESPClient_Close_DrainsAndClosesIdleConnections(t *testing.T) {
	closed := 0
	c := newRESPClientPool("127.0.0.1:0", "", 0, 3)
	// Replace each of the 3 idle slots' connection with one that records
	// whether Close was called on it.
	for i := 0; i < 3; i++ {
		pc := <-c.free
		pc.conn = &closeCountingConn{count: &closed}
		c.free <- pc
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(c.free); got != 0 {
		t.Errorf("len(free) after Close = %d, want 0 (every slot drained)", got)
	}
	if closed != 3 {
		t.Errorf("closed connections = %d, want 3 (every idle slot's connection actually closed)", closed)
	}
}

// TestRESPClient_Close_EmptyPool_ReturnsNilImmediately asserts Close on a
// pool with nothing yet dialled (every slot's conn is nil) is a no-op
// that returns promptly, not a hang — closeConn already tolerates a nil
// conn.
func TestRESPClient_Close_EmptyPool_ReturnsNilImmediately(t *testing.T) {
	c := newRESPClientPool("127.0.0.1:0", "", 0, 2)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(c.free); got != 0 {
		t.Errorf("len(free) after Close = %d, want 0", got)
	}
}
