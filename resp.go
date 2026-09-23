package traefikllmgateway

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
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
// handshake and command step it happens to perform.
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

// respMaxArrayLen caps a RESP2 array's declared element count. None of
// this client's commands (AUTH, SELECT, INCRBY, EXPIRE, GET) ever return
// an array reply, so 1024 is already generous headroom, not a limit tuned
// to real traffic.
const respMaxArrayLen = 1024

// respMaxLineLen caps a single RESP2 header line (the "+", "-", ":", "$",
// or "*" line preceding any body) read by readLine. Without a cap, a peer
// that never sends '\n' makes readLine buffer an unbounded number of
// bytes before erroring.
const respMaxLineLen = 64 << 10

// respPoolMin/respPoolMax bound the self-tuned connection pool size
// (house engineering rule: self-tuning over operator knobs, explicit
// override always wins — see RedisConfig.PoolSize, llmgateway.go's
// buildRedisClient). A single mutex-guarded connection was measured to be
// THE throughput ceiling under Yaegi interpretation (perf audit,
// 2026-08-2x): 1,458 req/s Redis-backed vs 7,811 req/s on memoryStore for
// the identical workload, latency scaling ~linearly with concurrency
// (869us@conc=1 -> 10.81ms@conc=16) — near-perfect serialization, since
// pipeline held c.mu for the whole round trip. A pool of independent
// connections, each handed to exactly one caller at a time, lets N
// callers run N round trips concurrently instead of queueing behind one.
// respPoolMax=8 is the exact size the audit's variants/resp_pool8.go
// prototype validated: at a realistic 200us Redis RTT, conc=16, p50
// 24.06ms -> 3.24ms (-86%), throughput 663 -> 4,670 req/s (+599%, ±1%
// over 3 reps); a pool=1 control matched HEAD exactly, proving the win is
// parallelism, not refactor noise. respPoolMin=4 keeps a low-core-count
// replica (GOMAXPROCS clamps low under a small CPU request/limit) from
// opening fewer connections than there are usable goroutine-scheduling
// slots to fill them.
const (
	respPoolMin = 4
	respPoolMax = 8
)

// respPoolConfigMax bounds RedisConfig.PoolSize (llmgateway.go's
// buildRedisClient rejects anything above it outright, mirroring the
// existing negative-DB/negative-poolSize checks): an unbounded override
// would let PoolSize: 1<<20 eagerly allocate over a million *respConn
// structs at construction (verified in review, 2026-08-2x) before a
// single request ever arrives. 256 is far beyond any real single-instance
// Redis/Dragonfly connection budget while staying well above
// respPoolMax, leaving room for a legitimately high explicit override.
const respPoolConfigMax = 256

// defaultRespPoolSize returns the self-tuned connection pool size: the
// host's GOMAXPROCS, clamped to [respPoolMin, respPoolMax]. Used whenever
// RedisConfig.PoolSize is left at its zero value; an operator who sets it
// explicitly always overrides this (newRESPClientPool, below).
func defaultRespPoolSize() int {
	return clampPoolSize(runtime.GOMAXPROCS(0))
}

// clampPoolSize clamps n to [respPoolMin, respPoolMax]. Split out of
// defaultRespPoolSize so the clamp itself is testable against every
// branch directly, independent of the test process's own real
// GOMAXPROCS.
func clampPoolSize(n int) int {
	switch {
	case n < respPoolMin:
		return respPoolMin
	case n > respPoolMax:
		return respPoolMax
	default:
		return n
	}
}

// respErr is a RESP2 error reply ("-message\r\n"). It implements error so a
// caller inspecting a decoded reply can type-assert for it the same way as
// any other error.
//
// A struct wrapping the message, not a defined string type ("type respErr
// string"): yaegi v0.16.1 conflates a defined string type with plain
// string in a type assertion — v.(respErr) on an any holding a genuine
// plain string (e.g. decodeReply's "+OK" success reply, itself a string)
// incorrectly reports ok=true, so every successful handshakeOn call
// (AUTH/SELECT) was misread as a failed one under real Traefik. A struct
// has no such ambiguity: verified empirically under real Traefik (Task
// 15's integration suite, cross-replica rate limiting via Redis).
type respErr struct{ msg string }

