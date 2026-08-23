package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTimeoutTestAdapter returns an *openaiAdapter pointed at baseURL with
// its request timeout resolved to timeout — mirroring exactly what
// buildAdapters (providers.go) does after newOpenAIAdapter's own bare
// construction: set .timeout, then rebuild .client via
// newAdapterHTTPClient(timeout) so Transport.ResponseHeaderTimeout
// reflects it too.
func newTimeoutTestAdapter(baseURL string, timeout time.Duration) *openaiAdapter {
	a := newOpenAIAdapter("p1", baseURL, "sk-test")
	a.timeout = timeout
	a.client = newAdapterHTTPClient(timeout)
	return a
}

// newStallingServer returns an httptest.Server whose single handler runs
// prewrite (nil is a no-op — writes nothing) and then blocks until
// release is called.
//
// Deliberately NOT `<-r.Context().Done()`: verified empirically that,
// in this environment, neither a client-side Transport.
// ResponseHeaderTimeout nor a client-side context.CancelFunc reliably
// unblocks a server handler parked on its own r.Context().Done() within
// a bounded time — httptest.Server.Close hung past 60s waiting for the
// connection in both cases. That is a property of net/http's server-side
// close detection under httptest, not of this feature's client-side
// behavior (which every test below still verifies independently, via the
// CLIENT-side call returning promptly). release decouples test cleanup
// from that detection entirely: the handler exits the instant the test
// explicitly says so, so Close never has anything left to wait for.
func newStallingServer(prewrite func(http.ResponseWriter)) (srv *httptest.Server, release func()) {
	done := make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if prewrite != nil {
			prewrite(w)
		}
		<-done
	}))
	return srv, func() {
		close(done)
		srv.Close()
	}
}

// TestChatCompletion_StreamKeepsEmittingPastTimeout_CompletesIntact is the
// single highest-priority test in this feature (written first, per the
// task brief): a streaming response whose TOTAL duration exceeds the
// configured timeout, but whose every INTER-CHUNK gap stays well under
// it, must complete intact — this is the exact behavior a total-duration
// cap (explicitly rejected by the operator) would break, and the whole
// reason the watchdog resets on every successful read instead of firing
// once at a fixed deadline.
//
// MUTATION VERIFIED: commenting out watchdogBody.Read's `wb.timer.Reset(
// wb.timeout)` (timeout.go) turns the idle watchdog into a de facto
// total-duration cap — the timer set at construction fires once, at
// timeout after the first byte, regardless of intervening progress. Under
// that mutation this test failed with "chatCompletion: ... provider
// request timed out" partway through the stream, confirming it kills the
// exact regression this test exists to catch. Reverted before committing.
func TestChatCompletion_StreamKeepsEmittingPastTimeout_CompletesIntact(t *testing.T) {
	const (
		timeout   = 80 * time.Millisecond
		chunkGap  = 30 * time.Millisecond
		numChunks = 6 // 6*30ms = 180ms total: well past timeout, but no single gap is
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

	a := newTimeoutTestAdapter(srv.URL, timeout)
	rec := httptest.NewRecorder()

	if _, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}, "stream": true}); err != nil {
		t.Fatalf("chatCompletion: %v (a stream whose total duration exceeds the timeout, but whose every gap stays under it, must complete — a total-duration cap was explicitly rejected)", err)
	}
	body := rec.Body.String()
	for i := 0; i < numChunks; i++ {
		want := fmt.Sprintf("tok%d", i)
		if !strings.Contains(body, want) {
			t.Errorf("rec.Body missing %q — stream was truncated: %s", want, body)
		}
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("rec.Body missing [DONE] terminator — stream did not complete: %s", body)
	}
}

