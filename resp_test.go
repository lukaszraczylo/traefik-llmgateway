package traefikllmgateway

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
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
// resp.go (attemptPipelineLocked, handshakeLocked, and decodeArray's own
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
// the connection is closed — by the client's own closeLocked after its
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