// Error implements the error interface.
func (e respErr) Error() string { return e.msg }

// respConn is one connection in respClient's pool: a lazily dialled TCP
// connection plus its buffered reader, or the zero value for a slot that
// has never dialled yet (or whose previous connection was discarded after
// an error — see closeConn). Owned by exactly one caller at a time,
// between an acquire and its matching release back onto respClient.free.
type respConn struct {
	conn net.Conn
	r    *bufio.Reader
}

// respClient is a minimal stdlib-only RESP2 client for a single Redis-
// compatible server: a pool of independently usable TCP connections, each
// lazily dialled on first use and reconnected at most once when a command
// round trip fails for a reason other than a timeout (see pipeline). It
// exists because the plugin runs interpreted under Yaegi with only the Go
// standard library available, so a full Redis client library is not an
// option.
//
// free holds every currently-idle connection slot; a caller acquires one
// (blocking, bounded by its call deadline, if the pool is momentarily
// exhausted) for its whole round trip and returns it via a deferred send
// back onto free — RESP is not multiplexed, so two callers interleaving
// writes on the SAME connection would corrupt both replies, but distinct
// connections have no such restriction and can run fully concurrently.
type respClient struct {
	free     chan *respConn
	addr     string
	password string
	db       int
	// tls wraps every dialled connection in TLS when true, verifying the
	// server certificate against addr's host.
	//
	// Security audit run-1: this client could previously ONLY speak
	// plaintext TCP — there was no TLS field, no scheme parsing and no
	// rediss:// handling anywhere — so the AUTH password, the counter
	// keys (which embed principal names verbatim, windowKey in limits.go)
	// and whole cached response bodies were unprotectable in transit by
	// ANY operator configuration. That is what distinguishes it from an
	// operator who merely chose plaintext: the choice was foreclosed.
	//
	// Off by default, so an existing deployment behaves exactly as
	// before; set redis.tls to opt in. Not a constructor parameter
	// because newRESPClient/newRESPClientPool have ~50 call sites across
	// the test suite and none of them needs it — buildRedisClient
	// (llmgateway.go) sets it directly on the constructed client.
	tls bool
}

// newRESPClient returns a respClient for addr, sized by defaultRespPoolSize
// — the self-tuned pool size used everywhere an explicit override
// (RedisConfig.PoolSize) is absent. It does not connect until the first
// do or pipeline call. password, if non-empty, is sent via AUTH on every
// new connection; db is always sent via SELECT on every new connection
// (including db 0), since a Redis-compatible proxy is not guaranteed to
// default a fresh connection to database 0.
func newRESPClient(addr, password string, db int) *respClient {
	return newRESPClientPool(addr, password, db, defaultRespPoolSize())
}

// newRESPClientPool is newRESPClient with an explicit poolSize, used by
// llmgateway.go's buildRedisClient when RedisConfig.PoolSize is set
// (house rule: an explicit override always wins over the self-tuned
// default). poolSize below 1 is clamped to 1 — a zero or negative value
// would leave free empty, making every acquire block forever.
func newRESPClientPool(addr, password string, db, poolSize int) *respClient {
	if poolSize < 1 {
		poolSize = 1
	}
	c := &respClient{
		addr:     addr,
		password: password,
		db:       db,
		free:     make(chan *respConn, poolSize),
	}
	for i := 0; i < poolSize; i++ {
		c.free <- &respConn{}
	}
	return c
}

