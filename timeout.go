package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// defaultRequestTimeout is the progress-based upstream request timeout
// applied when neither Config.RequestTimeout nor a provider's own
// ProviderConfig.RequestTimeout is set: five minutes.
//
// Before this feature, newAdapterHTTPClient (providers.go) set no timeout
// at all — Client.Timeout was left unset, and the Transport cloned from
// http.DefaultTransport set no ResponseHeaderTimeout either. Only the dial
// (30s) and TLS handshake (10s) were bounded. A provider that accepted the
// connection and then hung — sent no headers, or sent headers and then
// stalled mid-body — hung the gateway request forever; nothing recovered
// it. This constant is a deliberate behavior change for every deployment,
// not an invisible bug fix — see README.md's "Request timeout" section.
const defaultRequestTimeout = 5 * time.Minute

// errProviderTimeout is the sentinel wrapped into the error a watchdogBody
// (below) returns once its idle-progress watchdog fires: a period of the
// resolved timeout passed with no successful read from the upstream
// response body. Distinct from context.Canceled — a genuine client
// disconnect, classified separately by handleAdapterErrorEnvelope
// (routes_unified.go) and proxyUpstream (routes_passthrough.go), both of
// which check errors.Is(err, context.Canceled) ahead of their generic
// upstream-error fallback — watchdogBody.Read never lets a timeout error
// wrap context.Canceled, so the two stay mutually exclusive in every
// caller's classification.
//
// The message deliberately says "upstream progress stalled", not
// "provider timed out" (coordinator adversarial review, 2026-08-23,
// finding F3): the watchdog measures time since this gateway's last
// successful READ from the upstream body, which can go quiet either
// because the provider itself stalled, or because this gateway spent the
// whole window blocked WRITING to a slow/stalled client between reads (a
// streaming forwarder reads one chunk, writes it out, then reads again —
// the write is not itself watchdog-guarded). Aborting in either case is
// still correct (holding an upstream connection open for a stalled
// client is its own problem), but asserting the PROVIDER specifically
// caused it is not always true, so the message no longer claims it.
var errProviderTimeout = errors.New("llmgateway: upstream progress stalled")

