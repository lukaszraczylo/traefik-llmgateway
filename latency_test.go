package traefikllmgateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// delayedReadCloser is a minimal io.ReadCloser test double for
// watchdogBody's own latency unit tests below: its FIRST Read call
// sleeps firstReadDelay before delivering one byte; its SECOND sleeps
// afterFirstRead before returning io.EOF; every call after that is a
// no-op io.EOF. This lets a test control exactly how much wall-clock
// time falls BEFORE the first successful read (what TTFB must measure)
// versus AFTER it (what must count toward duration but never toward
// TTFB) without a real network connection.
type delayedReadCloser struct {
	firstReadDelay time.Duration
	afterFirstRead time.Duration
	read           int
}

func (d *delayedReadCloser) Read(p []byte) (int, error) {
	d.read++
	switch d.read {
	case 1:
		time.Sleep(d.firstReadDelay)
		p[0] = 'x'
		return 1, nil
	default:
		time.Sleep(d.afterFirstRead)
		return 0, io.EOF
	}
}

func (d *delayedReadCloser) Close() error { return nil }

// TestChatCompletion_NonStreaming_SmallBufferedBody_HasTTFB proves TTFB
// is still captured when the underlying reader combines its final data
// with io.EOF in ONE Read call — io.Reader's own contract explicitly
// permits this ("an instance ... may return either err == EOF or err ==
// nil"), and Go's real net/http body reader does exactly this for a
// small, fully-buffered response: measured directly against this test's
// own httptest server, a ~200-byte JSON completion arrives as a single
// Read call reporting (n=200, err=io.EOF) together, not as a data call
// followed by a separate EOF call. This is the common case for a
// typical, fully-buffered non-streaming completion — most real traffic
// — so getting it wrong here means hasTTFB is false for most requests,
// not an edge case.
//
// MUTATION VERIFIED: moving the TTFB-capture block (timeout.go's Read)
// back inside the `if err != nil { ... return ... }` branch's implicit
// else (i.e., gating it on err == nil, matching the pre-fix shape) made
// this test fail — samples[0].hasTTFB read false, since this test's own
// server response arrives in one combined (n>0, io.EOF) call. Reverted
// before committing.
func TestChatCompletion_NonStreaming_SmallBufferedBody_HasTTFB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	a := newTimeoutTestAdapter(srv.URL, 5*time.Second)

	var mu sync.Mutex
	var samples []latencySample
	ctx := withLatencyRecorder(context.Background(), func(s latencySample) {
		mu.Lock()
		samples = append(samples, s)
		mu.Unlock()
	})

	rec := httptest.NewRecorder()
	if _, err := a.chatCompletion(ctx, rec, map[string]any{"model": "gpt-5", "messages": []any{}}); err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(samples) != 1 {
		t.Fatalf("latencyRecorder invoked %d times, want 1: %+v", len(samples), samples)
	}
	if samples[0].streaming {
		t.Error("streaming = true, want false (Content-Type: application/json)")
	}
	if !samples[0].hasTTFB {
		t.Fatal("hasTTFB = false, want true — a small, fully-buffered non-streaming response must still report a TTFB, even when its data arrives combined with io.EOF in one Read call")
	}
	if samples[0].ttfb <= 0 {
		t.Errorf("ttfb = %s, want > 0", samples[0].ttfb)
	}
	if samples[0].ttfb > samples[0].duration {
		t.Errorf("ttfb (%s) > duration (%s) — ttfb must never exceed the total duration it is a prefix of", samples[0].ttfb, samples[0].duration)
	}
}