// Close closes every currently idle pooled connection (a non-blocking
// receive per slot, so it never waits on one still checked out by an
// in-flight caller — the same "don't call Close concurrently with
// in-flight use" contract any io.Closer implicitly carries) and always
// returns nil — closeConn already discards each connection's own Close
// error, matching every other close in this client.
//
// Review fix (Should-Fix 4, 2026-08-2x): the pool (perf finding 1) turned
// a discarded-instance leak of one idle connection into up to
// respPoolMax (8). Nothing in this codebase calls Close automatically —
// Traefik's plugin contract for a locally-loaded middleware is exactly
// `func New(...) (http.Handler, error)` (llmgateway.go); the returned
// value satisfies only http.Handler, with no Shutdown/Close interface
// Traefik itself checks for or invokes on a discarded instance (see
// Gateway.Close's own doc comment for the full picture, including why
// this repository has no evidence such a hook could even be wired to).
// Close exists so a respClient built and discarded outside that contract
// — tests, or a future framework/harness that does hold a lifecycle hook
// — has a correct, deterministic way to release pooled connections
// rather than depend solely on GC finalizing the underlying net.Conns.
func (c *respClient) Close() error {
	for {
		select {
		case pc := <-c.free:
			closeConn(pc)
		default:
			return nil
		}
	}
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
// decoded replies in the same order. It encodes cmds once and hands off to
// pipelineEncoded, which owns the connection/retry logic.
func (c *respClient) pipeline(cmds [][]string) ([]any, error) {
	return c.pipelineEncoded(encodeCommands(cmds), len(cmds), pipelineMutates(cmds))
}

// pipelineEncoded runs pipeline's connection/retry logic over an
// already-encoded RESP2 payload representing n commands, over one pooled
// connection acquired for the duration of the call, and returns their
// decoded replies in the same order.
//
// One absolute deadline is computed here, at call entry, and shared by
// acquiring a connection, the first attempt, and (if it happens) the one
// retry — not a fresh respCallTimeout for each. On a non-timeout I/O error
// it closes the connection and retries the whole pipeline once against the
// same deadline, on the SAME pooled slot (a fresh dial reusing that slot,
// not a second slot acquired from the pool) — but ONLY when doing so is
// known safe (finding 5, 2026-09 review, fixing a double-apply bug; the
// wroteRequest-alone gate was itself corrected by the verify-core round-4
// fix below):
//
//   - The failure happened before any byte of the request was written
//     (attemptEncodedOn's wroteRequest is false — a dial failure, an
//     ensureConnOn handshake failure, or the command write itself
//     erroring). This is one stale-pooled-connection case: a connection
//     idle in the pool that the peer silently closed sometimes fails on
//     this very first write, before the server has seen anything from
//     this attempt at all, so resending the identical pipeline against a
//     freshly dialled connection cannot duplicate an effect that never
//     happened.
//   - Or the request was written and a reply was awaited, but no reply
//     byte at all was ever read (attemptEncodedOn's readAny is false).
//     A write into an already half-closed TCP connection commonly still
//     succeeds into the kernel send buffer — the peer's close only
//     surfaces on the READ, as an immediate EOF with zero bytes
//     received. This is the OTHER, more common stale-pooled-connection
//     case, and is exactly as safe to retry as the write-side one above:
//     nothing was ever seen or applied server-side.
//   - Or the request WAS written, some reply reading happened, but cmds
//     contains no mutating command (pipelineMutates) — e.g. a pure
//     getBatch/setEx/getBytes call. GET/SET/EXPIRE/AUTH/SELECT are
//     idempotent: replaying one after a reply was lost (the connection
//     reset mid-read, after the server already processed and answered)
//     reproduces the same end state, so retrying is still safe even
//     though the request definitely reached the wire.
//
// Once at least one reply byte has actually been read AND cmds contains a
// mutating command (INCRBY/INCRBYFLOAT — redisStore's incrMulti/
// incrAndGetMulti), a non-timeout failure is surfaced directly, with NO
// retry: the server has demonstrably started answering, so it may already
// have applied that INCRBY before the connection reset (a proxy failover,
// or the backend restarting mid-reply) — resending the whole pipeline in
// that case would apply it a second time, double-counting a rate/token/
// cost counter into a false 429 or doubled recorded spend. The caller's
// own failOpen/failClosed policy (limits.go) decides what happens to THIS
// attempt's failure; it must never see a duplicated increment instead.
//
// On a timeout specifically, it never retries either way: a server that
// accepted the connection and then went silent would just be given the
// same non-answer a second time, doubling the caller's wait for nothing.
// Any failure leaves that connection closed (closeConn) so the next call
// to acquire this slot lazily reconnects fresh rather than reusing a
// connection already known bad — the slot itself always returns to the
// pool via the deferred send below, whether or not its connection
// survived the call.
//
// payload is encoded once by the caller, so the retry re-sends the
// identical bytes without re-encoding cmds a second time. mutates is the
// caller's pipelineMutates verdict for the commands payload encodes —
// pipelineEncoded never sees the []string form, so the caller supplies it.
func (c *respClient) pipelineEncoded(payload []byte, n int, mutates bool) ([]any, error) {
	deadline := time.Now().Add(respCallTimeout)

	pc, err := c.acquire(deadline)
	if err != nil {
		return nil, err
	}
	defer func() { c.free <- pc }()

	replies, wrote, readAny, err := c.attemptEncodedOn(pc, payload, n, deadline)
	if err == nil {
		return replies, nil
	}
	closeConn(pc)
	if isTimeout(err) {
		return nil, err
	}
	if wrote && readAny && mutates {
		return nil, err
	}

	replies, _, _, err = c.attemptEncodedOn(pc, payload, n, deadline)
	if err != nil {
		closeConn(pc)
		return nil, err
	}
	return replies, nil
}

// pipelineMutatingCommands names every RESP command this client ever sends
// that is NOT safe to blindly resend: each already changes server state on
// success, so replaying it after an ambiguous failure (the request bytes
// were written, but the reply for it — or a later command in the same
// pipeline — never came back) can double-apply it, unlike a GET/SET/
// EXPIRE/AUTH/SELECT retry, which merely reproduces the same end state.
// INCRBYFLOAT is not currently sent by this client (only INCRBY is —
// redis_store.go), but is listed for the same reason INCRBY is: an
// operator or a future caller adding it must not have to rediscover this
// gate.
var pipelineMutatingCommands = map[string]bool{
	"INCRBY":      true,
	"INCRBYFLOAT": true,
}

// pipelineMutates reports whether any command in cmds is one of
// pipelineMutatingCommands — pipeline's own gate (finding 5, 2026-09
// review) for whether a failure discovered AFTER the request was already
// written to the wire is still safe to retry.
func pipelineMutates(cmds [][]string) bool {
	for _, args := range cmds {
		if len(args) == 0 {
			continue
		}
		if pipelineMutatingCommands[args[0]] {
			return true
		}
	}
	return false
}

// acquire takes one idle connection slot from c.free, waiting no longer
// than deadline. The non-blocking check first is an optimization, not a
// correctness requirement of the blocking select below — with a buffered
// channel a receive that CAN succeed immediately always does, so this
// merely skips allocating a timer on the (overwhelmingly common) case
// where a slot is already free.
func (c *respClient) acquire(deadline time.Time) (*respConn, error) {
	select {
	case pc := <-c.free:
		return pc, nil
	default:
	}

	wait := time.Until(deadline)
	if wait <= 0 {
		return nil, respDeadlineExceededErr{}
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case pc := <-c.free:
		return pc, nil
	case <-t.C:
		return nil, respDeadlineExceededErr{}
	}
}

// isTimeout reports whether err is (or wraps) a net.Error that timed out.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// dialTimeoutFor returns the duration a dial starting now should use to
// avoid running past deadline: the smaller of respDialTimeout and the
// time remaining until deadline. Without this clamp, a dial always
// spending its own full respDialTimeout regardless of deadline would let
// a future change to respDialTimeout alone (leaving respCallTimeout
// untouched) silently widen how much of one call's budget a stalled dial
// can consume.
func dialTimeoutFor(deadline time.Time) time.Duration {
	if remaining := time.Until(deadline); remaining < respDialTimeout {
		return remaining
	}
	return respDialTimeout
}

// respDeadlineExceededErr signals that a call's deadline had already
// elapsed before an attempt could even acquire a pool slot or dial — e.g.
// this call queued behind every one of respClient's pool slots being busy
// for the whole budget while earlier calls were stuck. It implements
// net.Error so pipeline's isTimeout check treats it the same as any other
// timed-out I/O: no retry, since retrying an already-elapsed deadline can
// only fail the same way again.
type respDeadlineExceededErr struct{}

func (respDeadlineExceededErr) Error() string   { return "resp: call deadline already elapsed" }
func (respDeadlineExceededErr) Timeout() bool   { return true }
func (respDeadlineExceededErr) Temporary() bool { return true }

// attemptEncodedOn runs one full attempt of an already-encoded RESP2
// payload representing n commands over pc, connecting first if needed,
// with every read and write bound by deadline. Callers own pc exclusively
// for the duration of this call (acquired from c.free).
//
// wroteRequest is true as soon as every command has been fully written to
// pc.conn, regardless of what happens afterward (a reply read failing does
// not unset it) — pipeline's own retry gate (finding 5, 2026-09 review)
// uses this to tell "failed before the server could have seen anything"
// from "failed after the server may already have applied a mutating
// command", since only the first case is unconditionally safe to retry.
//
// readAny (verify-core fix, round 4) is true once at least one byte of
// the FIRST reply was actually consumed off pc.conn. Finding 5's original
// gate assumed a stale pooled connection the peer already closed always
// fails on the WRITE — wrong: a write into an already-half-closed TCP
// connection commonly still succeeds into the kernel send buffer, and
// the failure only surfaces on the read as an immediate EOF with zero
// bytes ever received. That is the stale-connection signature and it
// must still retry, even for a mutating pipeline, exactly like a
// pre-write failure does — nothing was ever seen or applied server-side.
// Once even one reply byte has arrived, the server has demonstrably
// started answering, so pipeline's caller no longer knows whether a
// mutating command already landed and must not resend it.
func (c *respClient) attemptEncodedOn(pc *respConn, payload []byte, n int, deadline time.Time) (replies []any, wroteRequest bool, readAny bool, err error) {
	if err := c.ensureConnOn(pc, deadline); err != nil {
		return nil, false, false, err
	}

	if _, err := pc.conn.Write(payload); err != nil {
		return nil, false, false, fmt.Errorf("resp: write: %w", err)
	}

	replies = make([]any, n)
	for i := 0; i < n; i++ {
		if !readAny {
			// Peek the very first byte of the very first reply before
			// handing off to decodeReply: this is the one read whose
			// outcome pipeline's retry gate needs isolated from every
			// later one. ReadByte+UnreadByte consumes nothing overall —
			// the byte goes straight back for decodeReply below — it
			// only distinguishes "the connection had nothing left to
			// give at all" (stale pooled conn, safe to retry) from
			// "some reply data has started arriving" (readAny=true for
			// the rest of this call, whatever happens next).
			if _, peekErr := pc.r.ReadByte(); peekErr != nil {
				return nil, true, false, fmt.Errorf("resp: read reply %d/%d: %w", i+1, n, peekErr)
			}
			if unreadErr := pc.r.UnreadByte(); unreadErr != nil {
				// bufio.Reader.UnreadByte only fails when the preceding
				// op was not itself a successful ReadByte, which cannot
				// happen here — kept as a hard error rather than
				// silently mis-tracking readAny, which the mutating-
				// pipeline retry decision depends on for correctness.
				return nil, true, true, fmt.Errorf("resp: read reply %d/%d: unread byte: %w", i+1, n, unreadErr)
			}
			readAny = true
		}
		v, err := decodeReply(pc.r, 0)
		if err != nil {
			return nil, true, readAny, fmt.Errorf("resp: read reply %d/%d: %w", i+1, n, err)
		}
		replies[i] = v
	}
	return replies, true, true, nil
}

// ensureConnOn dials, authenticates, and selects the database if pc has no
// live connection, then (whether freshly dialled or reused) sets deadline
// as the connection's single read/write deadline for the remainder of
// this call — including the handshake, when one runs. Callers own pc
// exclusively for the duration of this call.
func (c *respClient) ensureConnOn(pc *respConn, deadline time.Time) error {
	if pc.conn != nil {
		if err := pc.conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("resp: set deadline: %w", err)
		}
		return nil
	}

	dialTimeout := dialTimeoutFor(deadline)
	if dialTimeout <= 0 {
		return fmt.Errorf("resp: dial %q: %w", c.addr, respDeadlineExceededErr{})
	}
	conn, err := net.DialTimeout("tcp", c.addr, dialTimeout) // #nosec G704 -- c.addr is the operator-configured redis address from middleware config, never request-derived
	if err != nil {
		return fmt.Errorf("resp: dial %q: %w", c.addr, err)
	}

	// Optional TLS (security audit run-1, respClient.tls). Wrapping here
	// rather than at a separate dial keeps one dial path: everything
	// below — SetDeadline, the AUTH/SELECT handshake, and the caller's
	// own command round trip — then runs over the TLS stream instead of
	// the bare socket, with no other call site needing to know.
	//
	// ServerName is the host half of the configured address, so the
	// certificate is verified against the name the operator configured.
	// InsecureSkipVerify is deliberately left false: an opt-in TLS mode
	// that silently skipped verification would provide confidentiality
	// against a passive observer while giving none against the active
	// one, which is the attacker this exists for.
	if c.tls {
		host, _, splitErr := net.SplitHostPort(c.addr)
		if splitErr != nil {
			host = c.addr
		}
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		})
		if dlErr := tlsConn.SetDeadline(deadline); dlErr != nil {
			_ = tlsConn.Close()
			return fmt.Errorf("resp: set deadline: %w", dlErr)
		}
		if hsErr := tlsConn.Handshake(); hsErr != nil {
			_ = tlsConn.Close()
			return fmt.Errorf("resp: tls handshake %q: %w", c.addr, hsErr)
		}
		conn = tlsConn
	}

	pc.conn = conn
	pc.r = bufio.NewReader(conn)

	if err := pc.conn.SetDeadline(deadline); err != nil {
		closeConn(pc)
		return fmt.Errorf("resp: set deadline: %w", err)
	}

	if c.password != "" {
		if err := c.handshakeOn(pc, "AUTH", c.password); err != nil {
			closeConn(pc)
			return err
		}
	}
	if err := c.handshakeOn(pc, "SELECT", strconv.Itoa(c.db)); err != nil {
		closeConn(pc)
		return err
	}
	return nil
}

