package traefikllmgateway

import (
	"bytes"
	"context"
	"fmt"
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

// TestLogAuthEvent_ThrottleEngaged_DistinctLine proves logAuthEvent emits
// a distinct "auth throttle:" line, not the routine "auth failed:" one,
// once a source IP has crossed authFailureLimit (security review round
// 2, 2026-08-22, important finding 5) — an operator seeing this line
// knows a SOURCE, not just one request, needs attention.
func TestLogAuthEvent_ThrottleEngaged_DistinctLine(t *testing.T) {
	gw := newTestGatewayForLogger(t)
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	gw.auth.nowFn = clock.Now

	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.RemoteAddr = "203.0.113.9:1234"
	for i := 0; i < authFailureLimit; i++ {
		gw.auth.recordAuthFailure(clientIP(r))
	}

	out := captureStderr(t, func() {
		gw.logAuthEvent(false, "", r)
	})

	if !strings.Contains(out, "auth throttle:") {
		t.Errorf("want an 'auth throttle:' line once the IP has crossed the limit, got %q", out)
	}
	if strings.Contains(out, "auth failed:") {
		t.Errorf("want the routine 'auth failed:' line suppressed in favor of the distinct throttle line, got %q", out)
	}
}

// TestLogAuthEvent_Failure_SuppressedCountReported proves the routine
// failure line's rate-limit gate reports how many events it suppressed
// (security review round 2, 2026-08-22, important finding 6): a single
// global gate would otherwise make several rapid failures within one
// window indistinguishable from a single one — the next line actually
// written must name how many were collapsed into it.
func TestLogAuthEvent_Failure_SuppressedCountReported(t *testing.T) {
	gw := newTestGatewayForLogger(t)
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	gw.auth.nowFn = clock.Now

	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.RemoteAddr = "203.0.113.9:1234"

	captureStderr(t, func() {
		gw.logAuthEvent(false, "", r) // logged (first call on a fresh gate)
		gw.logAuthEvent(false, "", r) // suppressed #1
		gw.logAuthEvent(false, "", r) // suppressed #2
	})

	clock.Advance(authFailureLogEvery + time.Second)
	out := captureStderr(t, func() {
		gw.logAuthEvent(false, "", r)
	})

	if !strings.Contains(out, "2 more suppressed since last log") {
		t.Errorf("want the next line to report 2 suppressed events, got %q", out)
	}
}

// TestWarnf_WritesWarnPrefix pins warnf's own severity token, so the
// level cannot drift back without a test noticing.
func TestWarnf_WritesWarnPrefix(t *testing.T) {
	gw := newTestGatewayForLogger(t)

	out := captureStderr(t, func() {
		gw.warnf("something resolved itself: %d", 7)
	})

	if !strings.Contains(out, "llmgw[llmgw] WARN something resolved itself: 7") {
		t.Errorf("warnf output = %q, want a WARN-prefixed line", out)
	}
	if strings.Contains(out, "ERROR") {
		t.Errorf("warnf must not emit an ERROR line, got %q", out)
	}
}

// TestWarnCollisionOnce_LogsAtWarnNotError is the regression for the
// model-id collision line's severity (2026-08-22). A model id served by
// two configured providers is resolved deterministically by the registry
// and affects no request, so it must not be an ERROR: on a cluster with
// overlapping catalogs it put 144 ERROR lines in a healthy pod's boot
// log, which is exactly the noise that hides a real failure.
func TestWarnCollisionOnce_LogsAtWarnNotError(t *testing.T) {
	gw := newTestGatewayForLogger(t)
	reg, err := newModelRegistry(map[string]providerAdapter{}, CreateConfig(), gw.errorf)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	reg.warn = gw.warnf

	out := captureStderr(t, func() {
		reg.warnCollisionOnce("gpt-4o", "openai", []string{"openai", "openai-audio"})
	})

	if !strings.Contains(out, "WARN model registry: model id \"gpt-4o\" is provided by multiple providers") {
		t.Errorf("collision line = %q, want it emitted at WARN", out)
	}
	if strings.Contains(out, "ERROR") {
		t.Errorf("collision line must not be an ERROR, got %q", out)
	}

	// Still logged only once per id, regardless of severity.
	second := captureStderr(t, func() {
		reg.warnCollisionOnce("gpt-4o", "openai", []string{"openai", "openai-audio"})
	})
	if second != "" {
		t.Errorf("second collision for the same id logged again: %q", second)
	}
}

// TestRegistryWarnf_FallsBackToLogWhenWarnNil covers the nil-warn path:
// a registry built without an injected warn (every test call site, and
// any future caller that forgets) must still emit the line through log
// rather than panicking or dropping it silently.
func TestRegistryWarnf_FallsBackToLogWhenWarnNil(t *testing.T) {
	var got []string
	reg, err := newModelRegistry(map[string]providerAdapter{}, CreateConfig(), func(format string, args ...any) {
		got = append(got, fmt.Sprintf(format, args...))
	})
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	if reg.warn != nil {
		t.Fatal("newModelRegistry must leave warn nil; it is injected by the caller")
	}

	reg.warnCollisionOnce("m1", "p1", []string{"p1", "p2"})

	if len(got) != 1 {
		t.Fatalf("log calls = %d, want 1 (the fallback path), got %v", len(got), got)
	}
	if !strings.Contains(got[0], `model id "m1" is provided by multiple providers`) {
		t.Errorf("fallback line = %q, want the collision message", got[0])
	}
}