// TestChatCompletion_NeverSendsHeaders_AbortedAtTimeout proves the
// Transport.ResponseHeaderTimeout mechanism (newAdapterHTTPClient,
// providers.go): a provider that accepts the connection and then writes
// nothing at all is aborted, not left hanging forever.
//
// MUTATION VERIFIED: removing `tr.ResponseHeaderTimeout = timeout` from
// newAdapterHTTPClient (providers.go) made this test hang past a 5s
// `go test -run ... -timeout 5s` bound (test process killed, reported
// FAIL) — confirming no other mechanism bounds a provider that never
// sends headers. Reverted before committing.
func TestChatCompletion_NeverSendsHeaders_AbortedAtTimeout(t *testing.T) {
	const timeout = 60 * time.Millisecond
	srv, release := newStallingServer(nil)
	defer release()

	a := newTimeoutTestAdapter(srv.URL, timeout)
	rec := httptest.NewRecorder()

	start := time.Now()
	_, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("chatCompletion: want an error for a provider that never sends headers, got nil")
	}
	if !errors.Is(err, errUpstream) {
		t.Errorf("err = %v, want it to wrap errUpstream", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("chatCompletion took %s to abort a %s ResponseHeaderTimeout", elapsed, timeout)
	}
}

// TestChatCompletion_Stream_StallsMidBody_AbortedAtTimeout proves the
// idle-progress watchdog (watchdogBody, timeout.go): a provider that
// sends headers and one chunk, then goes silent, is aborted — proving
// ResponseHeaderTimeout alone (already satisfied once headers arrived) is
// not enough, and something must also bound the body.
//
// MUTATION VERIFIED: deleting the `resp.Body = newWatchdogBody(...)` line
// in upstreamBytes (providers.go) made this test hang past a 5s
// `-timeout 5s` bound (same failure mode as the ResponseHeaderTimeout
// test above, confirming this is a genuinely separate mechanism).
// Reverted before committing.
func TestChatCompletion_Stream_StallsMidBody_AbortedAtTimeout(t *testing.T) {
	const timeout = 60 * time.Millisecond
	srv, release := newStallingServer(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fl.Flush()
	})
	defer release()

	a := newTimeoutTestAdapter(srv.URL, timeout)
	rec := httptest.NewRecorder()

	start := time.Now()
	_, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}, "stream": true})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("chatCompletion: want an error for a stream that stalls mid-body, got nil")
	}
	if !errors.Is(err, errProviderTimeout) {
		t.Errorf("err = %v, want errors.Is(err, errProviderTimeout)", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("chatCompletion took %s to abort a %s idle timeout", elapsed, timeout)
	}
}

// TestChatCompletion_NonStream_StallsMidBody_AbortedAtTimeout is the
// non-streaming counterpart: "Non-streaming: at most timeout waiting for
// the response" (the task brief) is satisfied by the SAME watchdogBody
// mechanism as streaming — forwardJSON's io.ReadAll blocks on Read just
// like forwardStream's readSSE does, so one mechanism covers both
// documented behaviors without a second, total-duration-shaped one.
func TestChatCompletion_NonStream_StallsMidBody_AbortedAtTimeout(t *testing.T) {
	const timeout = 60 * time.Millisecond
	srv, release := newStallingServer(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = fmt.Fprint(w, `{"id":"c1",`) // deliberately incomplete JSON: a real close never arrives
		fl.Flush()
	})
	defer release()

	a := newTimeoutTestAdapter(srv.URL, timeout)
	rec := httptest.NewRecorder()

	start := time.Now()
	_, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("chatCompletion: want an error for a non-streaming body that stalls mid-write, got nil")
	}
	if !errors.Is(err, errProviderTimeout) {
		t.Errorf("err = %v, want errors.Is(err, errProviderTimeout)", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("chatCompletion took %s to abort a %s idle timeout", elapsed, timeout)
	}
}