// handshakeOn sends one connection-setup command (AUTH or SELECT) and
// requires a non-error reply. Callers own pc exclusively, with a live
// pc.conn whose deadline is already set.
func (c *respClient) handshakeOn(pc *respConn, cmd, arg string) error {
	if _, err := pc.conn.Write(encodeCommands([][]string{{cmd, arg}})); err != nil {
		return fmt.Errorf("resp: %s: write: %w", cmd, err)
	}
	v, err := decodeReply(pc.r, 0)
	if err != nil {
		return fmt.Errorf("resp: %s: read reply: %w", cmd, err)
	}
	if e, ok := v.(respErr); ok {
		return fmt.Errorf("resp: %s failed: %w", cmd, e)
	}
	return nil
}

// closeConn closes and clears pc's connection, if any, discarding it —
// the connection itself is never reused after an error, only the slot pc
// occupies, which pipeline always returns to c.free regardless.
func closeConn(pc *respConn) {
	if pc.conn == nil {
		return
	}
	_ = pc.conn.Close()
	pc.conn = nil
	pc.r = nil
}

// encodeCommands renders every command in cmds as consecutive RESP2 arrays,
// one fmt.Appendf call per header/argument appending into a single growing
// slice — no strings.Builder and no string/[]byte conversion. Measured
// under yaegi v0.16.1: cheaper than either presizing (the interpreted size
// loop cost more than native slice regrowth) or appending raw bytes
// directly (append with a string operand copies it under yaegi).
//
// A 3-argument command — INCRBY key delta or EXPIRE key seconds, this
// client's two hottest commands by far (every counter increment sends
// one, sometimes both) — takes a fast path instead: one fmt.Appendf call
// building the whole "*3\r\n$len\r\narg\r\n..." frame at once, rather than
// one header call plus 3 more per-argument calls. Measured under yaegi
// v0.16.1 on the counter-increment pipeline this dominates: about 3%
// fewer bytes per request. Any other argument count keeps the general
// loop below — wire bytes are identical either way.
func encodeCommands(cmds [][]string) []byte {
	var dst []byte
	for _, args := range cmds {
		if len(args) == 3 {
			dst = fmt.Appendf(dst, "*3\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n",
				len(args[0]), args[0], len(args[1]), args[1], len(args[2]), args[2])
			continue
		}
		dst = fmt.Appendf(dst, "*%d\r\n", len(args))
		for _, a := range args {
			dst = fmt.Appendf(dst, "$%d\r\n%s\r\n", len(a), a)
		}
	}
	return dst
}

