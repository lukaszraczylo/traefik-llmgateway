package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// upstream-error fallback — a watchdog firing is always this gateway
// giving up on an upstream that stopped making progress: a provider
// failure to attribute to that provider, never the client's own doing.
// watchdogBody.Read never lets a timeout error wrap context.Canceled, so
// the two stay mutually exclusive in every caller's classification.
var errProviderTimeout = errors.New("llmgateway: provider request timed out")

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
// instead resets on every successful read, so a stream (or a large
// non-streaming body) that keeps making progress runs indefinitely; only
// SILENCE, never total duration, ends it.
//
// The mechanism: cancel is the CancelFunc for the *http.Request's own
// context, derived via context.WithCancel by the caller that built the
// request (upstreamBytes, providers.go; proxyUpstream, routes_passthrough.
// go) before this watchdogBody existed. Every successful Read resets timer
// to fire timeout in the future again; if timer fires first, fire cancels
// the request context, which unblocks whatever Read is currently in
// flight (or the next one) with an error. Read then substitutes that
// error for one wrapping errProviderTimeout, naming providerName, instead
// of passing through whatever the canceled context's own error text says
// — a real client disconnect (the parent context canceling instead of
// this watchdog) never sets timedOut, so it is never misreported this way.
// Close stops the timer (a no-op once already fired) and always calls
// cancel, so this watchdog never leaves a live timer or an un-canceled
// derived context behind on the hot path — every upstream request this
// gateway makes.
//
// timedOut is accessed via sync/atomic's function API (atomic.StoreInt32/
// LoadInt32), not the newer atomic.Bool type, per this plugin's
// yaegi-interpreted-stdlib guardrail.
type watchdogBody struct {
	rc           io.ReadCloser
	cancel       context.CancelFunc
	timer        *time.Timer
	providerName string
	timeout      time.Duration
	timedOut     int32
}

// newWatchdogBody returns a watchdogBody wrapping rc, arming its watchdog
// for timeout starting now — the moment the response's headers arrived,
// so even the very first body read is bounded by the same window ("sends
// headers then stalls mid-body" is caught here, not left to
// ResponseHeaderTimeout, which has already been satisfied by the time a
// body exists to wrap).
func newWatchdogBody(rc io.ReadCloser, cancel context.CancelFunc, timeout time.Duration, providerName string) *watchdogBody {
	wb := &watchdogBody{rc: rc, cancel: cancel, timeout: timeout, providerName: providerName}
	wb.timer = time.AfterFunc(timeout, wb.fire)
	return wb
}

// fire runs once, in its own goroutine (time.AfterFunc's own contract —
// no goroutine is ever spawned for a request whose timer is Stopped
// before it fires, which is the common, non-timed-out case), the moment
// timeout passes with no successful Read: it marks timedOut so the next
// Read error substitutes the provider-attributed timeout error, then
// cancels the request context, unblocking whatever Read is currently
// parked waiting on the network.
func (wb *watchdogBody) fire() {
	atomic.StoreInt32(&wb.timedOut, 1)
	wb.cancel()
}

// Read implements io.Reader. A successful read (err == nil) resets the
// watchdog for another full timeout window — this is what lets a stream
// that keeps producing chunks run indefinitely: only silence, never total
// duration, ends it. A failed read while timedOut is set (fire already
// ran) is reported as errProviderTimeout, naming providerName and the
// configured timeout, instead of whatever error canceling the request
// context actually produced. Any other error — a real client disconnect,
// a clean EOF, a genuine network failure that has nothing to do with this
// watchdog — passes through unchanged.
func (wb *watchdogBody) Read(p []byte) (int, error) {
	n, err := wb.rc.Read(p)
	if err != nil {
		if atomic.LoadInt32(&wb.timedOut) == 1 {
			return n, fmt.Errorf("%w: %w: provider %q: no upstream progress for %s", errUpstream, errProviderTimeout, wb.providerName, wb.timeout)
		}
		return n, err
	}
	wb.timer.Reset(wb.timeout)
	return n, nil
}

// Close stops the watchdog timer (a harmless no-op if it already fired)
// and always calls cancel, so the derived request context this
// watchdogBody's cancel belongs to is released promptly — whether Close
// is reached because the body was read to completion, because the caller
// gave up early, or because the watchdog itself already fired — instead
// of being left to be reclaimed only once its parent context ends. Then
// closes the wrapped body.
func (wb *watchdogBody) Close() error {
	wb.timer.Stop()
	wb.cancel()
	return wb.rc.Close()
}
