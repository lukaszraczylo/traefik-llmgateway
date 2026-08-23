package traefikllmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeResp builds a canned *http.Response for retryPolicy.do tests that
// exercise the transience decision without a real network round trip.
// body is always readable and closable, matching a real *http.Response.
func fakeResp(status int, hdr http.Header) *http.Response {
	if hdr == nil {
		hdr = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: hdr, Body: io.NopCloser(strings.NewReader("body"))}
}

// retryAfterResp builds a fakeResp carrying a Retry-After header set to
// the raw value given — deliberately untyped (a caller passes a bogus
// or negative string too) so waitBefore's parsing branches can be
// exercised directly.
func retryAfterResp(status int, retryAfter string) *http.Response {
	h := http.Header{}
	h.Set("Retry-After", retryAfter)
	return fakeResp(status, h)
}

// spyWait returns a waitFn that records every requested duration
// instead of actually waiting, and always reports the wait completed
// (true), so retry-wait tests run instantly and can assert on the exact
// durations retryPolicy.do computed.
func spyWait(waits *[]time.Duration) func(context.Context, time.Duration) bool {
	return func(_ context.Context, d time.Duration) bool {
		*waits = append(*waits, d)
		return true
	}
}

// callSequence returns a call func for retryPolicy.do that returns
// results[i] on its i-th invocation (0-indexed), and counts invocations
// into calls.
func callSequence(calls *int32, results ...func() (*http.Response, error)) func() (*http.Response, error) {
	return func() (*http.Response, error) {
		n := int(atomic.AddInt32(calls, 1)) - 1
		if n >= len(results) {
			n = len(results) - 1
		}
		return results[n]()
	}
}

// TestIsTransient covers the transience classification retryPolicy.do
// relies on: net errors (wrapping errUpstream) and HTTP 429/5xx are
// transient; a successful or non-429 4xx status, a context
// cancellation/deadline error, and a request-build failure, are not.
func TestIsTransient(t *testing.T) {
	tests := []struct {
		resp *http.Response
		err  error
		name string
		want bool
	}{
		{name: "503", resp: fakeResp(http.StatusServiceUnavailable, nil), want: true},
		{name: "500", resp: fakeResp(http.StatusInternalServerError, nil), want: true},
		{name: "429", resp: fakeResp(http.StatusTooManyRequests, nil), want: true},
		{name: "200", resp: fakeResp(http.StatusOK, nil), want: false},
		{name: "400", resp: fakeResp(http.StatusBadRequest, nil), want: false},
		{name: "404", resp: fakeResp(http.StatusNotFound, nil), want: false},
		{name: "network error", err: fmt.Errorf("%w: dial tcp: connection reset", errUpstream), want: true},
		{name: "context canceled", err: fmt.Errorf("%w: %w", errUpstream, context.Canceled), want: false},
		{name: "context deadline exceeded", err: fmt.Errorf("%w: %w", errUpstream, context.DeadlineExceeded), want: false},
		{name: "request build failure", err: fmt.Errorf("%w: %w: build request: %w", errUpstream, errRequestBuildFailed, errors.New("invalid method")), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransient(tt.resp, tt.err); got != tt.want {
				t.Errorf("isTransient(%v, %v) = %v, want %v", tt.resp, tt.err, got, tt.want)
			}
		})
	}
}

// TestNewRetryPolicy_Disabled asserts the zero-value RetryConfig (the
// v0.1-preserving default) produces a policy with enabled=false and no
// validation error, even though Attempts/Backoff are left at their zero
// values, and that it always carries a non-nil waitFn (totalTries()
// never needs one, but do() calls p.waitFn only when a retry is
// warranted, so a nil waitFn would only panic on a path this policy
// shape never takes — still, newRetryPolicy always sets one).
func TestNewRetryPolicy_Disabled(t *testing.T) {
	p, err := newRetryPolicy(RetryConfig{})
	if err != nil {
		t.Fatalf("newRetryPolicy: %v", err)
	}
	if p == nil {
		t.Fatal("newRetryPolicy returned a nil policy for a valid (disabled) config")
	}
	if p.enabled {
		t.Error("p.enabled = true, want false for RetryConfig{}")
	}
	if p.waitFn == nil {
		t.Error("p.waitFn is nil, want a non-nil default")
	}
	if got := p.totalTries(); got != 1 {
		t.Errorf("p.totalTries() = %d, want 1 (disabled: no retry)", got)
	}
}

