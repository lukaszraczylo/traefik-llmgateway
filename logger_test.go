package traefikllmgateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestGatewayForLogger builds a minimal *Gateway via the real New()
// constructor (the only supported construction path — see this package's
// other test files, none of which build a bare &Gateway{} literal) for
// logAuthEvent's own tests below.
func newTestGatewayForLogger(t *testing.T) *Gateway {
	t.Helper()
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}
	return gw
}

// captureStderr redirects os.Stderr for the duration of fn (the same
// os.Pipe-based idiom this package's other test files already use for
// logf/errorf, e.g. TestGateway_Logf_WritesInfoPrefix, llmgateway_test.go)
// and returns everything written to it.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = origStderr
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}
	return buf.String()
}

// TestLogAuthEvent_Failure_RateLimited is the regression for the missing
// rate limit on logAuthEvent's failure line (security audit finding 3,
// 2026-08-22): an attacker driving many failed-auth attempts in quick
// succession must produce ONE log line per authFailureLogEvery, not one
// per attempt — otherwise the synchronous os.Stderr write itself becomes
// an amplification vector on the shared Traefik ingress.
func TestLogAuthEvent_Failure_RateLimited(t *testing.T) {
	gw := newTestGatewayForLogger(t)
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	gw.auth.nowFn = clock.Now

	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.RemoteAddr = "203.0.113.9:1234"

	out := captureStderr(t, func() {
		for i := 0; i < 5; i++ {
			gw.logAuthEvent(false, "", r)
		}
	})

	if n := strings.Count(out, "auth failed:"); n != 1 {
		t.Fatalf("auth-failed lines = %d, want exactly 1 (5 attempts within one authFailureLogEvery window), output=%q", n, out)
	}
}

// TestLogAuthEvent_Failure_LogsAgainAfterIntervalElapses proves the rate
// limit is a cadence, not a one-shot silence: once authFailureLogEvery has
// actually elapsed, the next failure is logged again.
func TestLogAuthEvent_Failure_LogsAgainAfterIntervalElapses(t *testing.T) {
	gw := newTestGatewayForLogger(t)
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	gw.auth.nowFn = clock.Now

	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.RemoteAddr = "203.0.113.9:1234"

	out := captureStderr(t, func() {
		gw.logAuthEvent(false, "", r)
		clock.Advance(authFailureLogEvery + time.Second)
		gw.logAuthEvent(false, "", r)
	})

	if n := strings.Count(out, "auth failed:"); n != 2 {
		t.Fatalf("auth-failed lines = %d, want exactly 2 (interval elapsed between the two attempts), output=%q", n, out)
	}
}

// TestLogAuthEvent_Success_NeverRateLimited proves successful-auth logging
// is deliberately left unbounded (shouldLogAuthFailure's own doc comment,
// auth.go, justifies why): it takes an already-valid key to reach, so its
// volume is bounded by legitimate traffic elsewhere, not by an
// unauthenticated caller's attempt rate.
func TestLogAuthEvent_Success_NeverRateLimited(t *testing.T) {
	gw := newTestGatewayForLogger(t)
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	gw.auth.nowFn = clock.Now

	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)

	out := captureStderr(t, func() {
		for i := 0; i < 5; i++ {
			gw.logAuthEvent(true, "alice", r)
		}
	})

	if n := strings.Count(out, "auth ok:"); n != 5 {
		t.Fatalf("auth-ok lines = %d, want 5 (success logging must never be rate-limited), output=%q", n, out)
	}
}
