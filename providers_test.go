package traefikllmgateway

import (
	"context"
	"net/http"
	"testing"
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