// TestNewRetryPolicy_Defaults asserts Enabled=true with Attempts/Backoff
// left unset applies the spec defaults: 1 retry (two tries total, not
// one) and 250ms backoff.
func TestNewRetryPolicy_Defaults(t *testing.T) {
	p, err := newRetryPolicy(RetryConfig{Enabled: true})
	if err != nil {
		t.Fatalf("newRetryPolicy: %v", err)
	}
	if !p.enabled {
		t.Error("p.enabled = false, want true")
	}
	if p.attempts != 1 {
		t.Errorf("p.attempts = %d, want 1 (retries after the first try)", p.attempts)
	}
	if p.backoff != 250*time.Millisecond {
		t.Errorf("p.backoff = %v, want 250ms", p.backoff)
	}
	if p.waitFn == nil {
		t.Error("p.waitFn is nil, want a non-nil default")
	}
	if got := p.totalTries(); got != 2 {
		t.Errorf("p.totalTries() = %d, want 2 (1 initial try + 1 default retry)", got)
	}
}

// TestNewRetryPolicy_ValidatesAttempts covers the 1..3 range —
// Attempts counts retries after the first try, so this range means
// "1..3 retries", not "1..3 tries".
func TestNewRetryPolicy_ValidatesAttempts(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
		attempt int
	}{
		{name: "min", attempt: 1, wantErr: false},
		{name: "max", attempt: 3, wantErr: false},
		{name: "mid", attempt: 2, wantErr: false},
		{name: "zero uses default", attempt: 0, wantErr: false},
		{name: "too high", attempt: 4, wantErr: true},
		{name: "negative", attempt: -1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := newRetryPolicy(RetryConfig{Enabled: true, Attempts: tt.attempt})
			if (err != nil) != tt.wantErr {
				t.Errorf("newRetryPolicy(Attempts:%d): err = %v, wantErr %v", tt.attempt, err, tt.wantErr)
			}
		})
	}
}

// TestNewRetryPolicy_ValidatesBackoff covers backoff parsing: an
// unparseable duration string is a config error; a valid one is used
// verbatim (not defaulted).
func TestNewRetryPolicy_ValidatesBackoff(t *testing.T) {
	if _, err := newRetryPolicy(RetryConfig{Enabled: true, Backoff: "not-a-duration"}); err == nil {
		t.Fatal("newRetryPolicy: want error for unparseable backoff, got nil")
	}
	p, err := newRetryPolicy(RetryConfig{Enabled: true, Backoff: "10ms"})
	if err != nil {
		t.Fatalf("newRetryPolicy: %v", err)
	}
	if p.backoff != 10*time.Millisecond {
		t.Errorf("p.backoff = %v, want 10ms", p.backoff)
	}
}