// TestChatCompletion_Timeout_ClassifiedAsProviderFailure_DistinctFromClientDisconnect
// proves the non-negotiable classification requirement: a watchdog-fired
// timeout must wrap errProviderTimeout and name the provider, and must
// never be confusable with a genuine client disconnect (context.Canceled)
// — and vice versa. handleAdapterErrorEnvelope (routes_unified.go) relies
// on exactly this mutual exclusivity to decide whether to log-only (a
// client that walked away) or report a provider failure with a 502.
//
// MUTATION VERIFIED: changing watchdogBody.Read to return the raw
// underlying error unchanged (dropping the errProviderTimeout wrap
// entirely) made the first subtest fail its errors.Is(err,
// errProviderTimeout) assertion, confirming the test actually checks the
// wrap rather than merely "an error occurred". Reverted before
// committing.
func TestChatCompletion_Timeout_ClassifiedAsProviderFailure_DistinctFromClientDisconnect(t *testing.T) {
	t.Run("watchdog timeout wraps errProviderTimeout, names the provider, never context.Canceled", func(t *testing.T) {
		const timeout = 60 * time.Millisecond
		srv, release := newStallingServer(func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
		})
		defer release()

		a := newTimeoutTestAdapter(srv.URL, timeout)
		rec := httptest.NewRecorder()
		_, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}, "stream": true})
		if err == nil {
			t.Fatal("want an error")
		}
		if !errors.Is(err, errProviderTimeout) {
			t.Errorf("err = %v, want errors.Is(err, errProviderTimeout)", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, must NOT satisfy errors.Is(err, context.Canceled) — a watchdog firing is a provider failure, not a client disconnect", err)
		}
		if !strings.Contains(err.Error(), `"p1"`) {
			t.Errorf("err = %v, want it to name the provider (%q)", err, "p1")
		}
	})

	t.Run("client cancellation before headers wraps context.Canceled, never errProviderTimeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(200 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		a := newTimeoutTestAdapter(srv.URL, 5*time.Second) // generous: must never fire
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		rec := httptest.NewRecorder()
		_, err := a.chatCompletion(ctx, rec, map[string]any{"model": "gpt-5", "messages": []any{}})
		if err == nil {
			t.Fatal("want an error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
		}
		if errors.Is(err, errProviderTimeout) {
			t.Errorf("err = %v, must NOT satisfy errors.Is(err, errProviderTimeout) — this is a client disconnect, not a provider failure", err)
		}
	})

	t.Run("client cancellation mid-stream (after headers) wraps context.Canceled, never errProviderTimeout", func(t *testing.T) {
		srv, release := newStallingServer(func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			fl.Flush()
		})
		defer release()

		a := newTimeoutTestAdapter(srv.URL, 5*time.Second) // generous: must never fire
		ctx, cancel := context.WithCancel(context.Background())
		rec := httptest.NewRecorder()
		go func() {
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		_, err := a.chatCompletion(ctx, rec, map[string]any{"model": "gpt-5", "messages": []any{}, "stream": true})
		if err == nil {
			t.Fatal("want an error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
		}
		if errors.Is(err, errProviderTimeout) {
			t.Errorf("err = %v, must NOT satisfy errors.Is(err, errProviderTimeout) — watchdogBody must pass a real client cancel through unchanged", err)
		}
	})
}

// TestResolveRequestTimeout is table-driven over resolveRequestTimeout's
// full contract: empty means the default; a valid duration parses; a
// malformed string, a zero, and a negative duration are all construction
// errors — the zero/negative case never means "no timeout" (that is the
// bug this feature fixes).
//
// MUTATION VERIFIED: removing the `if d <= 0 { return error }` branch
// (timeout.go) made the "zero" and "negative" cases return (0, nil)
// instead of an error, failing this test's assertions for both. Reverted
// before committing.
func TestResolveRequestTimeout(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{name: "empty means default", raw: "", want: defaultRequestTimeout},
		{name: "valid minutes", raw: "5m", want: 5 * time.Minute},
		{name: "valid seconds", raw: "90s", want: 90 * time.Second},
		{name: "malformed string is an error", raw: "not-a-duration", wantErr: true},
		{name: "zero is an error, not unlimited", raw: "0s", wantErr: true},
		{name: "negative is an error", raw: "-5s", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveRequestTimeout(c.raw, "requestTimeout")
			if c.wantErr {
				if err == nil {
					t.Fatalf("resolveRequestTimeout(%q) = (%v, nil), want an error", c.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRequestTimeout(%q): %v", c.raw, err)
			}
			if got != c.want {
				t.Errorf("resolveRequestTimeout(%q) = %v, want %v", c.raw, got, c.want)
			}
		})
	}
}