// resolveRequestTimeout parses raw (a Go duration string, e.g. "5m" or
// "90s" — matching RetryConfig.Backoff/CacheConfig.TTL's own convention)
// and returns defaultRequestTimeout when raw is empty: Config.
// RequestTimeout and ProviderConfig.RequestTimeout's own zero value.
//
// A non-empty raw that fails to parse, or parses to a duration <= 0, is a
// construction error. This is stricter than CacheConfig.TTL (validated by
// validateCacheConfig, cache.go), which only rejects a malformed string,
// never a non-positive one: a request timeout of zero or less is never a
// meaningful value for THIS feature specifically, because it exists only
// to fix a hang bug (newAdapterHTTPClient's pre-feature shape, described
// on defaultRequestTimeout above) — an operator writing "0s" hoping to
// restore "wait forever" must fail loudly at construction, not silently
// reintroduce the exact hang this feature exists to close. label names
// the config field in the resulting error message (e.g. "requestTimeout"
// or `provider "openai": requestTimeout`) so a bad value is attributable
// to its source.
func resolveRequestTimeout(raw, label string) (time.Duration, error) {
	if raw == "" {
		return defaultRequestTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("llmgateway: %s: %w", label, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("llmgateway: %s must be positive, got %q", label, raw)
	}
	return d, nil
}

// resolveProviderTimeout resolves the request timeout for one provider:
// providerRaw (ProviderConfig.RequestTimeout) when non-empty, validated
// via resolveRequestTimeout under a provider-attributed label — an
// explicit per-provider override always wins over the global default,
// house rule. Otherwise globalTimeout, the caller's already-resolved
// Config.RequestTimeout (buildAdapters, providers.go, resolves it once up
// front via resolveRequestTimeout, so a malformed global value fails
// construction even for a deployment where every provider happens to set
// its own override — a config bug must not lie dormant only to surface
// once someone adds a provider that relies on the default).
func resolveProviderTimeout(providerName, providerRaw string, globalTimeout time.Duration) (time.Duration, error) {
	if providerRaw == "" {
		return globalTimeout, nil
	}
	return resolveRequestTimeout(providerRaw, fmt.Sprintf("provider %q: requestTimeout", providerName))
}

// watchdogBody wraps an upstream response body so that a period of
// timeout with no successful read aborts the request instead of blocking
// forever. It is the idle-progress half of this feature's two mechanisms
// (Transport.ResponseHeaderTimeout, set in newAdapterHTTPClient, is the
// other — it only bounds the wait for headers, never the body).
//
// Deliberately NOT http.Client.Timeout: that bounds the whole request
// including the body read, which would truncate a healthy, long-running
// stream — exactly what this feature must never do. A watchdogBody
// instead resets on every successful, non-empty read, so a stream (or a
// large non-streaming body) that keeps making progress runs indefinitely;
// only SILENCE, never total duration, ends it.
//
// The mechanism: cancel is the CancelFunc for the *http.Request's own
// context, derived via context.WithCancel by the caller that built the
// request (upstreamBytes, providers.go; proxyUpstream, routes_passthrough.
// go) before this watchdogBody existed. Every successful Read resets timer
// to fire timeout in the future again; if timer fires first, fire cancels
// the request context, which unblocks whatever Read is currently in
// flight (or the next one) with an error. Read then substitutes that
// error for one wrapping errProviderTimeout, naming label, instead of
// passing through whatever the canceled context's own error text says —
// a real client disconnect (the parent context canceling instead of this
// watchdog) never sets timedOut, so it is never misreported this way.
// Close stops the timer (a no-op once already fired) and always calls
// cancel, so this watchdog never leaves a live timer or an un-canceled
// derived context behind on the hot path — every upstream request this
// gateway makes.
//
// onTimeout, when non-nil, is invoked once from fire (coordinator
// adversarial review, 2026-08-23, finding F4): a provider that sends
// headers and then stalls mid-body previously left provider-health
// accounting blind to the failure entirely — limiter.recordProviderAttempt
// is only ever reached from retryPolicy.do's own attemptRecorder call (at
// header-arrival time, before any body byte is read) and from
// proxyUpstream's identical call — so a chronic mid-body staller looked
// like 100% successful attempts to the discovery breaker and any
// request-path health signal that consumes those counters. onTimeout is
// that same attemptRecorder, reused rather than reinvented: callers look
// it up once via attemptRecorderFromContext and pass it straight through.
// This does mean a request that stalls mid-body is recorded as TWO
// attempts (one success at header time, one failure when the watchdog
// fires) rather than one attempt marked failed outright — deliberately:
// the alternative was deferring the FIRST recorder call until the body
// finishes, which would change when every other (already-tested,
// already-shipped) success path reports its attempt too, a much larger
// blast radius for what this fix needs. Two real, honestly-reported
// signals (headers arrived; the body then never finished) beats the
// previous zero.
//
// timedOut is accessed via sync/atomic's function API (atomic.StoreInt32/
// LoadInt32), not the newer atomic.Bool type, per this plugin's
// yaegi-interpreted-stdlib guardrail.
//
// onLatency/latencyStart/ttfb/streaming/ttfbRecorded/latencyRecorded
// (feat: instrument upstream latency, task brief) are this same
// watchdogBody's SECOND, independent observation: onLatency stays nil
// unless armLatency (below) is called with a non-nil recorder, and every
// access to the group below is gated on that one nil check first — a
// disabled deployment (or any caller that never observes latency at
// all, e.g. registry.go's discovery calls) pays one pointer comparison
// per Read and nothing else. See armLatency's own doc comment for why
// this is a separate method rather than additional newWatchdogBody
// parameters, and Read/Close below for where each field is written.
type watchdogBody struct {
	// latencyStart is declared first (fieldalignment, govet — enabled via
	// this repo's global golangci-lint config): time.Time embeds a
	// *time.Location, making it pointer-shaped, and every pointer-shaped
	// field in this struct is grouped at the front, then plain scalars
	// (time.Duration/int32/bool) last — see adminProviderView's identical
	// convention (admin.go) for the same reasoning spelled out once.
	latencyStart    time.Time
	rc              io.ReadCloser
	cancel          context.CancelFunc
	timer           *time.Timer
	onTimeout       attemptRecorder
	onLatency       latencyRecorder
	label           string
	timeout         time.Duration
	ttfb            time.Duration
	timedOut        int32
	ttfbRecorded    int32
	latencyRecorded int32
	streaming       bool
}

// newWatchdogBody returns a watchdogBody wrapping rc, arming its watchdog
// for timeout starting now — the moment the response's headers arrived,
// so even the very first body read is bounded by the same window ("sends
// headers then stalls mid-body" is caught here, not left to
// ResponseHeaderTimeout, which has already been satisfied by the time a
// body exists to wrap).
//
// label names what this watchdog guards, already formatted by the
// caller: providers.go passes `provider "openai"`; routes_passthrough.go
// passes its own logPrefix verbatim (`passthrough (provider "openai")` or
// `mcp target (name "foo")`) — NOT reworded here into a hardcoded
// "provider %q", which previously mislabeled an MCP/A2A target as a
// provider (coordinator adversarial review, 2026-08-23, finding F9).
// onTimeout may be nil (no attemptRecorder was ever wired onto this
// request's context — the common case for discovery's own listModels
// calls and the MCP/A2A target proxy, neither of which Feature A
// accounts).
func newWatchdogBody(rc io.ReadCloser, cancel context.CancelFunc, timeout time.Duration, label string, onTimeout attemptRecorder) *watchdogBody {
	wb := &watchdogBody{rc: rc, cancel: cancel, timeout: timeout, label: label, onTimeout: onTimeout}
	wb.timer = time.AfterFunc(timeout, wb.fire)
	return wb
}

// armLatency wires wb to observe upstream latency (feat: instrument
// upstream latency, task brief): rec is invoked exactly once, from
// Close below, with the latencySample (metrics.go) this watchdogBody
// measured. rec == nil disables all of it — Read/Close each take a
// single, cost-free nil check on wb.onLatency and do nothing further,
// so calling armLatency at all (even with rec nil) adds no measurable
// cost, and a caller may call it unconditionally.
//
// This is a separate method, not additional newWatchdogBody parameters:
// newWatchdogBody's existing signature and behavior are pinned by
// timeout_test.go's direct, positional 5-argument calls, which must
// keep passing unchanged (task constraint) — changing that signature
// would have broken every one of them for no benefit, since every real
// call site (providers.go, routes_passthrough.go) already holds a
// *watchdogBody reference to call this on immediately afterward.
//
// start is the moment just BEFORE the request was sent (the caller's
// own time.Now(), captured ahead of client.Do) — deliberately earlier
// than "now" here: newWatchdogBody itself only ever runs once the
// response's headers have already arrived, so using the caller's
// earlier timestamp is what makes both TTFB and total duration include
// the network round trip for headers too, matching the task brief's own
// definition ("from just before the upstream request is sent").
//
// streaming labels the eventual sample: the caller derives it from the
// response's own Content-Type via isEventStreamResponse (below) —
// deliberately not from the client's requested "stream" flag alone,
// since a provider that ignores it and answers with one buffered JSON
// body is, for latency purposes, indistinguishable from a genuinely
// non-streaming call: its TTFB IS its own total duration either way.
func (wb *watchdogBody) armLatency(start time.Time, streaming bool, rec latencyRecorder) {
	wb.onLatency = rec
	wb.latencyStart = start
	wb.streaming = streaming
}

// isEventStreamResponse reports whether resp's Content-Type indicates a
// text/event-stream body — the SAME convention each provider adapter's
// own forwardStream/forwardJSON split already applies (provider_openai.
// go, provider_anthropic.go, provider_gemini.go), duplicated here rather
// than imported from any one adapter file: newWatchdogBody has two call
// sites (providers.go, routes_passthrough.go), and routes_passthrough.go
// (native passthrough, the MCP/A2A target proxy) has no adapter-specific
// streaming decision of its own to share this with.
func isEventStreamResponse(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

// fire runs once, in its own goroutine (time.AfterFunc's own contract —
// no goroutine is ever spawned for a request whose timer is Stopped
// before it fires, which is the common, non-timed-out case), the moment
// timeout passes with no successful Read: it marks timedOut so the next
// Read error substitutes the timeout error, cancels the request context
// (unblocking whatever Read is currently parked waiting on the network),
// and — finding F4 — notifies onTimeout, if this watchdog has one, so
// provider-health accounting sees the failure even though no caller will
// ever read a body byte past this point.
func (wb *watchdogBody) fire() {
	atomic.StoreInt32(&wb.timedOut, 1)
	wb.cancel()
	if wb.onTimeout != nil {
		wb.onTimeout(nil, wb.timeoutError())
	}
}

// timeoutError builds the error Read returns once timedOut is set, and
// the error fire hands onTimeout — one construction, reused both places,
// so the two can never drift apart. Wraps errProviderTimeout only, not
// errUpstream: every caller that reads a body to completion already
// wraps errUpstream around whatever Read returns (forwardJSON/
// forwardStream/forwardMediaBody/listModels, providers.go and the
// translate-aware adapters alike), so wrapping it again here produced a
// doubled "upstream request failed: ... upstream request failed: ..."
// message (finding F9) with no compensating benefit — errors.Is(err,
// errUpstream) still holds once the caller's own wrap runs.
func (wb *watchdogBody) timeoutError() error {
	return fmt.Errorf("%w: %s: no upstream progress for %s", errProviderTimeout, wb.label, wb.timeout)
}

// Read implements io.Reader. A successful, non-empty read (err == nil and
// n > 0) resets the watchdog for another full timeout window — this is
// what lets a stream that keeps producing chunks run indefinitely: only
// silence, never total duration, ends it. n == 0 with err == nil (a
// legal, if unusual, io.Reader response — not currently produced by
// net/http's own body reader against a non-empty buffer, but not
// forbidden by the io.Reader contract either) does NOT reset the timer
// (finding F6): resetting on a read that delivered nothing would let a
// reader that keeps returning (0, nil) hold the watchdog open forever,
// exactly the naive-watchdog hole this feature exists to close. A failed
// read while timedOut is set (fire already ran) is reported via
// timeoutError, instead of whatever error canceling the request context
// actually produced. Any other error — a real client disconnect, a clean
// EOF, a genuine network failure that has nothing to do with this
// watchdog — passes through unchanged.
//
// The TTFB-capture block below (feat: instrument upstream latency) is
// purely additive and never changes what this method returns or the
// timer-reset logic's own behavior — it is checked FIRST, ahead of the
// `if err != nil` branch, and deliberately does NOT require err == nil
// the way the timer-reset block below still does: io.Reader's contract
// explicitly permits a reader to return its final data together with
// io.EOF in the SAME call (io.Reader's own doc: "an instance ... may
// return either err == EOF or err == nil"), and Go's real net/http body
// reader does exactly that for a small, fully-buffered response —
// measured directly against a real httptest server here, not assumed:
// a ~200-byte JSON completion arrives as ONE Read call reporting
// (n=200, err=io.EOF) together. Gating the capture on err == nil (as
// timer-reset still correctly does, per finding F6 below) would then
// skip it for every such response — the common case for a typical,
// fully-buffered non-streaming completion — leaving hasTTFB false for
// most real traffic. Checking `n > 0` alone, before the error branch,
// fixes that without touching the timer-reset semantics those tests
// pin: a self-contained top-of-function check that only ever WRITES
// wb.ttfb/wb.ttfbRecorded, never wb.timer or any error path.
func (wb *watchdogBody) Read(p []byte) (int, error) {
	n, err := wb.rc.Read(p)
	if n > 0 && wb.onLatency != nil && atomic.LoadInt32(&wb.ttfbRecorded) == 0 {
		wb.ttfb = time.Since(wb.latencyStart)
		atomic.StoreInt32(&wb.ttfbRecorded, 1)
	}
	if err != nil {
		if atomic.LoadInt32(&wb.timedOut) == 1 {
			return n, wb.timeoutError()
		}
		return n, err
	}
	if n > 0 {
		wb.timer.Reset(wb.timeout)
	}
	return n, nil
}

// Close stops the watchdog timer (a harmless no-op if it already fired)
// and always calls cancel, so the derived request context this
// watchdogBody's cancel belongs to is released promptly — whether Close
// is reached because the body was read to completion, because the caller
// gave up early, or because the watchdog itself already fired — instead
// of being left to be reclaimed only once its parent context ends. Then
// closes the wrapped body.
//
// The `if wb.onLatency != nil` block below (feat: instrument upstream
// latency) is purely additive, runs after the pre-existing timer.Stop/
// cancel/rc.Close sequence above is already decided, and never changes
// this method's return value: it reports exactly one latencySample
// (metrics.go) — duration always (time.Since(wb.latencyStart), this
// body's whole lifetime), ttfb only when a successful read actually
// happened first (hasTTFB, guarded by ttfbRecorded — Read's own doc
// comment above). latencyRecorded is a CompareAndSwap, not a plain
// check, so a caller that closes this watchdogBody more than once (not
// expected by any current call site, but not this method's job to
// forbid either) still reports the sample exactly once rather than
// double-counting it.
func (wb *watchdogBody) Close() error {
	wb.timer.Stop()
	wb.cancel()
	if wb.onLatency != nil && atomic.CompareAndSwapInt32(&wb.latencyRecorded, 0, 1) {
		sample := latencySample{
			duration:  time.Since(wb.latencyStart),
			streaming: wb.streaming,
		}
		if atomic.LoadInt32(&wb.ttfbRecorded) == 1 {
			sample.ttfb = wb.ttfb
			sample.hasTTFB = true
		}
		wb.onLatency(sample)
	}
	return wb.rc.Close()
}