// TestRetryPolicy_TotalTries pins totalTries()'s retries-to-tries
// conversion directly, including the defensive clamp on a malformed
// attempts value (0 or negative) reachable only by constructing a
// *retryPolicy by hand rather than through newRetryPolicy.
func TestRetryPolicy_TotalTries(t *testing.T) {
	tests := []struct {
		policy *retryPolicy
		name   string
		want   int
	}{
		{nil, "nil policy", 1},
		{&retryPolicy{enabled: false, attempts: 5}, "disabled, attempts ignored", 1},
		{&retryPolicy{enabled: true, attempts: 1}, "enabled, attempts=1 (default)", 2},
		{&retryPolicy{enabled: true, attempts: 2}, "enabled, attempts=2", 3},
		{&retryPolicy{enabled: true, attempts: 3}, "enabled, attempts=3 (max)", 4},
		{&retryPolicy{enabled: true, attempts: 0}, "enabled, attempts=0 (malformed, clamped to 1)", 2},
		{&retryPolicy{enabled: true, attempts: -5}, "enabled, attempts=-5 (malformed, clamped to 1)", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.totalTries(); got != tt.want {
				t.Errorf("totalTries() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestRetryPolicy_Do_MalformedAttempts_NeverReturnsNilNil proves the
// totalTries() clamp keeps do's loop from ever skipping call() entirely:
// a hand-built policy with an out-of-range attempts value (impossible
// through newRetryPolicy, but not impossible through a direct struct
// literal) must still make at least one real attempt, never returning
// the disallowed (nil, nil) shape.
func TestRetryPolicy_Do_MalformedAttempts_NeverReturnsNilNil(t *testing.T) {
	var calls int32
	p := &retryPolicy{enabled: true, attempts: -1, backoff: time.Millisecond, waitFn: spyWait(&[]time.Duration{})}
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil })

	resp, err := p.do(context.Background(), call)
	if resp == nil && err == nil {
		t.Fatal("do returned (nil, nil): call() was never invoked despite a malformed attempts value")
	}
	if got := atomic.LoadInt32(&calls); got < 1 {
		t.Errorf("calls = %d, want at least 1", got)
	}
}

// TestRetryPolicy_Do_EnabledDefaults_ExactlyOneRetry is the ruling's own
// acceptance case: retry:{enabled:true} with every other field left at
// its default must perform exactly one retry (two tries total), not
// zero — the bug the controller's review caught.
func TestRetryPolicy_Do_EnabledDefaults_ExactlyOneRetry(t *testing.T) {
	p, err := newRetryPolicy(RetryConfig{Enabled: true})
	if err != nil {
		t.Fatalf("newRetryPolicy: %v", err)
	}
	var waits []time.Duration
	p.waitFn = spyWait(&waits)

	var calls int32
	call := callSequence(&calls,
		func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil },
		func() (*http.Response, error) { return fakeResp(http.StatusOK, nil), nil },
	)
	resp, err := p.do(context.Background(), call)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("calls = %d, want 2 (default attempts=1 means one retry: two tries total)", got)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("resp.StatusCode = %d, want 200", resp.StatusCode)
	}
	if len(waits) != 1 {
		t.Errorf("waited %d times, want exactly 1", len(waits))
	}
}

// TestRetryPolicy_Do_EnabledDefaults_CapsAtTwoTotalTries is the same
// default-config case against an upstream that never succeeds: it must
// stop at 2 total tries (1 initial + 1 default retry), not loop forever
// and not stop at 1.
func TestRetryPolicy_Do_EnabledDefaults_CapsAtTwoTotalTries(t *testing.T) {
	p, err := newRetryPolicy(RetryConfig{Enabled: true})
	if err != nil {
		t.Fatalf("newRetryPolicy: %v", err)
	}
	p.waitFn = spyWait(&[]time.Duration{})

	var calls int32
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil })
	resp, err := p.do(context.Background(), call)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("calls = %d, want 2 (1 initial + 1 default retry)", got)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("resp.StatusCode = %d, want 503 (last attempt's result)", resp.StatusCode)
	}
}

// TestRetryPolicy_Do_RetriesOnTransientThenSucceeds covers the brief's
// three canonical transient-then-success cases: 503, a raw network error
// (connection reset), and 429 with a qualifying Retry-After header. Each
// uses attempts:1 (one retry allowed) and retries exactly once, waits
// the expected duration, and returns the second attempt's success.
func TestRetryPolicy_Do_RetriesOnTransientThenSucceeds(t *testing.T) {
	tests := []struct {
		name      string
		first     func() (*http.Response, error)
		wantWaits []time.Duration
	}{
		{
			name:      "503 then success",
			first:     func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil },
			wantWaits: []time.Duration{100 * time.Millisecond}, // backoff * 2^0
		},
		{
			name: "connection reset then success",
			first: func() (*http.Response, error) {
				return nil, fmt.Errorf("%w: dial tcp: connection reset by peer", errUpstream)
			},
			wantWaits: []time.Duration{100 * time.Millisecond},
		},
		{
			name:      "429 with Retry-After then success",
			first:     func() (*http.Response, error) { return retryAfterResp(http.StatusTooManyRequests, "1"), nil },
			wantWaits: []time.Duration{time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int32
			var waits []time.Duration
			p := &retryPolicy{enabled: true, attempts: 1, backoff: 100 * time.Millisecond, waitFn: spyWait(&waits)}
			call := callSequence(&calls, tt.first, func() (*http.Response, error) {
				return fakeResp(http.StatusOK, nil), nil
			})

			resp, err := p.do(context.Background(), call)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("resp.StatusCode = %d, want 200", resp.StatusCode)
			}
			if got := atomic.LoadInt32(&calls); got != 2 {
				t.Errorf("calls = %d, want 2 (one retry)", got)
			}
			if len(waits) != len(tt.wantWaits) || (len(waits) > 0 && waits[0] != tt.wantWaits[0]) {
				t.Errorf("waits = %v, want %v", waits, tt.wantWaits)
			}
		})
	}
}

