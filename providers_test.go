package traefikllmgateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"testing/iotest"
)

// TestProviderPassthroughEnabled is the security+performance audit
// (2026-08-22) table for ProviderConfig.Passthrough's default: nil (the
// zero value — every provider configured before this field existed)
// means enabled, matching prior behavior exactly; only an explicit false
// disables it.
func TestProviderPassthroughEnabled(t *testing.T) {
	trueVal, falseVal := true, false
	cases := []struct {
		pc   *ProviderConfig
		name string
		want bool
	}{
		{name: "nil field defaults to enabled", pc: &ProviderConfig{Type: "openai"}, want: true},
		{name: "explicit true stays enabled", pc: &ProviderConfig{Type: "openai", Passthrough: &trueVal}, want: true},
		{name: "explicit false disables", pc: &ProviderConfig{Type: "openai", Passthrough: &falseVal}, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{Providers: map[string]*ProviderConfig{"openai": c.pc}}
			if got := providerPassthroughEnabled(cfg, "openai"); got != c.want {
				t.Errorf("providerPassthroughEnabled = %v, want %v", got, c.want)
			}
		})
	}

	t.Run("unknown provider name defaults to enabled", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{}}
		if !providerPassthroughEnabled(cfg, "notconfigured") {
			t.Error("want true for a provider name absent from cfg.Providers")
		}
	})
}

// TestAttemptRecorderFromContext_RoundTrip proves withAttemptRecorder/
// attemptRecorderFromContext round-trip a non-nil recorder, and that the
// SAME func value comes back out (asserted indirectly here, by observing
// it fire when invoked) — Feature A's (v0.22) chokepoint plumbing between
// providers.go, retry.go's policy.do, and routes_passthrough.go's
// proxyUpstream.
func TestAttemptRecorderFromContext_RoundTrip(t *testing.T) {
	var gotResp *http.Response
	var gotErr error
	rec := attemptRecorder(func(resp *http.Response, err error) {
		gotResp = resp
		gotErr = err
	})

	ctx := withAttemptRecorder(context.Background(), rec)
	got := attemptRecorderFromContext(ctx)
	if got == nil {
		t.Fatal("attemptRecorderFromContext returned nil after withAttemptRecorder set one")
	}

	wantResp := &http.Response{StatusCode: http.StatusOK}
	got(wantResp, nil)
	if gotResp != wantResp {
		t.Errorf("gotResp = %v, want %v (the recorder round-tripped through ctx must be the same func)", gotResp, wantResp)
	}
	if gotErr != nil {
		t.Errorf("gotErr = %v, want nil", gotErr)
	}
}

// TestAttemptRecorderFromContext_NoneSet proves a ctx that never called
// withAttemptRecorder — the common case: registry.go's discovery
// listModels calls, and handleTargetProxy's MCP/A2A target proxy
// (mcp_a2a.go), neither of which is Feature A's accounting scope —
// returns a nil recorder, not a panic or a zero-value func that panics
// when invoked.
func TestAttemptRecorderFromContext_NoneSet(t *testing.T) {
	if got := attemptRecorderFromContext(context.Background()); got != nil {
		t.Errorf("attemptRecorderFromContext(bare ctx) = %v, want nil", got)
	}
}

// TestWithAttemptRecorder_NilRecorder_ReturnsSameContext proves passing a
// nil recorder is a safe no-op a caller may call unconditionally — it
// does not wrap ctx in a no-op value that would still satisfy a later
// "!= nil" nil-check with a non-functional func.
func TestWithAttemptRecorder_NilRecorder_ReturnsSameContext(t *testing.T) {
	ctx := context.Background()
	got := withAttemptRecorder(ctx, nil)
	if got != ctx { //nolint:staticcheck // deliberately comparing context identity, not using it as a map key
		t.Error("withAttemptRecorder(ctx, nil) returned a different context; want ctx unchanged")
	}
	if rec := attemptRecorderFromContext(got); rec != nil {
		t.Errorf("attemptRecorderFromContext = %v, want nil after a nil recorder was passed in", rec)
	}
}