// TestResolveProviderTimeout_OverrideBeatsGlobal proves the house rule
// ("explicit override always wins") for this feature specifically: a
// non-empty per-provider raw wins regardless of what globalTimeout is,
// and an empty one inherits globalTimeout exactly.
func TestResolveProviderTimeout_OverrideBeatsGlobal(t *testing.T) {
	const global = 2 * time.Minute

	got, err := resolveProviderTimeout("p1", "30s", global)
	if err != nil {
		t.Fatalf("resolveProviderTimeout: %v", err)
	}
	if got != 30*time.Second {
		t.Errorf("provider override: got %v, want 30s (must win over the 2m global)", got)
	}

	got, err = resolveProviderTimeout("p1", "", global)
	if err != nil {
		t.Fatalf("resolveProviderTimeout: %v", err)
	}
	if got != global {
		t.Errorf("empty provider override: got %v, want %v (inherit the global)", got, global)
	}

	if _, err := resolveProviderTimeout("p1", "0s", global); err == nil {
		t.Error("resolveProviderTimeout(\"p1\", \"0s\", ...): want an error, got nil")
	}
}

// TestBuildAdapters_RequestTimeout covers the full config-resolution
// matrix end to end through buildAdapters, matching the sibling
// TestBuildAdapters_WiresRetryPolicyFromConfig's own style (retry_test.go):
// unset everywhere resolves to five minutes; a global default applies
// when a provider sets no override; an explicit provider override wins
// over the global; and a malformed value at either level fails
// construction — including a malformed GLOBAL value that happens to be
// shadowed by every provider's own valid override, proving the global is
// still validated unconditionally rather than lying dormant.
func TestBuildAdapters_RequestTimeout(t *testing.T) {
	t.Run("unset everywhere yields the five-minute default", func(t *testing.T) {
		cfg := CreateConfig()
		cfg.Providers = map[string]*ProviderConfig{"p1": {Type: "openai", APIKey: "k"}}
		adapters, err := buildAdapters(cfg)
		if err != nil {
			t.Fatalf("buildAdapters: %v", err)
		}
		oa := adapters["p1"].(*openaiAdapter)
		if oa.timeout != defaultRequestTimeout {
			t.Errorf("oa.timeout = %v, want %v", oa.timeout, defaultRequestTimeout)
		}
	})

	t.Run("global default applies when the provider sets no override", func(t *testing.T) {
		cfg := CreateConfig()
		cfg.RequestTimeout = "2m"
		cfg.Providers = map[string]*ProviderConfig{"p1": {Type: "openai", APIKey: "k"}}
		adapters, err := buildAdapters(cfg)
		if err != nil {
			t.Fatalf("buildAdapters: %v", err)
		}
		oa := adapters["p1"].(*openaiAdapter)
		if oa.timeout != 2*time.Minute {
			t.Errorf("oa.timeout = %v, want 2m", oa.timeout)
		}
	})

	t.Run("provider override wins over the global", func(t *testing.T) {
		cfg := CreateConfig()
		cfg.RequestTimeout = "2m"
		cfg.Providers = map[string]*ProviderConfig{"p1": {Type: "openai", APIKey: "k", RequestTimeout: "30s"}}
		adapters, err := buildAdapters(cfg)
		if err != nil {
			t.Fatalf("buildAdapters: %v", err)
		}
		oa := adapters["p1"].(*openaiAdapter)
		if oa.timeout != 30*time.Second {
			t.Errorf("oa.timeout = %v, want 30s (override must win over the 2m global)", oa.timeout)
		}
	})

	t.Run("malformed global fails construction even when every provider overrides it", func(t *testing.T) {
		cfg := CreateConfig()
		cfg.RequestTimeout = "not-a-duration"
		cfg.Providers = map[string]*ProviderConfig{"p1": {Type: "openai", APIKey: "k", RequestTimeout: "30s"}}
		if _, err := buildAdapters(cfg); err == nil {
			t.Fatal("buildAdapters: want an error for a malformed global requestTimeout, got nil (it must be validated even when shadowed)")
		}
	})

	t.Run("zero global is a construction error", func(t *testing.T) {
		cfg := CreateConfig()
		cfg.RequestTimeout = "0s"
		cfg.Providers = map[string]*ProviderConfig{"p1": {Type: "openai", APIKey: "k"}}
		if _, err := buildAdapters(cfg); err == nil {
			t.Fatal("buildAdapters: want an error for requestTimeout: \"0s\", got nil")
		}
	})

	t.Run("negative provider override is a construction error", func(t *testing.T) {
		cfg := CreateConfig()
		cfg.Providers = map[string]*ProviderConfig{"p1": {Type: "openai", APIKey: "k", RequestTimeout: "-1s"}}
		if _, err := buildAdapters(cfg); err == nil {
			t.Fatal("buildAdapters: want an error for a provider requestTimeout of \"-1s\", got nil")
		}
	})
}