// TestRetryPolicy_WaitBefore_RetryAfterHandling pins spec §1's
// Retry-After handling directly: only a 429 whose Retry-After parses as
// a non-negative whole-second count within the 2s cap wins outright.
// Every other shape — over the cap, the HTTP-date form, negative, no
// header at all, or present on a non-429 status — falls back to
// exponential backoff (spec: ">2s -> exponential backoff", not a flat
// 2s), verified here against attempt=1's backoff*2^0 value.
func TestRetryPolicy_WaitBefore_RetryAfterHandling(t *testing.T) {
	p := &retryPolicy{backoff: 100 * time.Millisecond}
	backoffFallback := 100 * time.Millisecond // attempt=1: backoff * 2^0

	tests := []struct {
		resp *http.Response
		name string
		want time.Duration
	}{
		{retryAfterResp(http.StatusTooManyRequests, "1"), "429 with qualifying Retry-After (1s, within cap)", time.Second},
		{retryAfterResp(http.StatusTooManyRequests, "30"), "429 with Retry-After over the 2s cap falls back to exponential backoff", backoffFallback},
		{retryAfterResp(http.StatusTooManyRequests, "Wed, 21 Oct 2015 07:28:00 GMT"), "429 with HTTP-date Retry-After falls back to exponential backoff", backoffFallback},
		{retryAfterResp(http.StatusTooManyRequests, "-5"), "429 with negative Retry-After falls back to exponential backoff", backoffFallback},
		{fakeResp(http.StatusTooManyRequests, nil), "429 with no Retry-After header falls back to exponential backoff", backoffFallback},
		{retryAfterResp(http.StatusServiceUnavailable, "1"), "503 with a Retry-After header is ignored (not 429)", backoffFallback},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := p.waitBefore(1, tt.resp); got != tt.want {
				t.Errorf("waitBefore(1, ...) = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRetryPolicy_Do_NonTransientStatus_NoRetry asserts a 400 response
// (not 429, not 5xx) is returned as-is on the first attempt, with no
// retry and no wait.
func TestRetryPolicy_Do_NonTransientStatus_NoRetry(t *testing.T) {
	var calls int32
	var waits []time.Duration
	p := &retryPolicy{enabled: true, attempts: 3, backoff: time.Millisecond, waitFn: spyWait(&waits)}
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusBadRequest, nil), nil })

	resp, err := p.do(context.Background(), call)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("resp.StatusCode = %d, want 400", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (no retry on non-transient status)", got)
	}
	if len(waits) != 0 {
		t.Errorf("waits = %v, want none", waits)
	}
}

// TestRetryPolicy_Do_DisabledPolicy_NoRetry asserts an enabled=false
// policy (the v0.1-preserving default shape) tries exactly once even on
// a transient failure.
func TestRetryPolicy_Do_DisabledPolicy_NoRetry(t *testing.T) {
	var calls int32
	p := &retryPolicy{enabled: false, waitFn: func(context.Context, time.Duration) bool {
		t.Error("waitFn called on a disabled policy")
		return true
	}}
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil })

	resp, err := p.do(context.Background(), call)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("resp.StatusCode = %d, want 503", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (retry disabled)", got)
	}
}

// TestRetryPolicy_Do_NilPolicy_NoRetry asserts a nil *retryPolicy — the
// shape every adapter constructed by v0.1's plain constructors has, since
// they never set the retry field — behaves identically to an explicitly
// disabled one: exactly one attempt, no panic.
func TestRetryPolicy_Do_NilPolicy_NoRetry(t *testing.T) {
	var calls int32
	var p *retryPolicy
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil })

	resp, err := p.do(context.Background(), call)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("resp.StatusCode = %d, want 503", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (nil policy = disabled)", got)
	}
}