// TestWatchdogBody_ArmLatency_TTFBAtFirstRead_NotConstructionOrClose
// proves the task brief's core timing requirement directly, at the
// watchdogBody level: TTFB is captured strictly at the moment of the
// FIRST successful (n>0) Read, never at armLatency's own construction-
// time "start" and never re-derived at Close. firstReadDelay elapses
// BEFORE the first Read returns; afterFirstRead elapses AFTER it,
// before Close. A correct capture reports ttfb in
// [firstReadDelay, firstReadDelay+afterFirstRead) and duration >=
// firstReadDelay+afterFirstRead.
//
// MUTATION VERIFIED: changing Close (timeout.go) to unconditionally set
// `sample.ttfb = time.Since(wb.latencyStart); sample.hasTTFB = true`
// (ignoring the Read-captured wb.ttfb/ttfbRecorded entirely) made this
// test fail: got.ttfb read ~180ms (firstReadDelay+afterFirstRead, i.e.
// Close time), failing the "< firstReadDelay+afterFirstRead" assertion
// below. Reverted before committing.
func TestWatchdogBody_ArmLatency_TTFBAtFirstRead_NotConstructionOrClose(t *testing.T) {
	const (
		firstReadDelay = 60 * time.Millisecond
		afterFirstRead = 120 * time.Millisecond
	)
	rc := &delayedReadCloser{firstReadDelay: firstReadDelay, afterFirstRead: afterFirstRead}
	_, cancel := context.WithCancel(context.Background())
	wb := newWatchdogBody(rc, cancel, 5*time.Second, "p1", nil)

	var mu sync.Mutex
	var got *latencySample
	wb.armLatency(time.Now(), false, func(s latencySample) {
		mu.Lock()
		got = &s
		mu.Unlock()
	})

	buf := make([]byte, 16)
	if _, err := wb.Read(buf); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if _, err := wb.Read(buf); err != io.EOF {
		t.Fatalf("second Read: err = %v, want io.EOF", err)
	}
	if err := wb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got == nil {
		t.Fatal("latencyRecorder never invoked")
	}
	if !got.hasTTFB {
		t.Fatal("hasTTFB = false, want true — one successful read did happen")
	}
	if got.ttfb < firstReadDelay {
		t.Errorf("ttfb = %s, want >= %s (the first read's own delay)", got.ttfb, firstReadDelay)
	}
	if got.ttfb >= firstReadDelay+afterFirstRead {
		t.Errorf("ttfb = %s, want < %s — ttfb must not include time spent waiting on the SECOND read, proving it was captured at the FIRST read, not re-derived at Close", got.ttfb, firstReadDelay+afterFirstRead)
	}
	if got.duration < firstReadDelay+afterFirstRead {
		t.Errorf("duration = %s, want >= %s (the whole body's lifetime, both reads)", got.duration, firstReadDelay+afterFirstRead)
	}
}

// TestWatchdogBody_ArmLatency_NeverArmed_NoSampleEmitted proves the
// "no measurable hot-path cost, nothing collected" contract for a
// watchdogBody that is never armed at all (the disabled-metrics path,
// end to end: upstreamBytes/proxyUpstream only call armLatency when a
// non-nil latencyRecorder was actually looked up from context) — Read
// and Close must both behave exactly as they did before this feature
// existed, and nothing resembling a latencySample must ever be built or
// reported.
//
// MUTATION VERIFIED: changing Close (timeout.go) to call wb.onLatency
// unconditionally (dropping the `wb.onLatency != nil` guard) made this
// test panic with a nil-function-call ("runtime error: invalid memory
// address or nil pointer dereference"), which go test reports as a
// failure — confirming the guard is what this test actually exercises.
// Reverted before committing.
func TestWatchdogBody_ArmLatency_NeverArmed_NoSampleEmitted(t *testing.T) {
	rc := &delayedReadCloser{firstReadDelay: time.Millisecond, afterFirstRead: time.Millisecond}
	_, cancel := context.WithCancel(context.Background())
	wb := newWatchdogBody(rc, cancel, 5*time.Second, "p1", nil)
	// armLatency deliberately never called.

	buf := make([]byte, 16)
	if _, err := wb.Read(buf); err != nil {
		t.Fatalf("first Read: %v", err)
	}
	if _, err := wb.Read(buf); err != io.EOF {
		t.Fatalf("second Read: err = %v, want io.EOF", err)
	}
	if err := wb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if wb.onLatency != nil {
		t.Error("wb.onLatency is non-nil despite armLatency never being called")
	}
}