// encodeCommand renders args as a RESP2 command array:
// "*N\r\n$len\r\narg\r\n..." for each arg.
func encodeCommand(args []string) string {
	return string(encodeCommands([][]string{args}))
}

// decodeReply reads and decodes one RESP2 reply from r. depth is 0 for a
// top-level reply and depth+1 for each element decodeArray recurses into;
// an array is only accepted at depth 0 (see the '*' case) — none of this
// client's commands (AUTH, SELECT, INCRBY, EXPIRE, GET) ever return a
// nested array, so a server or attacker sending one is malformed input,
// not legitimate traffic worth the recursion:
//
//   - "+simple\r\n"  -> string
//   - "-error\r\n"   -> respErr (implements error; not returned as err)
//   - ":123\r\n"     -> int64
//   - "$len\r\n...\r\n" -> []byte, or nil for a null bulk ("$-1\r\n")
//   - "*n\r\n..."    -> []any of n decoded elements, or nil for a null
//     array ("*-1\r\n"); only at depth 0
//
// The returned error is non-nil only for a transport or protocol failure
// (short read, malformed line, unknown type byte, an out-of-range or
// mistrailed bulk/array, a nested array) — a well-formed RESP error reply
// decodes successfully to a respErr value, not to a non-nil error, so a
// caller pipelining several commands can still read every reply after one
// of them fails server-side.
func decodeReply(r *bufio.Reader, depth int) (any, error) {
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
		return respErr{msg: rest}, nil
	case ':':
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("resp: malformed integer reply %q: %w", rest, err)
		}
		return n, nil
	case '$':
		return decodeBulk(r, rest)
	case '*':
		if depth > 0 {
			// A nested array reply — "*1\r\n*1\r\n..." repeated — could
			// otherwise recurse to Go's stack limit (a fatal, unrecoverable
			// error, not a panic recoverPanic could catch) with only a few
			// megabytes of crafted wire input. No command this client sends
			// ever gets one back, so reject it outright instead of
			// recursing.
			return nil, fmt.Errorf("resp: nested array reply not supported")
		}
		return decodeArray(r, rest, depth)
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
// allocating, for the same reason as decodeBulk's cap. depth is the
// depth decodeReply was called at for this array's own "*" line (always
// 0 — see decodeReply's '*' case); each element decodes at depth+1, so an
// element that is itself an array is rejected by decodeReply rather than
// recursed into.
func decodeArray(r *bufio.Reader, lenField string, depth int) (any, error) {
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
		v, err := decodeReply(r, depth+1)
		if err != nil {
			return nil, fmt.Errorf("resp: array element %d/%d: %w", i+1, n, err)
		}
		arr[i] = v
	}
	return arr, nil
}