// TestRetryPolicy_Do_AttemptsCap asserts a policy that never sees
// success stops after exactly p.attempts+1 total tries (attempts
// retries after the first), waiting p.attempts times with exponential
// backoff, capped at maxRetryWait.
func TestRetryPolicy_Do_AttemptsCap(t *testing.T) {
	var calls int32
	var waits []time.Duration
	p := &retryPolicy{enabled: true, attempts: 3, backoff: time.Second, waitFn: spyWait(&waits)}
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil })

	resp, err := p.do(context.Background(), call)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Errorf("calls = %d, want 4 (1 initial + 3 retries)", got)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("resp.StatusCode = %d, want 503 (last attempt's result)", resp.StatusCode)
	}
	if len(waits) != 3 {
		t.Fatalf("waits = %v, want 3 entries (between 4 attempts)", waits)
	}
	if waits[0] != time.Second {
		t.Errorf("waits[0] = %v, want 1s (backoff * 2^0)", waits[0])
	}
	if waits[1] != 2*time.Second {
		t.Errorf("waits[1] = %v, want 2s (backoff * 2^1, at the cap)", waits[1])
	}
	if waits[2] != 2*time.Second {
		t.Errorf("waits[2] = %v, want 2s (backoff * 2^2, clamped to the cap)", waits[2])
	}
}

// TestRetryPolicy_Do_BackoffCappedAtMaxWait asserts a backoff whose
// exponential growth would exceed maxRetryWait is clamped to it.
func TestRetryPolicy_Do_BackoffCappedAtMaxWait(t *testing.T) {
	var calls int32
	var waits []time.Duration
	p := &retryPolicy{enabled: true, attempts: 3, backoff: 2 * time.Second, waitFn: spyWait(&waits)}
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil })

	if _, err := p.do(context.Background(), call); err != nil {
		t.Fatalf("do: %v", err)
	}
	if len(waits) != 3 {
		t.Fatalf("waits = %v, want 3 entries", waits)
	}
	for i, w := range waits {
		if w > maxRetryWait {
			t.Errorf("waits[%d] = %v, exceeds cap %v", i, w, maxRetryWait)
		}
	}
}

// TestRetryPolicy_Do_CtxAlreadyCanceled_NoRetry asserts a context
// canceled before do() even starts stops the loop after the first
// attempt: the real (ctx-aware) defaultWaitFn resolves the already-closed
// ctx.Done() case rather than blocking out the backoff.
func TestRetryPolicy_Do_CtxAlreadyCanceled_NoRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before do() ever runs

	var calls int32
	p := &retryPolicy{enabled: true, attempts: 3, backoff: 50 * time.Millisecond, waitFn: defaultWaitFn}
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil })

	resp, err := p.do(ctx, call)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (ctx already done, no retry)", got)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("resp.StatusCode = %d, want 503", resp.StatusCode)
	}
}

// TestRetryPolicy_Do_CtxCanceledDuringWait_ReturnsPromptly proves the
// ctx-aware waitFn aborts an in-progress backoff wait the moment ctx
// ends, rather than blocking out the full duration: a 2s backoff against
// a 20ms context deadline must return in well under 2s.
func TestRetryPolicy_Do_CtxCanceledDuringWait_ReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	var calls int32
	p := &retryPolicy{enabled: true, attempts: 2, backoff: 2 * time.Second, waitFn: defaultWaitFn}
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil })

	start := time.Now()
	resp, err := p.do(ctx, call)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("do took %v, want well under the 2s backoff (ctx should abort the wait promptly)", elapsed)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls = %d, want 1 (ctx canceled during the wait, no further attempt)", got)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("resp.StatusCode = %d, want 503 (last attempt's result)", resp.StatusCode)
	}
}

// trackingReadCloser wraps an io.Reader with a Close method and an EOF
// hook, for asserting a retried-away response body was both drained
// (read to EOF) and closed — not just closed with data left unread.
type trackingReadCloser struct {
	io.Reader
	onClose func()
	onEOF   func()
}

func (c *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	if err == io.EOF {
		c.onEOF()
	}
	return n, err
}

func (c *trackingReadCloser) Close() error {
	c.onClose()
	return nil
}