// TestNewAdapterHTTPClient_SetsResponseHeaderTimeout proves
// newAdapterHTTPClient wires its timeout argument onto the returned
// client's Transport.ResponseHeaderTimeout — the mechanism
// TestChatCompletion_NeverSendsHeaders_AbortedAtTimeout exercises
// end to end; this is the narrower, direct check.
func TestNewAdapterHTTPClient_SetsResponseHeaderTimeout(t *testing.T) {
	const want = 42 * time.Second
	c := newAdapterHTTPClient(want)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client.Transport is %T, want *http.Transport", c.Transport)
	}
	if tr.ResponseHeaderTimeout != want {
		t.Errorf("Transport.ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, want)
	}
	if c.Timeout != 0 {
		t.Errorf("Client.Timeout = %v, want 0 (unset) — it would bound the whole request including body read, truncating a healthy long stream", c.Timeout)
	}
}

// fakeReadCloser is a minimal io.ReadCloser test double for watchdogBody's
// own unit tests below: Read always blocks until unblock is closed (or
// forever if it never is), simulating a stalled upstream body without a
// real network connection.
type fakeReadCloser struct {
	unblock chan struct{}
	closed  chan struct{}
}

func newFakeReadCloser() *fakeReadCloser {
	return &fakeReadCloser{unblock: make(chan struct{}), closed: make(chan struct{})}
}

func (f *fakeReadCloser) Read(p []byte) (int, error) {
	<-f.unblock
	return 0, io.EOF
}

func (f *fakeReadCloser) Close() error {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	return nil
}

// TestWatchdogBody_Close_StopsTimer_FireNeverRuns proves Close's "no timer
// or goroutine leak" contract directly and deterministically (not via
// runtime.NumGoroutine, which only observes a timer once it has already
// fired): closing a watchdogBody before its timeout elapses must stop the
// timer outright, so fire (and therefore cancel) never runs even once
// timeout has since passed.
//
// MUTATION VERIFIED: removing `wb.timer.Stop()` from watchdogBody.Close
// (timeout.go) made this test fail — timedOut read back as 1 after the
// sleep, since the un-stopped timer still fired on schedule. Reverted
// before committing.
//
// wb.timedOut is read via atomic.LoadInt32 below, not a bare field read
// (coordinator adversarial review, 2026-08-23, finding F9): every other
// access in timeout.go itself is atomic, so a bare read here was a
// latent -race report waiting on a scheduling difference, even though
// Close's own Stop happens-before makes this specific read safe in
// practice.
func TestWatchdogBody_Close_StopsTimer_FireNeverRuns(t *testing.T) {
	const timeout = 30 * time.Millisecond
	frc := newFakeReadCloser()
	ctx, cancel := context.WithCancel(context.Background())
	wb := newWatchdogBody(frc, cancel, timeout, "p1", nil)

	if err := wb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(3 * timeout) // well past when an un-stopped timer would fire

	if got := atomic.LoadInt32(&wb.timedOut); got != 0 {
		t.Errorf("wb.timedOut = %d after Close, want 0 — the timer must not fire once stopped", got)
	}
	select {
	case <-ctx.Done():
		// Close itself calls cancel unconditionally (its own doc comment),
		// so ctx.Done() firing here is expected and does not indicate fire
		// ran — only wb.timedOut distinguishes that.
	default:
		t.Error("ctx.Done() not closed after wb.Close() — Close must call cancel unconditionally")
	}
}

