package traefikllmgateway

import (
	"context"
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

// TestIsTransient covers the transience classification retryPolicy.do
// relies on: net errors (wrapping errUpstream) and HTTP 429/5xx are
// transient; a successful or non-429 4xx status, and a context
// cancellation/deadline error, are not.
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
// values.
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
}

// TestNewRetryPolicy_Defaults asserts Enabled=true with Attempts/Backoff
// left unset applies the spec defaults: 1 attempt, 250ms backoff.
func TestNewRetryPolicy_Defaults(t *testing.T) {
	p, err := newRetryPolicy(RetryConfig{Enabled: true})
	if err != nil {
		t.Fatalf("newRetryPolicy: %v", err)
	}
	if !p.enabled {
		t.Error("p.enabled = false, want true")
	}
	if p.attempts != 1 {
		t.Errorf("p.attempts = %d, want 1", p.attempts)
	}
	if p.backoff != 250*time.Millisecond {
		t.Errorf("p.backoff = %v, want 250ms", p.backoff)
	}
}

// TestNewRetryPolicy_ValidatesAttempts covers the 1..3 attempts range.
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

// spySleep returns a sleepFn that records every requested duration
// instead of actually sleeping, so retry-wait tests run instantly and can
// assert on the exact durations retryPolicy.do computed.
func spySleep(waits *[]time.Duration) func(time.Duration) {
	return func(d time.Duration) {
		*waits = append(*waits, d)
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

// TestRetryPolicy_Do_RetriesOnTransientThenSucceeds covers the brief's
// three canonical transient-then-success cases: 503, a raw network error
// (connection reset), and 429 with a qualifying Retry-After header. Each
// retries exactly once, waits the expected duration, and returns the
// second attempt's success.
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
			name: "429 with Retry-After then success",
			first: func() (*http.Response, error) {
				h := http.Header{}
				h.Set("Retry-After", "1")
				return fakeResp(http.StatusTooManyRequests, h), nil
			},
			wantWaits: []time.Duration{1 * time.Second},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int32
			var waits []time.Duration
			p := &retryPolicy{enabled: true, attempts: 2, backoff: 100 * time.Millisecond, sleepFn: spySleep(&waits)}
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
			if calls != 2 {
				t.Errorf("calls = %d, want 2 (one retry)", calls)
			}
			if len(waits) != len(tt.wantWaits) || (len(waits) > 0 && waits[0] != tt.wantWaits[0]) {
				t.Errorf("waits = %v, want %v", waits, tt.wantWaits)
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
	p := &retryPolicy{enabled: true, attempts: 3, backoff: time.Millisecond, sleepFn: spySleep(&waits)}
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusBadRequest, nil), nil })

	resp, err := p.do(context.Background(), call)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("resp.StatusCode = %d, want 400", resp.StatusCode)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on non-transient status)", calls)
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
	p := &retryPolicy{enabled: false, sleepFn: func(time.Duration) { t.Error("sleepFn called on a disabled policy") }}
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil })

	resp, err := p.do(context.Background(), call)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("resp.StatusCode = %d, want 503", resp.StatusCode)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (retry disabled)", calls)
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
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (nil policy = disabled)", calls)
	}
}

// TestRetryPolicy_Do_AttemptsCap asserts a policy that never sees success
// stops after exactly p.attempts tries, waiting p.attempts-1 times with
// exponential backoff, capped at maxRetryWait.
func TestRetryPolicy_Do_AttemptsCap(t *testing.T) {
	var calls int32
	var waits []time.Duration
	p := &retryPolicy{enabled: true, attempts: 3, backoff: time.Second, sleepFn: spySleep(&waits)}
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil })

	resp, err := p.do(context.Background(), call)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (attempts cap)", calls)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("resp.StatusCode = %d, want 503 (last attempt's result)", resp.StatusCode)
	}
	wantWaits := []time.Duration{maxRetryWait, maxRetryWait} // 1s*2^0=1s→wait capped? no: 1s, 2s both <= cap(2s)
	_ = wantWaits
	if len(waits) != 2 {
		t.Fatalf("waits = %v, want 2 entries (between 3 attempts)", waits)
	}
	if waits[0] != time.Second {
		t.Errorf("waits[0] = %v, want 1s (backoff * 2^0)", waits[0])
	}
	if waits[1] != 2*time.Second {
		t.Errorf("waits[1] = %v, want 2s (backoff * 2^1, at the cap)", waits[1])
	}
}