// TestRetryPolicy_Do_ClosesFailedAttemptBody asserts a retried-away
// response's body is both drained to EOF and closed before the next
// attempt, so the underlying connection is eligible for reuse instead of
// being forced closed by an un-drained body.
func TestRetryPolicy_Do_ClosesFailedAttemptBody(t *testing.T) {
	var calls int32
	var waits []time.Duration
	closed := false
	drained := false
	firstBody := &trackingReadCloser{
		Reader:  strings.NewReader("stale body"),
		onClose: func() { closed = true },
		onEOF:   func() { drained = true },
	}
	p := &retryPolicy{enabled: true, attempts: 1, backoff: time.Millisecond, waitFn: spyWait(&waits)}
	call := callSequence(&calls,
		func() (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: firstBody}, nil
		},
		func() (*http.Response, error) { return fakeResp(http.StatusOK, nil), nil },
	)

	if _, err := p.do(context.Background(), call); err != nil {
		t.Fatalf("do: %v", err)
	}
	if !drained {
		t.Error("failed attempt's response body was never fully drained (read to EOF)")
	}
	if !closed {
		t.Error("failed attempt's response body was never closed")
	}
}

// TestUpstreamJSON_RequestBuildFailure_NotRetried proves a failure to
// build the outgoing *http.Request (an invalid method, here) is
// classified non-transient and never retried — retrying an identically
// malformed request would only waste attempts on a guaranteed repeat
// failure.
func TestUpstreamJSON_RequestBuildFailure_NotRetried(t *testing.T) {
	p := &retryPolicy{enabled: true, attempts: 3, backoff: time.Millisecond, waitFn: func(context.Context, time.Duration) bool {
		t.Error("waitFn called: a request-build failure must not be retried")
		return true
	}}
	client := &http.Client{}
	_, err := upstreamJSON(context.Background(), client, "IN VALID", "http://127.0.0.1:1/x", nil, nil, p, defaultRequestTimeout, "p1")
	if err == nil {
		t.Fatal("upstreamJSON: want an error for an invalid method, got nil")
	}
	if !errors.Is(err, errRequestBuildFailed) {
		t.Errorf("err = %v, want errors.Is(err, errRequestBuildFailed)", err)
	}
	if !errors.Is(err, errUpstream) {
		t.Errorf("err = %v, want it to still wrap errUpstream too", err)
	}
}

// TestBuildAdapters_WiresRetryPolicyFromConfig asserts buildAdapters
// derives a *retryPolicy from cfg.Retry and attaches it to every
// constructed adapter, and that an invalid Retry config fails
// construction the same way any other bad ProviderConfig does.
func TestBuildAdapters_WiresRetryPolicyFromConfig(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"p1": {Type: "openai", APIKey: "k"}}
	cfg.Retry = RetryConfig{Enabled: true, Attempts: 2, Backoff: "50ms"}

	adapters, err := buildAdapters(cfg)
	if err != nil {
		t.Fatalf("buildAdapters: %v", err)
	}
	oa, ok := adapters["p1"].(*openaiAdapter)
	if !ok {
		t.Fatalf("adapters[%q] = %T, want *openaiAdapter", "p1", adapters["p1"])
	}
	if oa.retry == nil {
		t.Fatal("oa.retry is nil, want a wired retryPolicy")
	}
	if !oa.retry.enabled || oa.retry.attempts != 2 || oa.retry.backoff != 50*time.Millisecond {
		t.Errorf("oa.retry = %+v, want enabled=true attempts=2 backoff=50ms", oa.retry)
	}
	if got := oa.retry.totalTries(); got != 3 {
		t.Errorf("oa.retry.totalTries() = %d, want 3 (1 initial + 2 retries)", got)
	}
}

// TestBuildAdapters_InvalidRetryConfig_ReturnsError asserts an
// out-of-range Retry.Attempts fails buildAdapters the same way any other
// invalid config value does.
func TestBuildAdapters_InvalidRetryConfig_ReturnsError(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"p1": {Type: "openai", APIKey: "k"}}
	cfg.Retry = RetryConfig{Enabled: true, Attempts: 9}

	if _, err := buildAdapters(cfg); err == nil {
		t.Fatal("buildAdapters: want error for Attempts=9, got nil")
	}
}