// setEx sets key to val with an expiry of ttl via RESP2 "SET key val EX
// seconds", used by the response cache (cache.go) to store a cached
// response. ttl is rounded up to whole seconds and floored at 1s — Redis's
// EX argument is whole seconds only — matching redisStore.incrMulti's EXPIRE
// rounding exactly. The payload is built directly with fmt.Appendf rather
// than going through do/encodeCommands: val (a cached response, often tens
// of KB) is framed straight from its own bytes — "%s" on a []byte writes
// it verbatim — avoiding both the string(val) copy the old []string-args
// call required and a second encoding pass.
func (c *respClient) setEx(key string, val []byte, ttl time.Duration) error {
	ttlSeconds := int64(ttl / time.Second)
	if ttl%time.Second != 0 {
		ttlSeconds++
	}
	if ttlSeconds < 1 {
		ttlSeconds = 1
	}

	ttlArg := strconv.FormatInt(ttlSeconds, 10)
	payload := fmt.Appendf(nil, "*5\r\n$3\r\nSET\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n$2\r\nEX\r\n$%d\r\n%s\r\n", len(key), key, len(val), val, len(ttlArg), ttlArg)
	// SET is idempotent (not in pipelineMutatingCommands), so a retry after
	// a lost reply reproduces the same end state.
	replies, err := c.pipelineEncoded(payload, 1, false)
	if err != nil {
		return fmt.Errorf("resp: setEx %q: %w", key, err)
	}
	reply := replies[0]
	if e, ok := reply.(respErr); ok {
		return fmt.Errorf("resp: setEx %q: %w", key, e)
	}
	return nil
}