// TestWatchdogBody_NoBlockingGoroutineLeak_AcrossManyRequests drives many
// successful (fast) and many timed-out requests through a real adapter
// and asserts the live goroutine count returns to its baseline afterward
// — proving neither path leaves a goroutine permanently blocked. The
// task's own concern ("do not leak a goroutine or a timer per request")
// is specifically about a goroutine that never exits (the classic
// `go func(){ select{ case <-time.After(d): ...; case <-done: } }()`
// leak, when done is never closed on the success path); watchdogBody's
// actual design (time.AfterFunc + Stop) never spawns a goroutine at all
// unless the watchdog fires, and that goroutine runs fire() to
// completion and exits immediately either way.
//
// MUTATION VERIFIED: adding `go func() { <-make(chan struct{}) }()` (a
// permanently blocked goroutine) to the top of watchdogBody.fire
// (timeout.go) made this test fail — the goroutine count after the
// timed-out-request loop stayed elevated by roughly the loop's iteration
// count and never settled within the poll window, confirming the test
// catches a real per-request leak. Reverted before committing.
func TestWatchdogBody_NoBlockingGoroutineLeak_AcrossManyRequests(t *testing.T) {
	const (
		iterations  = 40
		fastTimeout = 2 * time.Second // generous: must never fire for the success path
		hangTimeout = 20 * time.Millisecond
	)

	fastSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer fastSrv.Close()

	// prewrite sends headers before stalling: a "never sends headers" hang
	// is caught by Transport.ResponseHeaderTimeout, in a *http.Client.Do
	// error, and never even constructs a watchdogBody — this test is
	// specifically about the WATCHDOG's own goroutine, so its hang must
	// stall AFTER headers, the same shape as the mid-body-stall tests
	// above, to actually invoke fire() on every one of the iterations
	// below.
	hangSrv, release := newStallingServer(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
	})

	runtime.GC()
	baseline := runtime.NumGoroutine()

	fast := newTimeoutTestAdapter(fastSrv.URL, fastTimeout)
	for i := 0; i < iterations; i++ {
		rec := httptest.NewRecorder()
		if _, err := fast.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}}); err != nil {
			t.Fatalf("fast request %d: %v", i, err)
		}
	}

	hang := newTimeoutTestAdapter(hangSrv.URL, hangTimeout)
	for i := 0; i < iterations; i++ {
		rec := httptest.NewRecorder()
		if _, err := hang.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "messages": []any{}}); err == nil {
			t.Fatalf("hung request %d: want a timeout error, got nil", i)
		}
	}
	// release BEFORE measuring: each of the iterations above left its own
	// server-side handler goroutine parked on newStallingServer's <-done
	// (this test's own fixture design, not this feature's watchdog code)
	// — releasing them here, before the goroutine count below, keeps the
	// measurement about the watchdog's own goroutines settling, not about
	// this test's still-blocked handlers swamping the signal.
	release()

	deadline := time.Now().Add(3 * time.Second)
	var final int
	const slack = 5 // unrelated Go runtime/http.Transport bookkeeping goroutines, not this feature's concern
	for {
		runtime.GC()
		final = runtime.NumGoroutine()
		if final <= baseline+slack || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if final > baseline+slack {
		t.Errorf("goroutines after %d fast + %d timed-out requests = %d, baseline = %d (slack %d) — possible leak", iterations, iterations, final, baseline, slack)
	}
}

// TestHandlePassthrough_StalledUpstream_AbortedAtTimeout proves the
// passthrough route (routes_passthrough.go's proxyUpstream) shares this
// feature's idle-progress watchdog, not just each provider adapter's own
// upstreamBytes call path — the task brief's explicit "also cover the
// passthrough route" requirement. adapter.httpClient() and
// adapter.requestTimeout() are the SAME values chatCompletion uses, so a
// provider's requestTimeout config applies identically through native
// passthrough.
func TestHandlePassthrough_StalledUpstream_AbortedAtTimeout(t *testing.T) {
	srv, release := newStallingServer(nil)
	defer release()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up", RequestTimeout: "60ms"},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	gw.limiter.spawn = func(f func()) { f() }

	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()

	start := time.Now()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}
	if elapsed > 3*time.Second {
		t.Errorf("passthrough took %s to abort a 60ms provider requestTimeout", elapsed)
	}
}