// TestNewGateway_RetryDisabledByDefault_Unwired asserts a Config that
// never sets Retry at all — every v0.1 config — produces adapters whose
// retry field is a disabled (not necessarily nil, but enabled=false)
// policy, matching v0.1's always-one-attempt behavior.
func TestNewGateway_RetryDisabledByDefault_Unwired(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	oa, ok := gw.adapters["openai"].(*openaiAdapter)
	if !ok {
		t.Fatalf("adapters[openai] = %T, want *openaiAdapter", gw.adapters["openai"])
	}
	if oa.retry != nil && oa.retry.enabled {
		t.Errorf("oa.retry = %+v, want disabled (Retry never configured)", oa.retry)
	}
}

// TestRetryConfig_JSONDecode_WiresIntoGateway proves RetryConfig decodes
// correctly off the wire (JSON — Traefik's YAML dynamic config decodes
// to the same shape, per manifest_test.go's own note) and that
// newGateway builds a policy reflecting exactly what was decoded:
// attempts=2 means 2 retries (3 total tries), not the literal number of
// tries.
func TestRetryConfig_JSONDecode_WiresIntoGateway(t *testing.T) {
	cfg := CreateConfig()
	blob := `{"providers":{"openai":{"type":"openai","apiKey":"k"}},
	  "retry":{"enabled":true,"attempts":2,"backoff":"100ms"}}`
	if err := json.Unmarshal([]byte(blob), cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !cfg.Retry.Enabled || cfg.Retry.Attempts != 2 || cfg.Retry.Backoff != "100ms" {
		t.Fatalf("decoded cfg.Retry = %+v, want {Enabled:true Attempts:2 Backoff:100ms}", cfg.Retry)
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	oa, ok := gw.adapters["openai"].(*openaiAdapter)
	if !ok {
		t.Fatalf("adapters[openai] = %T, want *openaiAdapter", gw.adapters["openai"])
	}
	if oa.retry == nil || !oa.retry.enabled {
		t.Fatalf("oa.retry = %+v, want an enabled policy", oa.retry)
	}
	if oa.retry.attempts != 2 {
		t.Errorf("oa.retry.attempts = %d, want 2 (retries after the first try)", oa.retry.attempts)
	}
	if got := oa.retry.totalTries(); got != 3 {
		t.Errorf("oa.retry.totalTries() = %d, want 3 (1 initial + 2 retries)", got)
	}
}

// ---- adapter-level wiring tests (stream-initiation path) ----

// TestOpenAIAdapter_ChatCompletion_NonStream_RetriedOnTransientFailure
// proves upstreamJSON's retry covers a non-streaming chatCompletion call
// end to end: the client sees only the second (successful) response, and
// the upstream received exactly two requests.
func TestOpenAIAdapter_ChatCompletion_NonStream_RetriedOnTransientFailure(t *testing.T) {
	const okBody = `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":2}}`
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	a.retry = &retryPolicy{enabled: true, attempts: 2, backoff: time.Millisecond, waitFn: func(context.Context, time.Duration) bool { return true }}
	rec := httptest.NewRecorder()

	u, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}})
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream calls = %d, want 2", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("rec.Code = %d, want 200", rec.Code)
	}
	if rec.Body.String() != okBody {
		t.Errorf("rec.Body = %q, want %q", rec.Body.String(), okBody)
	}
	if u.prompt != 1 || u.completion != 2 {
		t.Errorf("usage = %+v, want {1 2}", u)
	}
}

// TestOpenAIAdapter_ChatCompletion_Stream_RetriedBeforeFirstChunk proves
// a streaming request whose first upstream exchange fails (before any
// SSE chunk exists) is retried, and the client receives only the
// successful stream — no trace of the failed first attempt.
func TestOpenAIAdapter_ChatCompletion_Stream_RetriedBeforeFirstChunk(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, f := range streamFrames {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", f)
			fl.Flush()
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	a.retry = &retryPolicy{enabled: true, attempts: 2, backoff: time.Millisecond, waitFn: func(context.Context, time.Duration) bool { return true }}
	rec := httptest.NewRecorder()

	u, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}, "stream": true})
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream calls = %d, want 2", got)
	}
	if u.prompt != 7 || u.completion != 9 {
		t.Errorf("usage = %+v, want {7 9}", u)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Hel") || !strings.Contains(body, "[DONE]") {
		t.Errorf("rec.Body = %q, want the full successful stream", body)
	}
	if strings.Count(body, "[DONE]") != 1 {
		t.Errorf("rec.Body contains %d [DONE] markers, want exactly 1 (no leaked failed-attempt output)", strings.Count(body, "[DONE]"))
	}
}