// getBytes reads key's value as a bulk reply. found is false for a missing
// key (RESP null bulk, "$-1") — not an error — mirroring redisStore.getMulti's
// treatment of a missing counter key.
func (c *respClient) getBytes(key string) ([]byte, bool, error) {
	reply, err := c.do("GET", key)
	if err != nil {
		return nil, false, fmt.Errorf("resp: getBytes %q: %w", key, err)
	}
	if reply == nil {
		return nil, false, nil
	}
	if e, ok := reply.(respErr); ok {
		return nil, false, fmt.Errorf("resp: getBytes %q: %w", key, e)
	}
	b, ok := reply.([]byte)
	if !ok {
		return nil, false, fmt.Errorf("resp: getBytes %q: unexpected reply type %T", key, reply)
	}
	return b, true, nil
}

// getBatch reads every key in keys in one pipelined round trip — N GET
// commands sent together, replies read together — instead of len(keys)
// separate calls each paying their own respCallTimeout budget (the fix
// for GET /admin/api/usage otherwise serializing 6 GETs per user/group
// scope on the single shared connection). It returns one value per key,
// in the same order, with a missing key (RESP null bulk) mapped to 0,
// matching getBytes/redisStore.getMulti's convention for a single key. Any
// reply that is a RESP error, an unexpected reply type, or a non-integer
// value fails the whole batch — mirrored by the limiter's
// storeGetMulti/currentUsage as one fail-open/fail-closed decision for
// the scope, never a partial result mixing real and zero values.
func (c *respClient) getBatch(keys []string) ([]int64, error) {
	cmds := make([][]string, len(keys))
	for i, k := range keys {
		cmds[i] = []string{"GET", k}
	}
	replies, err := c.pipeline(cmds)
	if err != nil {
		return nil, fmt.Errorf("resp: getBatch: %w", err)
	}

	out := make([]int64, len(keys))
	for i, reply := range replies {
		if reply == nil {
			continue // missing key -> 0, matching getBytes/get's convention
		}
		if e, ok := reply.(respErr); ok {
			return nil, fmt.Errorf("resp: getBatch %q: %w", keys[i], e)
		}
		b, ok := reply.([]byte)
		if !ok {
			return nil, fmt.Errorf("resp: getBatch %q: unexpected reply type %T", keys[i], reply)
		}
		v, err := strconv.ParseInt(string(b), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("resp: getBatch %q: non-integer value %q: %w", keys[i], b, err)
		}
		out[i] = v
	}
	return out, nil
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