// TestRetryPolicy_Do_BackoffCappedAtMaxWait asserts a backoff whose
// exponential growth would exceed maxRetryWait is clamped to it.
func TestRetryPolicy_Do_BackoffCappedAtMaxWait(t *testing.T) {
	var calls int32
	var waits []time.Duration
	p := &retryPolicy{enabled: true, attempts: 3, backoff: 2 * time.Second, sleepFn: spySleep(&waits)}
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil })

	if _, err := p.do(context.Background(), call); err != nil {
		t.Fatalf("do: %v", err)
	}
	for i, w := range waits {
		if w > maxRetryWait {
			t.Errorf("waits[%d] = %v, exceeds cap %v", i, w, maxRetryWait)
		}
	}
}

// TestRetryPolicy_Do_CtxCanceledAbortsRetry asserts a context canceled
// before a retry wait would begin stops the loop immediately: no further
// call, no sleep, and the already-failed result is returned as-is.
func TestRetryPolicy_Do_CtxCanceledAbortsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before do() ever runs

	var calls int32
	p := &retryPolicy{enabled: true, attempts: 3, backoff: time.Millisecond, sleepFn: func(time.Duration) {
		t.Error("sleepFn called after ctx was already canceled")
	}}
	call := callSequence(&calls, func() (*http.Response, error) { return fakeResp(http.StatusServiceUnavailable, nil), nil })

	resp, err := p.do(ctx, call)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (ctx already done, no retry)", calls)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("resp.StatusCode = %d, want 503", resp.StatusCode)
	}
}

// TestRetryPolicy_Do_ClosesFailedAttemptBody asserts a retried-away
// response's body is drained and closed before the next attempt, so the
// underlying connection is eligible for reuse instead of being forced
// closed by an un-drained body.
func TestRetryPolicy_Do_ClosesFailedAttemptBody(t *testing.T) {
	var calls int32
	var waits []time.Duration
	closed := false
	firstBody := &trackingCloser{Reader: strings.NewReader("stale body"), onClose: func() { closed = true }}
	p := &retryPolicy{enabled: true, attempts: 2, backoff: time.Millisecond, sleepFn: spySleep(&waits)}
	call := callSequence(&calls,
		func() (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: firstBody}, nil
		},
		func() (*http.Response, error) { return fakeResp(http.StatusOK, nil), nil },
	)

	if _, err := p.do(context.Background(), call); err != nil {
		t.Fatalf("do: %v", err)
	}
	if !closed {
		t.Error("failed attempt's response body was never closed")
	}
}

// trackingCloser wraps an io.Reader with a Close method that records the
// call, for asserting a retried-away response body was actually closed.
type trackingCloser struct {
	io.Reader
	onClose func()
}

func (c *trackingCloser) Close() error {
	c.onClose()
	return nil
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
	a.retry = &retryPolicy{enabled: true, attempts: 2, backoff: time.Millisecond, sleepFn: func(time.Duration) {}}
	rec := httptest.NewRecorder()

	u, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}})
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}
	if calls != 2 {
		t.Errorf("upstream calls = %d, want 2", calls)
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
	a.retry = &retryPolicy{enabled: true, attempts: 2, backoff: time.Millisecond, sleepFn: func(time.Duration) {}}
	rec := httptest.NewRecorder()

	u, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}, "stream": true})
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}
	if calls != 2 {
		t.Errorf("upstream calls = %d, want 2", calls)
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

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter does not support Hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("Hijack: %v", err)
		}
		_ = conn.Close() // abrupt drop mid-chunked-body: client sees a read error, not a clean EOF
	}))
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	a.retry = &retryPolicy{enabled: true, attempts: 3, backoff: time.Millisecond, sleepFn: func(time.Duration) {
		t.Error("sleepFn called: a mid-stream death must not trigger a retry")
	}}
	rec := httptest.NewRecorder()

	_, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}, "stream": true})
	if err == nil {
		t.Fatal("chatCompletion: want an error for a connection dropped mid-stream, got nil")
	}
	if !errors.Is(err, errUpstream) {
		t.Errorf("chatCompletion err = %v, want it to wrap errUpstream", err)
	}
	if calls != 1 {
		t.Errorf("upstream calls = %d, want 1 (mid-stream death is not retried)", calls)
	}
	if !strings.Contains(rec.Body.String(), "Hel") {
		t.Errorf("rec.Body = %q, want it to contain the chunk already forwarded before the drop", rec.Body.String())
	}
}
