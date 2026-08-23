package traefikllmgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// newMetricsTestConfig builds a Config with Metrics enabled at the default
// path, one provider, one group ("g"), a non-admin user ("alice") and an
// admin user ("admin1") — the minimal shape every gate/structure test
// below needs. Callers may mutate the returned Config (in particular
// cfg.Metrics) before calling New.
func newMetricsTestConfig() *Config {
	cfg := CreateConfig()
	cfg.Metrics = &MetricsConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: "http://openai.invalid", APIKey: "sk-up", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"g": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "g", APIKey: "sk-alice", Limits: &LimitsConfig{}},
		{Name: "admin1", Group: "g", APIKey: "sk-admin1", Admin: true},
	}}
	return cfg
}

// newMetricsGatewayHandle mirrors newAdminGatewayHandle (admin_test.go):
// builds the Gateway from cfg and returns both the http.Handler ServeHTTP
// uses and the concrete *Gateway, with recordProviderAttempt's store
// write forced synchronous so a test asserting on a resulting provider
// counter never races the production fire-and-forget spawn (see
// newAdminGatewayHandle's own doc comment for why).
func newMetricsGatewayHandle(t *testing.T, cfg *Config) (http.Handler, *Gateway) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}
	gw.limiter.spawn = func(f func()) { f() }
	return h, gw
}

func metricsRequest(method, path, apiKey string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req
}

// --- gate: disabled/custom-path/unauth/non-admin/admin/CIDR ---

func TestMetrics_Disabled_FallsThroughTo404(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	cfg.Metrics = nil
	h, _ := newMetricsGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (metrics nil)", rec.Code)
	}
}

func TestMetrics_DisabledExplicitly_FallsThroughTo404(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	cfg.Metrics = &MetricsConfig{Enabled: false}
	h, _ := newMetricsGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (metrics disabled, even with a valid admin key)", rec.Code)
	}
}

func TestMetrics_CustomPath(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	cfg.Metrics = &MetricsConfig{Enabled: true, Path: "/internal/metrics"}
	h, _ := newMetricsGatewayHandle(t, cfg)

	// The default path is no longer registered once Path is set.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("default path status = %d, want 404 (Path overrides it)", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, "/internal/metrics", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("custom path status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
}

