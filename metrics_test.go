package traefikllmgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
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
	h, _ := newMetricsGatewayHandle(t, cfg)

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
		kind, ok := types[s.name]
		if !ok {
			t.Errorf("sample %q has no # TYPE line", s.name)
		}
		if kind == "counter" && !strings.HasSuffix(s.name, "_total") {
			t.Errorf("counter %q does not end in _total", s.name)
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