// TestReadAllLimited compares readAllLimited against the reference
// io.ReadAll(io.LimitReader(r, limit)) on an identical input for every
// shape of contentLength hint: exact, too small, too large, unknown (-1),
// zero with a non-empty body, larger than limit, and a body longer than
// limit (which must still truncate to limit) both via the io.ReadAll
// fallback (hint <= 0 or hint > limit) and via the presized bytes.Buffer
// fast path directly (hint > 0 and hint <= limit) — the fast path's own
// io.LimitReader must still do the truncating, not just size the buffer.
func TestReadAllLimited(t *testing.T) {
	cases := []struct {
		name          string
		body          []byte
		contentLength int64
		limit         int64
	}{
		{name: "exact content-length", body: []byte("hello world"), contentLength: 11, limit: 1024},
		{name: "hint smaller than body", body: []byte("hello world"), contentLength: 3, limit: 1024},
		{name: "hint larger than body", body: []byte("hi"), contentLength: 1000, limit: 1024},
		{name: "unknown content-length (-1)", body: []byte("hello"), contentLength: -1, limit: 1024},
		{name: "zero hint with non-empty body", body: []byte("hello"), contentLength: 0, limit: 1024},
		{name: "hint larger than limit", body: []byte("hello world"), contentLength: 2000, limit: 5},
		{name: "body longer than limit, hint exceeds limit (fallback path) truncates", body: bytes.Repeat([]byte("x"), 20), contentLength: 20, limit: 10},
		{name: "body longer than limit, unknown hint (fallback path) truncates", body: bytes.Repeat([]byte("x"), 20), contentLength: -1, limit: 10},
		{name: "body longer than limit, hint equals limit (fast path) truncates", body: bytes.Repeat([]byte("x"), 20), contentLength: 10, limit: 10},
		{name: "body longer than limit, hint below limit (fast path) truncates", body: bytes.Repeat([]byte("x"), 20), contentLength: 5, limit: 10},
		{name: "empty body", body: nil, contentLength: 0, limit: 1024},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want, wantErr := io.ReadAll(io.LimitReader(bytes.NewReader(c.body), c.limit))
			got, gotErr := readAllLimited(bytes.NewReader(c.body), c.contentLength, c.limit)

			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("err = %v, want error presence %v", gotErr, wantErr != nil)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("readAllLimited(%q, %d, %d) = %q, want %q", c.body, c.contentLength, c.limit, got, want)
			}
		})
	}
}

// TestReadAllLimited_ClampsHintToReadAllHintMax asserts a large declared
// Content-Length never drives an unbounded up-front allocation before any
// byte has arrived — an authenticated peer that declares a huge length and
// then stalls must not make readAllLimited hold more than readAllHintMax
// per call — while a genuinely large body (bigger than readAllHintMax)
// still reads back identically to the io.ReadAll(io.LimitReader(...))
// reference, just via regrowth past the initial cap.
func TestReadAllLimited_ClampsHintToReadAllHintMax(t *testing.T) {
	t.Run("huge declared length, tiny body: allocation stays capped", func(t *testing.T) {
		body := []byte("0123456789")
		const contentLength = 32 << 20
		const limit = 32 << 20

		got, err := readAllLimited(bytes.NewReader(body), contentLength, limit)
		if err != nil {
			t.Fatalf("readAllLimited: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("readAllLimited = %q, want %q", got, body)
		}
		if cap(got) > readAllHintMax+bytes.MinRead {
			t.Errorf("cap(got) = %d, want <= %d: the declared length must not drive an unbounded up-front allocation",
				cap(got), readAllHintMax+bytes.MinRead)
		}
	})

	t.Run("body larger than readAllHintMax with exact hint still reads fully", func(t *testing.T) {
		body := bytes.Repeat([]byte("y"), readAllHintMax+100)
		contentLength := int64(len(body))
		limit := int64(len(body))

		want, wantErr := io.ReadAll(io.LimitReader(bytes.NewReader(body), limit))
		if wantErr != nil {
			t.Fatalf("reference io.ReadAll: %v", wantErr)
		}
		got, err := readAllLimited(bytes.NewReader(body), contentLength, limit)
		if err != nil {
			t.Fatalf("readAllLimited: %v", err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("readAllLimited returned %d bytes, want %d bytes matching reference", len(got), len(want))
		}
	})
}

// TestReadAllLimited_PropagatesNonEOFError asserts a reader that yields
// some data and then a non-EOF error fails readAllLimited the same way it
// fails the reference io.ReadAll(io.LimitReader(r, limit)) — the injected
// error must still be reachable via errors.Is, not swallowed.
func TestReadAllLimited_PropagatesNonEOFError(t *testing.T) {
	injected := errors.New("stub: read failed")
	newFailingReader := func() io.Reader {
		return io.MultiReader(bytes.NewReader([]byte("partial")), iotest.ErrReader(injected))
	}

	_, refErr := io.ReadAll(io.LimitReader(newFailingReader(), 1024))
	if !errors.Is(refErr, injected) {
		t.Fatalf("reference io.ReadAll(io.LimitReader) err = %v, want errors.Is match for %v", refErr, injected)
	}

	_, gotErr := readAllLimited(newFailingReader(), 100, 1024)
	if gotErr == nil {
		t.Fatal("readAllLimited: want a non-nil error")
	}
	if !errors.Is(gotErr, injected) {
		t.Errorf("readAllLimited err = %v, want errors.Is match for %v", gotErr, injected)
	}
}