// TestNewGateway_TargetTimeout_ResolvedFromConfig proves the MCP/A2A
// target proxy's own client (g.targetClient, llmgateway.go) picks up
// Config.RequestTimeout the same way a provider adapter does — g.
// targetTimeout is what proxyUpstream's handleTargetProxy call site
// (mcp_a2a.go) passes as the idle-watchdog bound, and targetClient's own
// Transport.ResponseHeaderTimeout must match it too.
func TestNewGateway_TargetTimeout_ResolvedFromConfig(t *testing.T) {
	cfg := CreateConfig()
	cfg.RequestTimeout = "90s"
	cfg.Providers = map[string]*ProviderConfig{"p1": {Type: "openai", APIKey: "k"}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	if gw.targetTimeout != 90*time.Second {
		t.Errorf("gw.targetTimeout = %v, want 90s", gw.targetTimeout)
	}
	tr, ok := gw.targetClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("targetClient.Transport is %T, want *http.Transport", gw.targetClient.Transport)
	}
	if tr.ResponseHeaderTimeout != 90*time.Second {
		t.Errorf("targetClient Transport.ResponseHeaderTimeout = %v, want 90s", tr.ResponseHeaderTimeout)
	}
}

// TestChatCompletion_Stream_StallsMidBody_RecordsProviderFailure proves
// finding F4 (coordinator adversarial review, 2026-08-23): a mid-body
// watchdog timeout must reach provider-health accounting through the
// SAME attemptRecorder mechanism retryPolicy.do already uses at
// header-arrival time. Before this fix, limiter.recordProviderAttempt was
// only ever invoked once per request — at header time — so a provider
// that reliably stalls mid-body looked 100% successful to any consumer
// of those counters (the discovery breaker, cross-provider failover)
// forever: attempts=1 failures=0, identical to a real success.
//
// MUTATION VERIFIED: removing the `if wb.onTimeout != nil { ... }` block
// from watchdogBody.fire (timeout.go) made this test fail — the spy
// recorder was invoked exactly once (the header-time success), never a
// second time recording the failure. Reverted before committing.
func TestChatCompletion_Stream_StallsMidBody_RecordsProviderFailure(t *testing.T) {
	const timeout = 60 * time.Millisecond
	srv, release := newStallingServer(func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
	})
	defer release()

	a := newTimeoutTestAdapter(srv.URL, timeout)

	type recorded struct {
		resp *http.Response
		err  error
	}
	var mu sync.Mutex
	var calls []recorded
	rec := attemptRecorder(func(resp *http.Response, err error) {
		mu.Lock()
		calls = append(calls, recorded{resp, err})
		mu.Unlock()
	})
	ctx := withAttemptRecorder(context.Background(), rec)

	rw := httptest.NewRecorder()
	if _, err := a.chatCompletion(ctx, rw, map[string]any{"model": "gpt-5", "messages": []any{}, "stream": true}); err == nil {
		t.Fatal("chatCompletion: want an error for a stream that stalls mid-body, got nil")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("attemptRecorder invoked %d times, want 2 (header-time success, then watchdog-fire failure): %+v", len(calls), calls)
	}
	if calls[0].err != nil {
		t.Errorf("first call err = %v, want nil (the header-arrival attempt succeeded)", calls[0].err)
	}
	if calls[1].resp != nil {
		t.Errorf("second call resp = %v, want nil", calls[1].resp)
	}
	if !errors.Is(calls[1].err, errProviderTimeout) {
		t.Errorf("second call err = %v, want errors.Is(err, errProviderTimeout)", calls[1].err)
	}
}

// zeroByteReadCloser always returns (0, nil) — a legal io.Reader response
// that delivers no bytes without signaling an error or EOF — until
// Close, which switches it to (0, io.EOF). Used by
// TestWatchdogBody_ZeroByteRead_DoesNotResetTimer (finding F6) to prove a
// Read that "succeeds" but delivers nothing does not keep the watchdog
// alive.
type zeroByteReadCloser struct {
	closed chan struct{}
}

func newZeroByteReadCloser() *zeroByteReadCloser {
	return &zeroByteReadCloser{closed: make(chan struct{})}
}

func (z *zeroByteReadCloser) Read([]byte) (int, error) {
	select {
	case <-z.closed:
		return 0, io.EOF
	default:
		return 0, nil
	}
}

func (z *zeroByteReadCloser) Close() error {
	select {
	case <-z.closed:
	default:
		close(z.closed)
	}
	return nil
}