// TestChatCompletion_Stream_LongSteadyStream_ReportsSmallTTFBLargeDuration
// is the end-to-end proof for the exact case this whole feature exists
// to distinguish (task brief, "The signal, and the trap"): a stream
// whose total wall-clock duration is large, but whose FIRST chunk
// arrives quickly and every later chunk keeps the connection making
// progress, must report a SMALL time-to-first-byte and a LARGE total
// duration — never a duration that collapses to roughly the same value
// as TTFB (which is what a non-streaming, fully-buffered response looks
// like instead).
//
// MUTATION VERIFIED: deleting the `if n > 0 && wb.onLatency != nil && ...
// { ... }` TTFB-capture block from watchdogBody.Read (timeout.go)
// entirely made this test fail: ttfbRecorded never flips, hasTTFB stays
// false on every sample, and the `!s.hasTTFB` assertion below fails
// immediately. Reverted before committing.
func TestChatCompletion_Stream_LongSteadyStream_ReportsSmallTTFBLargeDuration(t *testing.T) {
	const (
		chunkGap  = 40 * time.Millisecond
		numChunks = 5 // 5*40ms = 200ms total: a large duration by this test's own standard
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for i := 0; i < numChunks; i++ {
			time.Sleep(chunkGap)
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"tok%d\"}}]}\n\n", i)
			fl.Flush()
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	a := newTimeoutTestAdapter(srv.URL, 5*time.Second) // generous: the idle watchdog must never fire

	var mu sync.Mutex
	var samples []latencySample
	ctx := withLatencyRecorder(context.Background(), func(s latencySample) {
		mu.Lock()
		samples = append(samples, s)
		mu.Unlock()
	})

	rec := httptest.NewRecorder()
	if _, err := a.chatCompletion(ctx, rec, map[string]any{"model": "gpt-5", "messages": []any{}, "stream": true}); err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(samples) != 1 {
		t.Fatalf("latencyRecorder invoked %d times, want 1: %+v", len(samples), samples)
	}
	s := samples[0]
	if !s.streaming {
		t.Error("streaming = false, want true (Content-Type: text/event-stream)")
	}
	if !s.hasTTFB {
		t.Fatal("hasTTFB = false, want true — the stream's first chunk was read successfully")
	}
	if s.ttfb >= 2*chunkGap {
		t.Errorf("ttfb = %s, want well under %s (roughly one chunkGap, not the whole stream)", s.ttfb, 2*chunkGap)
	}
	wantMinDuration := time.Duration(numChunks) * chunkGap
	if s.duration < wantMinDuration {
		t.Errorf("duration = %s, want at least %s (the whole stream's wall-clock time)", s.duration, wantMinDuration)
	}
	if s.duration <= 2*s.ttfb {
		t.Errorf("duration (%s) is not meaningfully larger than ttfb (%s) — a long, steadily-emitting stream must report a small TTFB and a large duration, the whole point of this feature", s.duration, s.ttfb)
	}
}

// TestProxyUpstream_Latency_ArmedFromContext_StreamingDerivedFromContentType
// proves proxyUpstream (routes_passthrough.go, native passthrough's
// shared core) wires latency the same way upstreamBytes (providers.go)
// does: armed with whatever latencyRecorder its caller's request context
// carries, with streaming derived from the upstream response's own
// Content-Type — not from anything passthrough itself would have no way
// to know (it forwards an opaque body, never decoding a "stream"
// request field the way an adapter's chatCompletion does). The
// recorder is injected directly onto the request's context here (cfg.
// Metrics is left unset, so handlePassthrough's own metricsEnabled-
// gated wiring never runs and never overwrites it) — this test is about
// proxyUpstream's OWN arming/derivation logic, not about the
// metricsEnabled gate itself (covered separately).
//
// MUTATION VERIFIED: removing the `if latRec != nil { wb.armLatency(...)
// }` block from proxyUpstream (routes_passthrough.go) made this test
// fail — the spy recorder was never invoked, so `len(samples) != 1`
// failed with 0. Reverted before committing.
func TestProxyUpstream_Latency_ArmedFromContext_StreamingDerivedFromContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"hello\":\"world\"}\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	gw.limiter.spawn = func(f func()) { f() }

	var mu sync.Mutex
	var samples []latencySample

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	req = req.WithContext(withAttemptRecorder(req.Context(), func(*http.Response, error) {}))
	req = req.WithContext(withLatencyRecorder(req.Context(), func(s latencySample) {
		mu.Lock()
		samples = append(samples, s)
		mu.Unlock()
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	mu.Lock()
	defer mu.Unlock()
	if len(samples) != 1 {
		t.Fatalf("latencyRecorder invoked %d times, want 1", len(samples))
	}
	if !samples[0].streaming {
		t.Error("streaming = false, want true (Content-Type: text/event-stream)")
	}
}