// TestOpenAIAdapter_ChatCompletion_Stream_MidDeathNotRetried proves a
// stream that dies after already forwarding a chunk is NOT retried: the
// client keeps the partial data it already received (proving those bytes
// were forwarded), chatCompletion returns an error, and the upstream saw
// exactly one request.
func TestOpenAIAdapter_ChatCompletion_Stream_MidDeathNotRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", streamFrames[0])
		fl.Flush()

		// t.Error (not t.Fatal) — this handler runs on its own goroutine,
		// spawned by net/http's server; t.FailNow (which t.Fatal calls)
		// is documented to be safe only from the goroutine running the
		// test function itself.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("ResponseWriter does not support Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		_ = conn.Close() // abrupt drop mid-chunked-body: client sees a read error, not a clean EOF
	}))
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	a.retry = &retryPolicy{enabled: true, attempts: 3, backoff: time.Millisecond, waitFn: func(context.Context, time.Duration) bool {
		t.Error("waitFn called: a mid-stream death must not trigger a retry")
		return true
	}}
	rec := httptest.NewRecorder()

	_, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}, "stream": true})
	if err == nil {
		t.Fatal("chatCompletion: want an error for a connection dropped mid-stream, got nil")
	}
	if !errors.Is(err, errUpstream) {
		t.Errorf("chatCompletion err = %v, want it to wrap errUpstream", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (mid-stream death is not retried)", got)
	}
	if !strings.Contains(rec.Body.String(), "Hel") {
		t.Errorf("rec.Body = %q, want it to contain the chunk already forwarded before the drop", rec.Body.String())
	}
}

// TestRetryPolicy_Do_AttemptRecorder_FiresOncePerAttempt proves
// attempt-accounting (Feature A, v0.22 — see do's own doc comment and
// limits.go's recordProviderAttempt): a request retried twice before
// succeeding reports THREE attempts to ctx's attemptRecorder, not one —
// each with that specific attempt's own resp, in call order.
func TestRetryPolicy_Do_AttemptRecorder_FiresOncePerAttempt(t *testing.T) {
	var calls int32
	p := &retryPolicy{enabled: true, attempts: 2, backoff: time.Millisecond, waitFn: spyWait(&[]time.Duration{})}
	call := callSequence(&calls,
		func() (*http.Response, error) { return fakeResp(http.StatusInternalServerError, nil), nil },
		func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil },
		func() (*http.Response, error) { return fakeResp(http.StatusOK, nil), nil },
	)

	var recorded []int
	ctx := withAttemptRecorder(context.Background(), func(resp *http.Response, err error) {
		if err != nil {
			t.Fatalf("recorder got unexpected err: %v", err)
		}
		recorded = append(recorded, resp.StatusCode)
	})

	resp, err := p.do(ctx, call)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("resp.StatusCode = %d, want 200", resp.StatusCode)
	}

	want := []int{http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusOK}
	if len(recorded) != len(want) {
		t.Fatalf("recorded = %v, want %v", recorded, want)
	}
	for i, w := range want {
		if recorded[i] != w {
			t.Errorf("recorded[%d] = %d, want %d", i, recorded[i], w)
		}
	}
}

// TestRetryPolicy_Do_NoAttemptRecorderInContext_NoPanic proves the common
// case — a ctx that never called withAttemptRecorder, e.g. registry.go's
// discovery listModels calls or the MCP/A2A target proxy — is a silent
// no-op, not a nil-func-call panic.
func TestRetryPolicy_Do_NoAttemptRecorderInContext_NoPanic(t *testing.T) {
	p := &retryPolicy{enabled: true, attempts: 1, backoff: time.Millisecond, waitFn: spyWait(&[]time.Duration{})}
	call := func() (*http.Response, error) { return fakeResp(http.StatusOK, nil), nil }

	resp, err := p.do(context.Background(), call) // must not panic
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("resp.StatusCode = %d, want 200", resp.StatusCode)
	}
}
