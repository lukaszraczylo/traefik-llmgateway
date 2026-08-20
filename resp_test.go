package traefikllmgateway

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
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
// array (including the null array "*-1" and nesting).
func TestDecodeReply(t *testing.T) {
	t.Run("simple string", func(t *testing.T) {
		v, err := decodeReply(newReader("+OK\r\n"))
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		if v != "OK" {
			t.Errorf("v = %#v, want %q", v, "OK")
		}
	})

	t.Run("error reply decodes to respErr, not a decode error", func(t *testing.T) {
		v, err := decodeReply(newReader("-ERR wrong number of arguments\r\n"))
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
		v, err := decodeReply(newReader(":42\r\n"))
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		if v != int64(42) {
			t.Errorf("v = %#v, want int64(42)", v)
		}
	})

	t.Run("negative integer", func(t *testing.T) {
		v, err := decodeReply(newReader(":-7\r\n"))
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		if v != int64(-7) {
			t.Errorf("v = %#v, want int64(-7)", v)
		}
	})

	t.Run("bulk string", func(t *testing.T) {
		v, err := decodeReply(newReader("$5\r\nhello\r\n"))
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		b, ok := v.([]byte)
		if !ok || !bytes.Equal(b, []byte("hello")) {
			t.Errorf("v = %#v, want []byte(\"hello\")", v)
		}
	})

	t.Run("null bulk (missing key)", func(t *testing.T) {
		v, err := decodeReply(newReader("$-1\r\n"))
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		if v != nil {
			t.Errorf("v = %#v, want nil", v)
		}
	})

	t.Run("array", func(t *testing.T) {
		v, err := decodeReply(newReader("*2\r\n:5\r\n:1\r\n"))
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		arr, ok := v.([]any)
		if !ok || len(arr) != 2 || arr[0] != int64(5) || arr[1] != int64(1) {
			t.Errorf("v = %#v, want []any{int64(5), int64(1)}", v)
		}
	})

	t.Run("null array", func(t *testing.T) {
		v, err := decodeReply(newReader("*-1\r\n"))
		if err != nil {
			t.Fatalf("decodeReply: %v", err)
		}
		if v != nil {
			t.Errorf("v = %#v, want nil", v)
		}
	})

	t.Run("malformed type byte is a decode error", func(t *testing.T) {
		if _, err := decodeReply(newReader("?nope\r\n")); err == nil {
			t.Fatal("want error for an unknown reply type byte")
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