// TestWatchdogBody_ZeroByteRead_DoesNotResetTimer proves finding F6
// (coordinator adversarial review, 2026-08-23): watchdogBody.Read
// resetting on every err == nil read, regardless of n, let a reader that
// keeps returning (0, nil) hold the watchdog open forever — the
// coordinator's own probe measured this surviving past 20x the
// configured timeout across 1044 reads. Not reachable from net/http
// against a non-empty buffer today, but exactly the naive-watchdog hole
// this feature exists to close, so it is fixed and pinned regardless.
//
// This checks wb.timedOut directly rather than expecting wb.Read to
// surface an error: zeroByteReadCloser never inspects the watchdog's
// canceled context (unlike a real net.Conn, whose blocked Read a
// cancellation actually unblocks with an error), so it would return
// (0, nil) forever either way — the property under test is whether the
// watchdog's internal deadline advances, not what Read eventually
// returns.
//
// MUTATION VERIFIED: reverting watchdogBody.Read's `if n > 0 { ... }`
// guard (timeout.go) to an unconditional `wb.timer.Reset(wb.timeout)`
// made this test fail — timedOut stayed 0 for the full 10-timeout poll
// window, since every (0, nil) read kept re-arming the timer. Reverted
// before committing.
func TestWatchdogBody_ZeroByteRead_DoesNotResetTimer(t *testing.T) {
	const timeout = 20 * time.Millisecond
	zrc := newZeroByteReadCloser()
	defer zrc.Close()
	_, cancel := context.WithCancel(context.Background())
	wb := newWatchdogBody(zrc, cancel, timeout, "p1", nil)
	defer wb.Close()

	buf := make([]byte, 16)
	deadline := time.Now().Add(10 * timeout)
	for time.Now().Before(deadline) && atomic.LoadInt32(&wb.timedOut) == 0 {
		n, _ := wb.Read(buf) // always (0, nil) from this fixture, until Close
		if n != 0 {
			t.Fatalf("Read returned n=%d, want 0 (fixture bug)", n)
		}
		time.Sleep(time.Millisecond)
	}

	if got := atomic.LoadInt32(&wb.timedOut); got != 1 {
		t.Fatalf("wb.timedOut = %d after %s of continuous (0, nil) reads (timeout=%s), want 1 — a Read returning (0, nil) must not keep resetting the watchdog", got, 10*timeout, timeout)
	}
}

// TestWatchdogBody_TimeoutError_UsesCallerLabel_NotHardcodedProvider
// proves finding F9 (coordinator adversarial review, 2026-08-23):
// proxyUpstream (routes_passthrough.go) previously passed logPrefix as a
// bare "providerName" argument that watchdogBody wrapped in a hardcoded
// `provider %q` — producing a timeout error reading `provider
// "mcp target (name foo)"` for an MCP/A2A target proxy request, which is
// not a provider at all. watchdogBody now takes an already-formatted
// label from the caller and uses it verbatim.
//
// MUTATION VERIFIED: changing watchdogBody.timeoutError's format string
// back to `"%w: provider %q: no upstream progress for %s"` (timeout.go)
// made this test fail both assertions — the caller's label no longer
// appeared verbatim, and the hardcoded "provider \"mcp target" prefix
// reappeared. Reverted before committing.
func TestWatchdogBody_TimeoutError_UsesCallerLabel_NotHardcodedProvider(t *testing.T) {
	const timeout = 20 * time.Millisecond
	const label = `mcp target (name "foo")`
	frc := newFakeReadCloser()
	_, cancel := context.WithCancel(context.Background())
	wb := newWatchdogBody(frc, cancel, timeout, label, nil)
	defer wb.Close()

	time.Sleep(3 * timeout) // let the watchdog fire
	close(frc.unblock)      // simulate the underlying connection erroring out once canceled
	_, err := wb.Read(make([]byte, 16))
	if err == nil {
		t.Fatal("Read: want an error once the watchdog has fired, got nil")
	}
	if !strings.Contains(err.Error(), label) {
		t.Errorf("err = %v, want it to contain the caller's own label (%q) verbatim", err, label)
	}
	if strings.Contains(err.Error(), `provider "mcp target`) {
		t.Errorf(`err = %v, must not wrap the label in a hardcoded "provider %%q" — an MCP/A2A target is not a provider`, err)
	}
}