// TestMetrics_GateMatrix covers every auth outcome the brief calls out by
// name: unauthenticated is rejected, a non-admin key is rejected, an
// admin key is accepted, a source address inside AllowedCIDRs is accepted
// with no key at all, and a source address outside it is still rejected
// with no key.
func TestMetrics_GateMatrix(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	// httptest.NewRequest's default RemoteAddr is 192.0.2.1:1234 (verified
	// empirically, not assumed) — 192.0.2.0/24 is the allowlisted block
	// below, so a request built via metricsRequest with no explicit
	// RemoteAddr override lands inside it.
	cfg.Metrics.AllowedCIDRs = []string{"192.0.2.0/24"}
	h, _ := newMetricsGatewayHandle(t, cfg)

	t.Run("unauthenticated_non_allowlisted", func(t *testing.T) {
		req := metricsRequest(http.MethodGet, metricsPathDefault, "")
		req.RemoteAddr = "203.0.113.9:5555" // TEST-NET-3, outside 192.0.2.0/24
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("authenticated_non_admin", func(t *testing.T) {
		req := metricsRequest(http.MethodGet, metricsPathDefault, "sk-alice")
		req.RemoteAddr = "203.0.113.9:5555" // outside the allowlist too — the key path must still gate on admin
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
	t.Run("admin_key_from_non_allowlisted_address", func(t *testing.T) {
		req := metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1")
		req.RemoteAddr = "203.0.113.9:5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (admin bearer token always works), body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("allowlisted_cidr_no_key", func(t *testing.T) {
		req := metricsRequest(http.MethodGet, metricsPathDefault, "") // default RemoteAddr, inside 192.0.2.0/24
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (source address inside AllowedCIDRs), body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestMetrics_NoAllowlistConfigured_KeylessRequestRejected proves the
// CIDR bypass is opt-in: with no AllowedCIDRs at all, even the default
// httptest RemoteAddr (which the test above proves IS matched once
// configured) gets 401 without a key.
func TestMetrics_NoAllowlistConfigured_KeylessRequestRejected(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig() // no AllowedCIDRs
	h, _ := newMetricsGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no allowlist configured)", rec.Code)
	}
}

// TestMetrics_AllowlistedRequest_NeverTouchesAuthFailureTracking pins the
// ORDERING handleMetrics' own doc comment claims (metrics.go): the CIDR
// allowlist is checked BEFORE auth.identify ever runs, so a keyless,
// allowlisted scrape never records a failed-auth attempt for its own
// source address. This is not observable from status codes alone — a
// handler that checked identify FIRST and fell back to the CIDR
// allowlist only on failure would return the identical 200 for every
// case TestMetrics_GateMatrix asserts, while still quietly feeding
// authStore's failure tracker on every single scrape. Mutation-proofing:
// reordering the two checks changes only this test's outcome, nothing
// else in this file.
func TestMetrics_AllowlistedRequest_NeverTouchesAuthFailureTracking(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	cfg.Metrics.AllowedCIDRs = []string{"192.0.2.0/24"}
	h, gw := newMetricsGatewayHandle(t, cfg)

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		// Default RemoteAddr (192.0.2.1:1234, verified empirically) —
		// inside the allowlist, no key presented at all.
		h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
	}

	if got := gw.auth.failures.get("192.0.2.1"); got != 0 {
		t.Errorf("auth failure count for the allowlisted scraper's address = %d, want 0 — the CIDR check must run before auth.identify, never after", got)
	}
}

func TestMetrics_InvalidCIDR_FailsConstruction(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	cfg.Metrics.AllowedCIDRs = []string{"not-a-cidr"}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	_, err := New(context.Background(), next, cfg, "llmgw")
	if err == nil {
		t.Fatal("New: want an error for a malformed allowedCIDRs entry, got nil")
	}
}

// --- structure: valid exposition format ---

// promSample is one parsed exposition-format sample line.
type promSample struct {
	name   string
	labels map[string]string
	value  string
}

// parsePrometheusText parses body as a Prometheus text-exposition
// document, failing the test on any structural violation. Label values
// are unescaped char-by-char, honoring \\, \", and \n exactly as the
// format (and escapeLabelValue, metrics.go) define them — a real,
// unescaped quote or embedded newline in a label value would desync this
// parser (an unterminated quote, or a value silently truncated where the
// injected control character landed), which is exactly the failure mode
// this test suite exists to catch.
func parsePrometheusText(t *testing.T, body []byte) (types map[string]string, samples []promSample) {
	t.Helper()
	types = map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, kind, ok := strings.Cut(rest, " ")
			if !ok {
				t.Fatalf("malformed TYPE line: %q", line)
			}
			types[name] = kind
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		samples = append(samples, parsePromSample(t, line))
	}
	return types, samples
}

func parsePromSample(t *testing.T, line string) promSample {
	t.Helper()
	i := 0
	for i < len(line) && line[i] != '{' && line[i] != ' ' {
		i++
	}
	name := line[:i]
	if name == "" {
		t.Fatalf("empty metric name in line: %q", line)
	}
	labels := map[string]string{}
	if i < len(line) && line[i] == '{' {
		i++
		for {
			if i >= len(line) {
				t.Fatalf("unterminated label block in line: %q", line)
			}
			if line[i] == '}' {
				i++
				break
			}
			lnStart := i
			for i < len(line) && line[i] != '=' {
				i++
			}
			if i >= len(line) {
				t.Fatalf("malformed label (no '=') in line: %q", line)
			}
			lname := line[lnStart:i]
			i++ // skip '='
			if i >= len(line) || line[i] != '"' {
				t.Fatalf("malformed label value (no opening quote) in line: %q", line)
			}
			i++ // skip opening quote
			var val strings.Builder
			for {
				if i >= len(line) {
					t.Fatalf("unterminated label value in line: %q", line)
				}
				c := line[i]
				if c == '\\' {
					i++
					if i >= len(line) {
						t.Fatalf("dangling escape in line: %q", line)
					}
					switch line[i] {
					case '\\':
						val.WriteByte('\\')
					case '"':
						val.WriteByte('"')
					case 'n':
						val.WriteByte('\n')
					default:
						t.Fatalf("unknown escape \\%c in line: %q", line[i], line)
					}
					i++
					continue
				}
				if c == '"' {
					i++
					break
				}
				val.WriteByte(c)
				i++
			}
			labels[lname] = val.String()
			if i < len(line) && line[i] == ',' {
				i++
				continue
			}
			if i < len(line) && line[i] == '}' {
				i++
				break
			}
			t.Fatalf("malformed label separator in line: %q", line)
		}
	}
	for i < len(line) && line[i] == ' ' {
		i++
	}
	value := line[i:]
	if value == "" {
		t.Fatalf("missing value in line: %q", line)
	}
	if _, err := strconv.ParseFloat(value, 64); err != nil {
		t.Fatalf("sample value %q does not parse as a number in line: %q", value, line)
	}
	return promSample{name: name, labels: labels, value: value}
}

// metricNameCharset is the character set a Prometheus metric name must
// match: [a-zA-Z_:][a-zA-Z0-9_:]*.
func validMetricName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_' || r == ':':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// TestMetrics_OutputParsesAsValidExposition seeds real traffic (a
// successful request, a rejected one, and an opted-in model breakdown)
// so every metric family below actually has at least one sample line,
// then asserts structure rather than a golden blob: every metric name is
// well-formed, every family has a # TYPE line, every counter's name ends
// "_total", and no (name, label set) combination repeats.
func TestMetrics_OutputParsesAsValidExposition(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	cfg := newMetricsTestConfig()
	cfg.Metrics.ModelLabel = true
	cfg.Providers["openai"].BaseURL = srv.URL
	cfg.Users.Inline[0].Limits = &LimitsConfig{RequestsPerMinute: 1}
	h, gw := newMetricsGatewayHandle(t, cfg)
	// A configured (if in-process) store, so llmgateway_limit_store_up
	// (item 7) also has a sample to check structure against below —
	// newMemoryStore behaves identically to the fallback it stands in
	// for, so this changes nothing else about this test's traffic.
	gw.limiter.store = newMemoryStore()

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	// First request: admitted, and succeeds against the local httptest
	// server — seeds request/token/cost/provider counters via
	// checkAndCount+account+recordProviderAttempt.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	// Second request: alice's RequestsPerMinute:1 is now exhausted, so
	// this one is rejected — seeding llmgateway_rate_limit_rejections_total.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429 (RequestsPerMinute:1 exhausted)", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain prefix", ct)
	}

	types, samples := parsePrometheusText(t, rec.Body.Bytes())
	if len(samples) == 0 {
		t.Fatal("no samples parsed at all")
	}

	seen := map[string]bool{}
	for _, s := range samples {
		if !validMetricName(s.name) {
			t.Errorf("metric name %q does not match [a-zA-Z_:][a-zA-Z0-9_:]*", s.name)
		}
		// familyName strips a histogram's own _bucket/_sum/_count sample
		// suffix before the # TYPE lookup: a real Prometheus exposition
		// document carries exactly ONE "# TYPE <base> histogram" line per
		// family (metricWriter.family, metrics.go's histogram method),
		// covering all three suffixed sample names — that is the format's
		// actual rule (promparse.go's own histogram handling), not a gap
		// in this test. Every non-histogram family's sample name already
		// equals its own base name, so this is a no-op for them.
		familyName := s.name
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			if trimmed, ok := strings.CutSuffix(s.name, suffix); ok {
				if k, ok := types[trimmed]; ok && k == "histogram" {
					familyName = trimmed
				}
			}
		}
		kind, ok := types[familyName]
		if !ok {
			t.Errorf("sample %q has no # TYPE line", s.name)
		}
		if kind == "counter" && !strings.HasSuffix(s.name, "_total") {
			t.Errorf("counter %q does not end in _total", s.name)
		}
		if kind == "histogram" && s.name != familyName+"_bucket" && s.name != familyName+"_sum" && s.name != familyName+"_count" {
			t.Errorf("histogram sample %q does not end in _bucket/_sum/_count", s.name)
		}

		labelKeys := make([]string, 0, len(s.labels))
		for k := range s.labels {
			labelKeys = append(labelKeys, k)
		}
		sort.Strings(labelKeys)
		var key strings.Builder
		key.WriteString(s.name)
		for _, k := range labelKeys {
			key.WriteByte('\x00')
			key.WriteString(k)
			key.WriteByte('=')
			key.WriteString(s.labels[k])
		}
		if seen[key.String()] {
			t.Errorf("duplicate series: %s%v", s.name, s.labels)
		}
		seen[key.String()] = true
	}

	wantFamilies := []string{
		"llmgateway_requests_total",
		"llmgateway_tokens_total",
		"llmgateway_cost_micro_usd_total",
		"llmgateway_budget_consumed_ratio",
		"llmgateway_provider_attempts_total",
		"llmgateway_provider_failures_total",
		"llmgateway_provider_healthy",
		"llmgateway_provider_model_attempts_total",
		"llmgateway_provider_model_failures_total",
		"llmgateway_rate_limit_rejections_total",
		"llmgateway_limit_store_up",
		"llmgateway_upstream_ttfb_seconds",
		"llmgateway_upstream_duration_seconds",
	}
	for _, name := range wantFamilies {
		if _, ok := types[name]; !ok {
			t.Errorf("expected family %q to have a # TYPE line, got none", name)
		}
	}
}

// --- label escaping: the correctness requirement, not a nicety ---

func TestEscapeLabelValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "gpt-test", "gpt-test"},
		{"quote", `say "hi"`, `say \"hi\"`},
		{"backslash", `C:\models\x`, `C:\\models\\x`},
		{"newline", "line1\nline2", `line1\nline2`},
		{"quote_then_backslash", `"\`, `\"\\`},
		{"backslash_then_quote", `\"`, `\\\"`},
		{"all_three", "a\"b\\c\nd", `a\"b\\c\nd`},
		// Invalid UTF-8 bytes (0x80/0x81 are bare continuation bytes,
		// never valid as the START of any UTF-8 sequence) must become a
		// deterministic, recoverable \xHH marker — see
		// TestEscapeLabelValue_InvalidUTF8_InjectiveAndValidUTF8 below for
		// why this is the only choice satisfying both required
		// properties (injective, and always valid UTF-8 out).
		{"invalid_utf8_lone_0x80", "u\x80", `u\x80`},
		{"invalid_utf8_lone_0x81", "u\x81", `u\x81`},
		// A literal, user-typed "\x80" (five ordinary ASCII characters,
		// not a raw invalid byte) must render distinguishably from the
		// marker above: its own backslash gets doubled like any other,
		// producing \\x80 (six characters) rather than colliding with
		// the four-character \x80 marker an actual invalid byte 0x80
		// produces.
		{"literal_backslash_x80_text", `\x80`, `\\x80`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := escapeLabelValue(tt.in); got != tt.want {
				t.Errorf("escapeLabelValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestEscapeLabelValue_InvalidUTF8_InjectiveAndValidUTF8 is the
// mutation-proofing test for the two-round rewrite (review fixes,
// adversarial verification 2026-08-23, round 2 correcting round 1):
//
//   - Round 1 fixed a rune-oriented `for _, r := range s` implementation,
//     which decodes invalid UTF-8 to U+FFFD and is therefore NOT
//     injective: "u\x80" and "u\x81" both render as "u�", colliding two
//     distinct scope_id values onto one series (a duplicate series).
//   - Round 1's own fix (copying an invalid byte through raw) achieved
//     injectivity but broke the OTHER required property: the OUTPUT is
//     then itself invalid UTF-8. Measured against prometheus/prometheus's
//     real scrape parser (promparse.go) this round: that is FATAL — the
//     scrape errors out entirely (target reported DOWN), which is worse
//     than a duplicate series (measured non-fatal against scrape.go:
//     dropped, target stays up). Round 1's own doc comment asserted the
//     opposite — that a duplicate series is what takes a target down —
//     which was the wrong premise, corrected here.
//
// This test pins both properties at once, since a fix for either one
// alone (as both prior rounds separately proved) is not sufficient.
func TestEscapeLabelValue_InvalidUTF8_InjectiveAndValidUTF8(t *testing.T) {
	t.Parallel()
	a := escapeLabelValue("u\x80")
	b := escapeLabelValue("u\x81")
	if a == b {
		t.Fatalf("escapeLabelValue(%q) == escapeLabelValue(%q) == %q — two distinct inputs collided onto one label value (not injective)", "u\x80", "u\x81", a)
	}
	if !utf8.ValidString(a) {
		t.Errorf("escapeLabelValue(%q) = %q is not valid UTF-8 — fatal to a real Prometheus scrape (promparse.go), not merely a duplicate series", "u\x80", a)
	}
	if !utf8.ValidString(b) {
		t.Errorf("escapeLabelValue(%q) = %q is not valid UTF-8 — fatal to a real Prometheus scrape (promparse.go), not merely a duplicate series", "u\x81", b)
	}
}

// TestEscapeLabelValue_Exhaustive1And2Byte_InjectiveAndValidUTF8 proves
// both required properties by construction rather than by spot-check,
// over every possible 1-byte string (256) and every possible 2-byte
// string (65,536) — valid UTF-8, invalid UTF-8, and every ASCII special
// character alike — all checked against ONE shared map, so a collision
// BETWEEN a 1-byte and a 2-byte input (the escaped output is not
// length-preserving, so this is not automatically ruled out just
// because the inputs differ in length) would be caught too, not only a
// same-length collision. Fast enough (well under a second) to run on
// every test invocation, not just as a one-off manual verification.
func TestEscapeLabelValue_Exhaustive1And2Byte_InjectiveAndValidUTF8(t *testing.T) {
	t.Parallel()
	seen := make(map[string]string, 256+65536)
	check := func(in string) {
		out := escapeLabelValue(in)
		if !utf8.ValidString(out) {
			t.Fatalf("escapeLabelValue(%q) = %q is not valid UTF-8", in, out)
		}
		if prior, ok := seen[out]; ok && prior != in {
			t.Fatalf("collision: escapeLabelValue(%q) and escapeLabelValue(%q) both produced %q", prior, in, out)
		}
		seen[out] = in
	}
	for b := 0; b < 256; b++ {
		check(string([]byte{byte(b)}))
	}
	for a := 0; a < 256; a++ {
		for b := 0; b < 256; b++ {
			check(string([]byte{byte(a), byte(b)}))
		}
	}
}

// TestMetrics_LabelValuesWithSpecialChars_Escaped is the end-to-end
// version of TestEscapeLabelValue: a real user name containing a double
// quote, a backslash, and a newline (auth.go's buildEntry doc comment
// confirms a user name is validated as non-empty only, never restricted
// to configNamePattern's URL-path-segment character set) flows all the
// way into the scope_id label of a real rendered document, and the
// document still parses.
func TestMetrics_LabelValuesWithSpecialChars_Escaped(t *testing.T) {
	t.Parallel()
	const trickyName = `weird"user\name` + "\nwith-newline"
	cfg := newMetricsTestConfig()
	cfg.Users.Inline = append(cfg.Users.Inline, &UserConfig{Name: trickyName, Group: "g", APIKey: "sk-tricky"})
	h, gw := newMetricsGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }
	gw.limiter.incrCounter("user", trickyName, metricReq, windowDay, fixedNow, 3, dayWindowTTL)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	_, samples := parsePrometheusText(t, rec.Body.Bytes())
	var found *promSample
	for i, s := range samples {
		if s.name == "llmgateway_requests_total" && s.labels["scope_id"] == trickyName {
			found = &samples[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no llmgateway_requests_total sample with scope_id == %q found among %d samples", trickyName, len(samples))
	}
	if found.value != "3" {
		t.Errorf("value = %q, want 3", found.value)
	}
	// The raw bytes must never contain an unescaped copy of the tricky
	// name — only its escaped form — confirming this genuinely exercised
	// the escaping path rather than the name having been dropped/replaced.
	if strings.Contains(rec.Body.String(), trickyName) {
		t.Error("response body contains the raw, unescaped name — escaping did not run")
	}
}

// --- model label: opt-in ---

func TestMetrics_ModelLabel_AbsentByDefault(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	h, _ := newMetricsGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "llmgateway_provider_model_attempts_total") {
		t.Error("llmgateway_provider_model_attempts_total present with ModelLabel unset (default off)")
	}
	if strings.Contains(body, "llmgateway_provider_model_failures_total") {
		t.Error("llmgateway_provider_model_failures_total present with ModelLabel unset (default off)")
	}
}

func TestMetrics_ModelLabel_PresentWhenEnabled(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	cfg.Metrics.ModelLabel = true
	h, _ := newMetricsGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	_, samples := parsePrometheusText(t, rec.Body.Bytes())
	var found bool
	for _, s := range samples {
		if s.name == "llmgateway_provider_model_attempts_total" && s.labels["provider"] == "openai" && s.labels["model"] == "gpt-test" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no llmgateway_provider_model_attempts_total{provider=%q,model=%q} sample found", "openai", "gpt-test")
	}
}

// --- counters reflect actual recorded usage ---

func TestMetrics_RequestTokenCostCounters_ReflectSeededUsage(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	h, gw := newMetricsGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }
	gw.limiter.incrCounter("user", "alice", metricReq, windowDay, fixedNow, 7, dayWindowTTL)
	gw.limiter.account([]limitScope{{kind: "user", id: "alice"}}, usage{prompt: 40, completion: 10}, 2_500_000)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	_, samples := parsePrometheusText(t, rec.Body.Bytes())

	assertSample(t, samples, "llmgateway_requests_total", map[string]string{"scope_kind": "user", "scope_id": "alice"}, "7")
	assertSample(t, samples, "llmgateway_tokens_total", map[string]string{"scope_kind": "user", "scope_id": "alice", "direction": "prompt"}, "40")
	assertSample(t, samples, "llmgateway_tokens_total", map[string]string{"scope_kind": "user", "scope_id": "alice", "direction": "completion"}, "10")
	assertSample(t, samples, "llmgateway_cost_micro_usd_total", map[string]string{"scope_kind": "user", "scope_id": "alice"}, "2.5e+06")
}

// assertSample fails the test unless samples contains exactly one sample
// named name whose labels are exactly want (same keys, same values), and
// whose value round-trips (as a float64) to the same value wantValue
// does. Comparing as float64 rather than as a raw string tolerates
// strconv.FormatFloat's shortest-representation choice (e.g. "2.5e+06"
// for 2,500,000) without the test hardcoding that exact spelling.
func assertSample(t *testing.T, samples []promSample, name string, want map[string]string, wantValue string) {
	t.Helper()
	wantF, err := strconv.ParseFloat(wantValue, 64)
	if err != nil {
		t.Fatalf("assertSample: wantValue %q does not parse: %v", wantValue, err)
	}
	for _, s := range samples {
		if s.name != name || len(s.labels) != len(want) {
			continue
		}
		match := true
		for k, v := range want {
			if s.labels[k] != v {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		gotF, err := strconv.ParseFloat(s.value, 64)
		if err != nil {
			t.Fatalf("sample %s%v value %q does not parse: %v", name, s.labels, s.value, err)
		}
		if gotF != wantF {
			t.Errorf("sample %s%v value = %q, want %s", name, s.labels, s.value, wantValue)
		}
		return
	}
	t.Errorf("no sample %s with labels %v found", name, want)
}

// TestMetrics_ProviderCounters_ReflectRealTraffic drives one successful
// and one failing upstream call through the real unified route (not a
// direct counter seed) and asserts llmgateway_provider_attempts_total/
// _failures_total match — the same shape TestAdminOverview_
// ProviderRates_EndToEndAfterTraffic (admin_test.go) already proves for
// the JSON admin API, proving the identical underlying providerUsage read
// renders correctly here too.
func TestMetrics_ProviderCounters_ReflectRealTraffic(t *testing.T) {
	t.Parallel()
	var callN int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callN++
		if callN == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	cfg := newMetricsTestConfig()
	cfg.Providers["openai"].BaseURL = srv.URL
	h, _ := newMetricsGatewayHandle(t, cfg)

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	_, samples := parsePrometheusText(t, rec.Body.Bytes())

	assertSample(t, samples, "llmgateway_provider_attempts_total", map[string]string{"provider": "openai"}, "2")
	assertSample(t, samples, "llmgateway_provider_failures_total", map[string]string{"provider": "openai"}, "1")
}

// TestMetrics_ProviderHealthy_ReflectsOpenBreaker proves the gauge
// follows the discovery circuit breaker's real state, not a static 1 —
// mutating registry.states directly (same package, mirrors how
// registry_health_test.go inspects breaker state) rather than driving a
// real failing discovery loop, since this test only needs to prove the
// RENDERING reads health correctly, not re-prove the breaker state
// machine itself (registry_health_test.go already does that).
func TestMetrics_ProviderHealthy_ReflectsOpenBreaker(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	h, gw := newMetricsGatewayHandle(t, cfg)

	st := gw.registry.states["openai"]
	st.mu.Lock()
	st.health = breakerOpen
	st.mu.Unlock()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	_, samples := parsePrometheusText(t, rec.Body.Bytes())
	assertSample(t, samples, "llmgateway_provider_healthy", map[string]string{"provider": "openai"}, "0")
}

// --- store outage: skip, never fabricate a zero ---

// TestMetrics_StoreDown_SkipsRatherThanFabricatesZero is the mutation-
// proofing test for every `if su.storeDown { continue }` /
// `if mc.storeDown { continue }` guard in metrics.go: deleting any of
// them makes this test fail. Forces the SAME limiter every family in
// this file reads from into a store outage — a real, configured store
// (alwaysErrStore, routes_unified_test.go's own stub) that errors on
// every call — then asserts every store-backed family emits NO samples
// at all rather than a fabricated 0 (review fix, adversarial
// verification 2026-08-23: without the guards, a Redis blip reads as a
// hard 0 mid-outage, then jumps back to the real total on recovery —
// Prometheus treats that as a counter reset followed by a spurious
// full-total "increase").
//
// Deliberately does NOT touch gw.limiter.failOpen (round 2, adversarial
// verification 2026-08-23): newConfiguredLimiter (llmgateway.go)
// defaults it to true, and a round-1 version of this test hand-set it
// to false, which meant the suite only ever exercised the fail-CLOSED
// path — the one case where storeGetMulti's own ok=false already made
// the bug impossible to miss. The actual production default (failOpen
// true) silently read zeros from the empty in-process fallback and
// reported ok=true, bypassing every storeDown guard in this file
// entirely; that is the path this test now pins, matching
// limiter.configuredStoreDown's own doc comment (limits.go).
//
// llmgateway_provider_healthy is deliberately excluded from the "must be
// absent" list: it comes from the in-process discovery breaker, never
// the counterStore, so a store outage must NOT affect it — asserted
// present here for exactly that reason.
func TestMetrics_StoreDown_SkipsRatherThanFabricatesZero(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	cfg.Metrics.ModelLabel = true
	cfg.Users.Inline[0].Limits = &LimitsConfig{RequestsPerDay: 100}
	h, gw := newMetricsGatewayHandle(t, cfg)

	// Simulates "Redis was configured and reachable, then broke" without
	// standing up a real broken Redis connection: gw.limiter.store starts
	// nil (no cfg.Redis in newMetricsTestConfig) and is forced non-nil
	// here, exactly mirroring TestLimiterCurrentUsage_StoreDown's own
	// direct-field-injection style (admin_test.go). failOpen is left
	// untouched at its true default.
	gw.limiter.store = alwaysErrStore{}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	_, samples := parsePrometheusText(t, rec.Body.Bytes())
	mustBeAbsent := []string{
		"llmgateway_requests_total",
		"llmgateway_tokens_total",
		"llmgateway_cost_micro_usd_total",
		"llmgateway_budget_consumed_ratio",
		"llmgateway_provider_attempts_total",
		"llmgateway_provider_failures_total",
		"llmgateway_provider_model_attempts_total",
		"llmgateway_provider_model_failures_total",
	}
	for _, s := range samples {
		for _, absent := range mustBeAbsent {
			if s.name == absent {
				t.Errorf("sample %s%v present during a store outage — must be skipped, not fabricated as 0", s.name, s.labels)
			}
		}
	}

	var providerHealthyPresent bool
	for _, s := range samples {
		if s.name == "llmgateway_provider_healthy" {
			providerHealthyPresent = true
		}
	}
	if !providerHealthyPresent {
		t.Error("llmgateway_provider_healthy missing — this family reads in-process breaker state, not the store, and must still render during a store outage")
	}

	// llmgateway_limit_store_up (item 7, adversarial verification
	// 2026-08-23) is the one signal that IS supposed to go to 0 here —
	// it exists specifically to explain why every family above just
	// went silent.
	assertSample(t, samples, "llmgateway_limit_store_up", nil, "0")
}

// --- store health gauge ---

// TestMetrics_LimitStoreUp_AbsentWhenNoStoreConfigured proves the gauge
// is not emitted at all for a fallback-only deployment (no cfg.Redis) —
// there is no store health to report there.
func TestMetrics_LimitStoreUp_AbsentWhenNoStoreConfigured(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig() // no cfg.Redis
	h, _ := newMetricsGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if strings.Contains(rec.Body.String(), "llmgateway_limit_store_up") {
		t.Error("llmgateway_limit_store_up present with no store configured, want absent entirely")
	}
}

// TestMetrics_LimitStoreUp_ReflectsHealthyStore proves the gauge reads 1
// for a configured, currently-healthy store — newMemoryStore, not
// alwaysErrStore, standing in for "a real store that is up": it
// implements counterStore and never errors, so l.store is non-nil
// (configured) while l.storeLatched() stays false (never failed).
func TestMetrics_LimitStoreUp_ReflectsHealthyStore(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	h, gw := newMetricsGatewayHandle(t, cfg)

	gw.limiter.store = newMemoryStore()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	_, samples := parsePrometheusText(t, rec.Body.Bytes())
	assertSample(t, samples, "llmgateway_limit_store_up", nil, "1")
}

// --- rate-limit rejections ---

// TestMetrics_RateLimitRejections_CountedByScope drives a real 429
// (alice's RequestsPerMinute:1 exhausted by a second request) through the
// unified route, then asserts llmgateway_rate_limit_rejections_total
// carries exactly 1 for {scope_kind="user",scope_id="alice"} — the
// SAME scope checkAndCount's own violation message names.
func TestMetrics_RateLimitRejections_CountedByScope(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	cfg.Users.Inline[0].Limits = &LimitsConfig{RequestsPerMinute: 1}
	h, _ := newMetricsGatewayHandle(t, cfg)

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	_, samples := parsePrometheusText(t, rec.Body.Bytes())
	assertSample(t, samples, "llmgateway_rate_limit_rejections_total", map[string]string{"scope_kind": "user", "scope_id": "alice"}, "1")
}

// TestLimiter_RejectionSnapshot_StoreDown proves checkAndCount records a
// store_down rejection (distinct from a real limit breach) when the
// configured store errors and failOpen is false — driven directly
// against a *limiter (not through Gateway/HTTP), mirroring
// TestLimiterCurrentUsage_StoreDown's own direct-limiter style
// (admin_test.go).
func TestLimiter_RejectionSnapshot_StoreDown(t *testing.T) {
	t.Parallel()
	l := newLimiter(alwaysErrStore{}, false)
	scopes := []limitScope{{kind: "user", id: "x", limits: &LimitsConfig{RequestsPerMinute: 10}}}

	v := l.checkAndCount(scopes)
	if v == nil || !v.storeDown {
		t.Fatalf("checkAndCount = %+v, want a storeDown violation", v)
	}

	snaps := l.rejectionSnapshot()
	var found bool
	for _, s := range snaps {
		if s.kind == rejectionScopeStoreDown && s.count == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("rejectionSnapshot = %+v, want one store_down entry with count 1", snaps)
	}
}

// TestMetrics_PruneRejectionsAfterReload_EndToEnd is the end-to-end proof
// for item 7 (adversarial verification, 2026-08-23): a file-sourced user
// removed via a real users-file hot-reload (auth.go's replaceFileUsers,
// driven through ServeHTTP exactly as production traffic drives it, not
// a direct limiter call) must stop exporting
// llmgateway_rate_limit_rejections_total, proving the wiring from
// ServeHTTP through authStore.maybeReload's new return value to
// Gateway.pruneRejectionsAfterReload actually runs, not just the
// pruning logic in isolation (TestLimiter_PruneRejections_
// RemovesDeletedUserScope, limits_test.go).
func TestMetrics_PruneRejectionsAfterReload_EndToEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fp := filepath.Join(dir, "users.json")
	writeUsersDoc(t, fp, []*UserConfig{
		{Name: "alice", Group: "g", APIKey: "sk-alice", Limits: &LimitsConfig{RequestsPerMinute: 1}},
	}, time.Time{})

	cfg := newMetricsTestConfig()
	cfg.Users = &UsersConfig{
		File:   fp,
		Inline: []*UserConfig{{Name: "admin1", Group: "g", APIKey: "sk-admin1", Admin: true}},
	}
	h, gw := newMetricsGatewayHandle(t, cfg)

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	h.ServeHTTP(httptest.NewRecorder(), newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (RequestsPerMinute:1 exhausted)", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	_, samples := parsePrometheusText(t, rec.Body.Bytes())
	assertSample(t, samples, "llmgateway_rate_limit_rejections_total", map[string]string{"scope_kind": "user", "scope_id": "alice"}, "1")

	// Remove alice from the users file and force ServeHTTP's own
	// maybeReload call to pick it up — mirrors users_file_test.go's own
	// clock-advance idiom (newReloadableAuthStore).
	clock := &fakeClock{now: time.Now()}
	gw.auth.nowFn = clock.Now
	writeUsersDoc(t, fp, nil, time.Now().Add(time.Hour))
	clock.Advance(reloadEvery + time.Second)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	_, samples = parsePrometheusText(t, rec.Body.Bytes())
	for _, s := range samples {
		if s.name == "llmgateway_rate_limit_rejections_total" && s.labels["scope_id"] == "alice" {
			t.Errorf("alice's rejection series is still present after being removed from the users file and reloaded: %+v", s)
		}
	}
}

// TestServeHTTP_PruneRejectionsOnlyAfterRealReload is the mutation-
// proofing counterpart to TestMetrics_PruneRejectionsAfterReload_
// EndToEnd above: that test proves pruning DOES happen on a real
// reload; this one proves it does NOT happen otherwise (review fix,
// adversarial verification 2026-08-23 — replacing `if g.auth.
// maybeReload() { g.pruneRejectionsAfterReload() }` in ServeHTTP
// (llmgateway.go) with two unconditional statements survived the whole
// suite until this test existed).
//
// newMetricsTestConfig configures no Users.File at all, so authStore.
// maybeReload's very first check (usersFile == nil) makes it return
// false on every one of the 200 requests below — a real reload never
// happens. Seeds a rejection entry for "ghost", a name that is not any
// currently active user: pruneRejectionsAfterReload would delete it
// (it is not in authStore's own snapshot) the instant it ever ran, so
// its survival after 200 ordinary requests is the pinned guarantee.
func TestServeHTTP_PruneRejectionsOnlyAfterRealReload(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	h, gw := newMetricsGatewayHandle(t, cfg)

	gw.limiter.rejections.increment("user", "ghost")

	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
	}

	var found bool
	for _, s := range gw.limiter.rejectionSnapshot() {
		if s.kind == "user" && s.id == "ghost" {
			found = true
		}
	}
	if !found {
		t.Error(`the "ghost" rejection entry was pruned despite no real reload ever happening — pruneRejectionsAfterReload must run only when authStore.maybeReload reports a real reload, never unconditionally on every request`)
	}
}

// --- budget-consumed ratio ---

func TestMetrics_BudgetConsumedRatio_ReflectsUsageVsLimit(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	cfg.Users.Inline[0].Limits = &LimitsConfig{RequestsPerDay: 100}
	h, gw := newMetricsGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }
	gw.limiter.incrCounter("user", "alice", metricReq, windowDay, fixedNow, 25, dayWindowTTL)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	_, samples := parsePrometheusText(t, rec.Body.Bytes())

	assertSample(t, samples, "llmgateway_budget_consumed_ratio",
		map[string]string{"scope_kind": "user", "scope_id": "alice", "budget": "requests-per-day"}, "0.25")

	// The synthetic total scope carries no limit — no budget ratio sample
	// of any kind should exist for it.
	for _, s := range samples {
		if s.name == "llmgateway_budget_consumed_ratio" && s.labels["scope_kind"] == totalScopeKind {
			t.Errorf("unexpected budget ratio sample for the unlimited total scope: %+v", s)
		}
	}
}

// --- upstream latency (feat: instrument upstream latency) ---

// TestMetrics_LatencyHistogram_BucketsCumulativeOrdered_PlusInfPresent_SumCountConsistent
// is the exposition-format correctness test the task brief calls out by
// name: a malformed histogram does not drop one metric, it fails the
// whole scrape and the target reports DOWN — so this parses the real
// rendered document (not a golden string) and checks the format's own
// required shape by hand: buckets present for every configured bound
// plus +Inf, strictly ascending `le`, cumulative (non-decreasing)
// counts, +Inf last, and `_count` exactly equal to the final cumulative
// bucket.
//
// Observations are seeded directly via gw.recordLatency (bypassing
// watchdogBody's own real-clock capture, already covered by
// latency_test.go's TestWatchdogBody_ArmLatency_TTFBAtFirstRead_
// NotConstructionOrClose) so each one lands in a KNOWN bucket
// deterministically, including one past every configured bound (the
// overflow/+Inf case) — a real network delay could not reliably hit
// that case without an unreasonably slow test.
//
// MUTATION VERIFIED: changing metricWriter.histogram (metrics.go) to
// emit the DENSE per-bucket count directly (dropping the running
// `cumulative +=` accumulation, i.e. `m.sampleInt(name+"_bucket",
// bucketLabels, buckets[i])` instead of `cumulative`) made this test
// fail: consecutive bucket values were no longer non-decreasing (a
// dense count can drop back to 0 after a nonzero one), failing the
// "must be cumulative" assertion below. Reverted before committing.
func TestMetrics_LatencyHistogram_BucketsCumulativeOrdered_PlusInfPresent_SumCountConsistent(t *testing.T) {
	t.Parallel()
	cfg := newMetricsTestConfig()
	h, gw := newMetricsGatewayHandle(t, cfg)

	durations := []time.Duration{
		50 * time.Millisecond,  // <= 0.1 bucket
		400 * time.Millisecond, // <= 0.5 bucket
		3 * time.Second,        // <= 5 bucket
		20 * time.Second,       // <= 30 bucket
		1000 * time.Second,     // past every bound: overflow / +Inf
	}
	for _, d := range durations {
		gw.recordLatency("openai", "", latencySample{duration: d, ttfb: d / 10, hasTTFB: true, streaming: false})
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	_, samples := parsePrometheusText(t, rec.Body.Bytes())

	type bucketLine struct {
		le    float64
		value int64
	}
	var buckets []bucketLine
	var sumVal, countVal string
	for _, s := range samples {
		if s.labels["provider"] != "openai" || s.labels["stream"] != "false" {
			continue
		}
		switch s.name {
		case "llmgateway_upstream_duration_seconds_bucket":
			le := math.Inf(1)
			if s.labels["le"] != "+Inf" {
				var err error
				le, err = strconv.ParseFloat(s.labels["le"], 64)
				if err != nil {
					t.Fatalf("le=%q does not parse: %v", s.labels["le"], err)
				}
			}
			v, err := strconv.ParseInt(s.value, 10, 64)
			if err != nil {
				t.Fatalf("bucket value %q does not parse: %v", s.value, err)
			}
			buckets = append(buckets, bucketLine{le: le, value: v})
		case "llmgateway_upstream_duration_seconds_sum":
			sumVal = s.value
		case "llmgateway_upstream_duration_seconds_count":
			countVal = s.value
		}
	}

	if len(buckets) != len(latencyBucketBounds)+1 {
		t.Fatalf("got %d bucket lines, want %d (%d configured bounds + Inf)", len(buckets), len(latencyBucketBounds)+1, len(latencyBucketBounds))
	}
	for i := 1; i < len(buckets); i++ {
		if buckets[i].le <= buckets[i-1].le {
			t.Errorf("bucket %d le=%v is not strictly greater than bucket %d le=%v — buckets must be strictly ascending", i, buckets[i].le, i-1, buckets[i-1].le)
		}
		if buckets[i].value < buckets[i-1].value {
			t.Errorf("bucket %d value=%d < bucket %d value=%d — buckets must be cumulative (non-decreasing)", i, buckets[i].value, i-1, buckets[i-1].value)
		}
	}
	if !math.IsInf(buckets[len(buckets)-1].le, 1) {
		t.Errorf("last bucket le=%v, want +Inf", buckets[len(buckets)-1].le)
	}
	if sumVal == "" {
		t.Fatal("no _sum sample found for this label set")
	}
	if countVal == "" {
		t.Fatal("no _count sample found for this label set")
	}
	wantCount := int64(len(durations))
	if buckets[len(buckets)-1].value != wantCount {
		t.Errorf("final cumulative (+Inf) bucket = %d, want %d (one per seeded observation, including the overflow one)", buckets[len(buckets)-1].value, wantCount)
	}
	if countVal != strconv.FormatInt(buckets[len(buckets)-1].value, 10) {
		t.Errorf("_count = %s, want %d — must exactly equal the final +Inf cumulative bucket, never drift from it", countVal, buckets[len(buckets)-1].value)
	}
}

// TestMetrics_LatencyHistogram_StreamingAndNonStreaming_DistinctLabelSets
// drives one streaming and one non-streaming request through the real
// unified route (not a direct seed) and asserts both land in distinct
// stream="true"/stream="false" series for the same provider — proving
// the end-to-end wiring (isEventStreamResponse's derivation at
// newWatchdogBody's call sites, through to recordLatency's dual key)
// actually separates them, not just that the renderer could in
// principle.
//
// MUTATION VERIFIED: hardcoding isEventStreamResponse (timeout.go) to
// always return false made this test fail — no
// llmgateway_upstream_duration_seconds_count{...,stream="true"} sample
// was ever emitted, since every response (including the real SSE one)
// was misclassified as non-streaming. Reverted before committing.
func TestMetrics_LatencyHistogram_StreamingAndNonStreaming_DistinctLabelSets(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if streaming, _ := body["stream"].(bool); streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
			fl.Flush()
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	cfg := newMetricsTestConfig()
	cfg.Providers["openai"].BaseURL = srv.URL
	h, _ := newMetricsGatewayHandle(t, cfg)

	nonStreamBody := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	streamBody := map[string]any{"model": "gpt-test", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", nonStreamBody))
	if rec1.Code != http.StatusOK {
		t.Fatalf("non-streaming request status = %d, want 200, body=%s", rec1.Code, rec1.Body.String())
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", streamBody))
	if rec2.Code != http.StatusOK {
		t.Fatalf("streaming request status = %d, want 200, body=%s", rec2.Code, rec2.Body.String())
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, metricsRequest(http.MethodGet, metricsPathDefault, "sk-admin1"))
	_, samples := parsePrometheusText(t, rec.Body.Bytes())

	var sawStreamTrue, sawStreamFalse bool
	for _, s := range samples {
		if s.name != "llmgateway_upstream_duration_seconds_count" || s.labels["provider"] != "openai" {
			continue
		}
		switch s.labels["stream"] {
		case "true":
			sawStreamTrue = true
		case "false":
			sawStreamFalse = true
		}
	}
	if !sawStreamTrue {
		t.Error(`no llmgateway_upstream_duration_seconds_count{provider="openai",stream="true"} sample — the streaming request must land in the stream="true" label set`)
	}
	if !sawStreamFalse {
		t.Error(`no llmgateway_upstream_duration_seconds_count{provider="openai",stream="false"} sample — the non-streaming request must land in the stream="false" label set`)
	}
}

// TestRecordLatency_MetricsDisabled_NoObservationCollected proves the
// "no collection cost, no series emitted" requirement at the
// Gateway/collection level (not merely "the renderer skips an empty
// store", which would be true even if collection were silently still
// happening): with Config.Metrics.Enabled false, driving a real,
// successful request through the unified route must leave g.latency
// completely empty — every withLatencyRecorder call site
// (routes_unified.go, routes_media.go, routes_passthrough.go) gates its
// own wiring on metricsEnabled(g.cfg), so no latencyRecorder is ever
// looked up from context in the first place, and armLatency is never
// called at all.
//
// MUTATION VERIFIED: removing the `if metricsEnabled(g.cfg)` guard
// around runUnified's own withLatencyRecorder wiring (routes_unified.go)
// made this test fail — gw.latency.snapshot() returned 1 entry instead
// of 0. Reverted before committing.
func TestRecordLatency_MetricsDisabled_NoObservationCollected(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	cfg := newMetricsTestConfig()
	cfg.Metrics.Enabled = false // metrics collection disabled, but Metrics itself stays non-nil
	cfg.Providers["openai"].BaseURL = srv.URL
	h, gw := newMetricsGatewayHandle(t, cfg)

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	if snaps := gw.latency.snapshot(); len(snaps) != 0 {
		t.Errorf("g.latency has %d entries after a request with metrics disabled, want 0 — no latency observation should ever be collected when metrics is off", len(snaps))
	}
}
