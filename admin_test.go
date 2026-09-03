package traefikllmgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newAdminTestConfig builds a Config with two providers ("zeta", "alpha" —
// deliberately out of sorted order, to prove snapshot sorting), two
// groups ("zgroup", "agroup", same reason), a non-admin user in one group
// and an admin user in the other, and Admin.Enabled true. Callers may
// mutate the returned Config before calling New.
func newAdminTestConfig() *Config {
	cfg := CreateConfig()
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{
		"zeta":  {Type: "openai", BaseURL: "http://zeta.invalid", APIKey: "sk-zeta", Models: []string{"z-model"}},
		"alpha": {Type: "openai", BaseURL: "http://alpha.invalid", APIKey: "sk-alpha", Models: []string{"a-model-1", "a-model-2"}},
	}
	cfg.Groups = map[string]*GroupConfig{
		"zgroup": {Limits: &LimitsConfig{RequestsPerDay: 500}},
		"agroup": {},
	}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "agroup", APIKey: "sk-alice", Limits: &LimitsConfig{RequestsPerDay: 100}},
		{Name: "admin1", Group: "zgroup", APIKey: "sk-admin1", Admin: true},
	}}
	return cfg
}

// newAdminGatewayHandle builds the Gateway from cfg and returns both the
// http.Handler ServeHTTP uses and the concrete *Gateway for tests that
// need to reach into its internals (limiter, registry, auth).
func newAdminGatewayHandle(t *testing.T, cfg *Config) (http.Handler, *Gateway) {
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
	// SHOULD-5 (v0.22 review round): production spawns recordProviderAttempt's
	// store write in its own goroutine, off a request's TTFB path — every
	// admin_test.go test that asserts on a resulting provider counter (GET
	// /admin/api/overview's Attempts*/Failures* fields) needs it to have
	// already landed, so spawn runs synchronously here rather than per-test
	// (the deterministic-test half of that dependency-injection field; see
	// limiter.spawn's own doc comment, limits.go).
	gw.limiter.spawn = func(f func()) { f() }
	return h, gw
}

func adminRequest(method, path, apiKey string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req
}

// --- gate: disabled/unauth/non-admin/admin (spec §4) ---

func TestAdmin_Disabled_FallsThroughTo404(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Admin = nil // not registered at all
	h, _ := newAdminGatewayHandle(t, cfg)

	for _, p := range []string{adminPagePath, adminOverviewPath, adminUsagePath, adminTargetsPath} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(http.MethodGet, p, ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %q: status = %d, want 404 (admin nil)", p, rec.Code)
		}
	}
}

func TestAdmin_DisabledExplicitly_FallsThroughTo404(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Admin = &AdminConfig{Enabled: false}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminPagePath, "sk-admin1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (admin disabled, even with a valid admin key)", rec.Code)
	}
}

func TestAdmin_Disabled_HonorsPassthroughUnknown(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Admin = nil
	cfg.PassthroughUnknown = true
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminPagePath, ""))
	// The stub "next" handler (newAdminGatewayHandle) answers 418 — proves
	// /admin got no special-casing at all when disabled, it fell through
	// exactly like any other unregistered path.
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 from next handler (passthroughUnknown)", rec.Code)
	}
}

// TestAdminPage_ServedWithoutAuth is the controller-approved amendment to
// spec §4 (2026-08-20 review): GET /admin serves the HTML shell with no
// authentication at all, for every caller — a browser navigating
// straight to the URL has no way to attach a custom Authorization or
// x-api-key header, so gating the page itself made the dashboard
// unreachable from a browser. /admin/api/* stay fully gated (see
// TestAdmin_GateMatrix below); the page carries no data of its own, so
// serving it unauthenticated leaks nothing — verified here by scanning
// the body for the same canary-shaped check TestAdmin_SecretRedaction
// uses, not just trusting the doc comment.
func TestAdminPage_ServedWithoutAuth(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	for _, apiKey := range []string{"", "sk-alice", "sk-admin1", "not-a-real-key"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(http.MethodGet, adminPagePath, apiKey))
		if rec.Code != http.StatusOK {
			t.Errorf("apiKey=%q: status = %d, want 200 (GET /admin is unauthenticated)", apiKey, rec.Code)
		}
		for _, secret := range []string{"sk-zeta", "sk-alpha", "sk-alice", "sk-admin1"} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Errorf("apiKey=%q: page body leaks configured secret %q", apiKey, secret)
			}
		}
	}
}

// TestAdmin_GateMatrix covers the /admin/api/* routes only — GET /admin
// itself is unauthenticated by design (TestAdminPage_ServedWithoutAuth
// above).
func TestAdmin_GateMatrix(t *testing.T) {
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	paths := []string{adminOverviewPath, adminUsagePath, adminTargetsPath}
	for _, p := range paths {
		t.Run(p+"/unauthenticated", func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, p, ""))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			assertOAIErrorEnvelope(t, rec)
		})
		t.Run(p+"/authenticated_non_admin", func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, p, "sk-alice"))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			assertOAIErrorEnvelope(t, rec)
		})
		t.Run(p+"/admin", func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, p, "sk-admin1"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// assertOAIErrorEnvelope checks rec's body decodes as writeOAIError's
// envelope shape.
func assertOAIErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, rec.Body.String())
	}
	if body.Error.Message == "" {
		t.Error("error envelope has empty message")
	}
}

// --- overview: shape, sorting, provider snapshot correctness ---

func TestAdminOverview_SortedShapeAndVersion(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	// Two aliases, deliberately out of sorted order, proving both the
	// overview's aliases table (spec §5, v0.2) is populated AND sorted by
	// alias — "zaliased/x" is a bare target ("z-model", zeta's own
	// explicit model), "aaliased/y" is provider-prefixed.
	cfg.ModelAliases = map[string]string{
		"zaliased/x": "z-model",
		"aaliased/y": "alpha/a-model-1",
	}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Assert shape, not identity: comparing against the pluginVersion const
	// itself is tautological (it would pass even if the overview handler
	// stopped reading pluginVersion at all, as long as it echoed some other
	// copy of the same string). The real invariant is what version.go's
	// stamping contract promises: either the unstamped dev sentinel, or a
	// release semver a build stamped in.
	versionShape := regexp.MustCompile(`^(0\.0\.0-dev|\d+\.\d+\.\d+)$`)
	if got.Version == "" || !versionShape.MatchString(got.Version) {
		t.Errorf("version = %q, want non-empty and matching %s", got.Version, versionShape)
	}

	if len(got.Providers) != 2 || got.Providers[0].Name != "alpha" || got.Providers[1].Name != "zeta" {
		t.Fatalf("providers not sorted by name: %+v", got.Providers)
	}
	if got.Providers[0].Type != "openai" || got.Providers[0].BaseURL != "http://alpha.invalid" {
		t.Errorf("alpha provider view = %+v, want type openai baseUrl http://alpha.invalid", got.Providers[0])
	}
	if got.Providers[0].ModelCount != 2 {
		t.Errorf("alpha modelCount = %d, want 2 (a-model-1, a-model-2)", got.Providers[0].ModelCount)
	}
	if got.Providers[1].ModelCount != 1 {
		t.Errorf("zeta modelCount = %d, want 1 (z-model)", got.Providers[1].ModelCount)
	}
	// models (provider-model-accordion task): the sorted id list backing
	// modelCount above, not just its length.
	wantAlphaModels := []string{"a-model-1", "a-model-2"}
	if !slices.Equal(got.Providers[0].Models, wantAlphaModels) {
		t.Errorf("alpha models = %v, want sorted %v", got.Providers[0].Models, wantAlphaModels)
	}
	wantZetaModels := []string{"z-model"}
	if !slices.Equal(got.Providers[1].Models, wantZetaModels) {
		t.Errorf("zeta models = %v, want %v", got.Providers[1].Models, wantZetaModels)
	}
	if got.Providers[0].LastErr != "" || got.Providers[1].LastErr != "" {
		t.Errorf("providers with discovery disabled must have empty lastErr, got %+v", got.Providers)
	}

	if len(got.Groups) != 2 || got.Groups[0].Name != "agroup" || got.Groups[1].Name != "zgroup" {
		t.Fatalf("groups not sorted by name: %+v", got.Groups)
	}
	if got.Groups[1].MemberCount != 1 { // admin1
		t.Errorf("zgroup memberCount = %d, want 1", got.Groups[1].MemberCount)
	}
	if got.Groups[0].MemberCount != 1 { // alice
		t.Errorf("agroup memberCount = %d, want 1", got.Groups[0].MemberCount)
	}
	if got.Groups[1].Limits == nil || got.Groups[1].Limits.RequestsPerDay != 500 {
		t.Errorf("zgroup limits = %+v, want RequestsPerDay 500", got.Groups[1].Limits)
	}

	if got.Cache.Enabled {
		t.Error("cache.enabled must be false: cfg.Cache was never configured")
	}
	if got.Redis.Configured {
		t.Error("redis.configured must be false: cfg.Redis was never configured")
	}
	if got.Retry.Enabled {
		t.Error("retry.enabled must be false: cfg.Retry was never configured")
	}
	if got.Retry.Attempts != 0 || got.Retry.Backoff != "" {
		t.Errorf("retry.attempts/backoff must be omitted when disabled, got %+v", got.Retry)
	}

	wantAliases := []adminAliasView{
		{Alias: "aaliased/y", Target: "alpha/a-model-1"},
		{Alias: "zaliased/x", Target: "z-model"},
	}
	if len(got.Aliases) != len(wantAliases) {
		t.Fatalf("aliases = %+v, want exactly %+v", got.Aliases, wantAliases)
	}
	for i, want := range wantAliases {
		if got.Aliases[i] != want {
			t.Errorf("aliases[%d] = %+v, want %+v (sorted by alias)", i, got.Aliases[i], want)
		}
	}
}

// TestAdminOverview_RetryEffectiveValues proves GET /admin/api/overview's
// retry block reports the EFFECTIVE, defaulted values newRetryPolicy
// (retry.go) resolves an enabled retry block to, not the raw config —
// Attempts and Backoff are both left at their zero value here specifically
// to exercise that defaulting (v0.2 final review wave, 2026-08-20).
func TestAdminOverview_RetryEffectiveValues(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Retry = RetryConfig{Enabled: true}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Retry.Enabled {
		t.Fatal("retry.enabled must be true")
	}
	if got.Retry.Attempts != defaultRetryAttempts {
		t.Errorf("retry.attempts = %d, want the default %d (raw config left Attempts at 0)", got.Retry.Attempts, defaultRetryAttempts)
	}
	if got.Retry.Backoff != defaultRetryBackoff {
		t.Errorf("retry.backoff = %q, want the default %q (raw config left Backoff empty)", got.Retry.Backoff, defaultRetryBackoff)
	}
}

func TestAdminOverview_ProviderLastErrAfterFailedRefresh(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{
		"broken": {Type: "openai", BaseURL: srv.URL, APIKey: "sk", Discovery: true},
	}
	cfg.Groups = map[string]*GroupConfig{"g": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "admin1", Group: "g", APIKey: "sk-admin1", Admin: true}}}

	// newGateway's warmFill runs the first discovery fetch synchronously
	// during New, so the failure is already recorded before ServeHTTP is
	// ever called.
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("providers = %+v, want 1 entry", got.Providers)
	}
	p := got.Providers[0]
	if p.LastErr == "" {
		t.Error("lastErr must be non-empty after a failed discovery refresh")
	}
	if p.ModelCount != 0 {
		t.Errorf("modelCount = %d, want 0 (no explicit models, discovery failed)", p.ModelCount)
	}
	if len(p.Models) != 0 {
		t.Errorf("models = %v, want empty (no explicit models, discovery has never once succeeded)", p.Models)
	}
	if p.Models == nil {
		t.Error("models must marshal as [] (a non-nil empty slice), not be omitted or null")
	}
	if p.LastRefresh.IsZero() {
		t.Error("lastRefresh must be set even on a failed refresh (finishRefresh always advances it)")
	}
}

// --- overview: Feature A (v0.22) per-provider/per-model success-rate accounting ---

// TestAdminOverview_ProviderRates_NoTraffic_AllZeroWithModelRatesPopulated
// proves a provider with no traffic yet reports every Attempts*/Failures*
// field at 0 (the webui's "no traffic" state, not a misleadingly perfect
// 100%), and ModelRates carries one zero-valued entry per configured
// model — never an empty/omitted map, mirroring Models' own convention.
func TestAdminOverview_ProviderRates_NoTraffic_AllZeroWithModelRatesPopulated(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, p := range got.Providers {
		if p.AttemptsDay != 0 || p.FailuresDay != 0 || p.AttemptsMinute != 0 || p.FailuresMinute != 0 {
			t.Errorf("provider %q rates = %+v, want all-zero (no traffic yet)", p.Name, p)
		}
		if p.ModelRates == nil {
			t.Errorf("provider %q ModelRates is nil, want a non-nil map (marshals as {})", p.Name)
		}
		for _, model := range p.Models {
			mr, ok := p.ModelRates[model]
			if !ok {
				t.Errorf("provider %q ModelRates missing entry for model %q", p.Name, model)
				continue
			}
			if mr != (adminModelRateView{}) {
				t.Errorf("provider %q model %q rates = %+v, want all-zero", p.Name, model, mr)
			}
		}
	}
}

// TestAdminOverview_DiscoveryEnabledFlag proves adminProviderView.
// DiscoveryEnabled mirrors each provider's own configured Discovery flag —
// Feature B (v0.22): the webui needs this to tell "discovery is off" apart
// from "discovery is on but has not refreshed yet", both of which
// otherwise show the same zero LastRefresh.
func TestAdminOverview_DiscoveryEnabledFlag(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{
		"discoverable": {Type: "openai", BaseURL: srv.URL, APIKey: "sk", Discovery: true},
		"pinned":       {Type: "openai", BaseURL: srv.URL, APIKey: "sk", Models: []string{"pinned-model"}},
	}
	cfg.Groups = map[string]*GroupConfig{"g": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "admin1", Group: "g", APIKey: "sk-admin1", Admin: true}}}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Providers) != 2 {
		t.Fatalf("providers = %+v, want 2 entries", got.Providers)
	}
	for _, p := range got.Providers {
		switch p.Name {
		case "discoverable":
			if !p.DiscoveryEnabled {
				t.Errorf("provider %q discoveryEnabled = false, want true", p.Name)
			}
		case "pinned":
			if p.DiscoveryEnabled {
				t.Errorf("provider %q discoveryEnabled = true, want false", p.Name)
			}
		default:
			t.Fatalf("unexpected provider %q", p.Name)
		}
	}
}

// TestAdminOverview_ProviderRates_EndToEndAfterTraffic drives one
// successful and one provider-fault (500) chat completion through the
// SAME (provider, model), then asserts GET /admin/api/overview reports
// exactly 2 attempts and 1 failure at both provider and model scope — end
// to end from runUnified's attemptRecorder wiring (routes_unified.go)
// through retryPolicy.do (retry.go) to recordProviderAttempt (limits.go)
// to buildAdminOverview's own batched read (admin.go).
func TestAdminOverview_ProviderRates_EndToEndAfterTraffic(t *testing.T) {
	t.Parallel()
	var callN int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&callN, 1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
		{Name: "admin1", Group: "default", APIKey: "sk-admin1", Admin: true},
	}}
	h, _ := newAdminGatewayHandle(t, cfg)

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for i := 0; i < 2; i++ {
		req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("providers = %+v, want 1 entry", got.Providers)
	}
	p := got.Providers[0]
	if p.AttemptsDay != 2 || p.FailuresDay != 1 {
		t.Errorf("provider attempts/failures = %d/%d, want 2/1", p.AttemptsDay, p.FailuresDay)
	}
	if p.AttemptsMinute != 2 || p.FailuresMinute != 1 {
		t.Errorf("provider attempts/failures (minute) = %d/%d, want 2/1", p.AttemptsMinute, p.FailuresMinute)
	}
	mr, ok := p.ModelRates["gpt-test"]
	if !ok {
		t.Fatalf("modelRates missing entry for gpt-test: %+v", p.ModelRates)
	}
	if mr.AttemptsDay != 2 || mr.FailuresDay != 1 {
		t.Errorf("model attempts/failures = %d/%d, want 2/1", mr.AttemptsDay, mr.FailuresDay)
	}
}

// TestAdminOverview_LatencySummary_AvgReflectsSeededObservations proves
// buildAdminLatencyViews' own math (admin.go, feat: instrument upstream
// latency): plain averages (sum/count), split by stream state, and that
// the opt-in per-model dimension never leaks into this compact admin
// surface (the task brief's own "compact... do not bloat" requirement).
//
// alpha's observation is seeded directly via gw.latency.record under a
// MODEL-keyed latencyKey only — bypassing recordLatency's own
// dual-write (which always also writes the bare provider+stream key,
// tested separately by metrics_test.go's TestMetrics_ModelLabel_*
// family) — specifically so no base (model="") entry exists for
// alpha's "streaming" state at all. That isolates buildAdminLatencyViews'
// own model-exclusion filter: if it ever stopped skipping model-keyed
// entries, THIS is the one that would leak through as a phantom row,
// since there is no correctly-shaped base entry that could coincidentally
// produce the same numbers instead (unlike a same-provider dual-write,
// where the base and model entries are byte-identical and an overwrite
// would go unnoticed).
//
// MUTATION VERIFIED: removing the `if s.key.model != "" ... continue`
// guard from buildAdminLatencyViews (admin.go) made this test fail —
// alpha.Latency gained a "streaming" entry (Count=1, AvgDurationMs=5000)
// that must never exist, since alpha never received any base-key
// observation. Reverted before committing.
func TestAdminOverview_LatencySummary_AvgReflectsSeededObservations(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)

	// zeta, non-streaming: two base-key observations, 100ms and 300ms —
	// average 200ms.
	gw.recordLatency("zeta", "", latencySample{duration: 100 * time.Millisecond, ttfb: 100 * time.Millisecond, hasTTFB: true, streaming: false})
	gw.recordLatency("zeta", "", latencySample{duration: 300 * time.Millisecond, ttfb: 300 * time.Millisecond, hasTTFB: true, streaming: false})

	// alpha, streaming: model-keyed only (see doc comment above).
	gw.latency.record(latencyKey{provider: "alpha", streaming: true, model: "a-model-1"},
		latencySample{duration: 5 * time.Second, ttfb: time.Second, hasTTFB: true, streaming: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var zeta, alpha *adminProviderView
	for i := range got.Providers {
		switch got.Providers[i].Name {
		case "zeta":
			zeta = &got.Providers[i]
		case "alpha":
			alpha = &got.Providers[i]
		}
	}
	if zeta == nil || alpha == nil {
		t.Fatalf("providers = %+v, want entries named zeta and alpha", got.Providers)
	}

	nonStream, ok := zeta.Latency["non-streaming"]
	if !ok {
		t.Fatalf("zeta.Latency missing \"non-streaming\" entry: %+v", zeta.Latency)
	}
	if nonStream.Count != 2 {
		t.Errorf("non-streaming Count = %d, want 2", nonStream.Count)
	}
	if math.Abs(nonStream.AvgDurationMs-200) > 0.01 {
		t.Errorf("non-streaming AvgDurationMs = %v, want 200 ((100+300)/2)", nonStream.AvgDurationMs)
	}
	if math.Abs(nonStream.AvgTTFBMs-200) > 0.01 {
		t.Errorf("non-streaming AvgTTFBMs = %v, want 200", nonStream.AvgTTFBMs)
	}

	if _, ok := alpha.Latency["streaming"]; ok {
		t.Errorf(`alpha.Latency has a "streaming" entry despite the only observation being model-keyed (never a base provider+stream one): %+v — the opt-in per-model dimension must never leak into this compact admin summary`, alpha.Latency)
	}
}

// TestAdminOverview_ProvenanceSummary_ExcludesReportedIncludesEstimatedAndUnbilled
// proves buildAdminProvenanceViews' own filtering (admin.go, feat:
// expose token-accounting provenance): "reported" observations never
// surface on this compact admin summary (the operator already sees
// AttemptsDay/FailuresDay for the healthy default), while "estimated"
// and "unbilled" both do, as DISTINCT keys — never merged.
//
// MUTATION VERIFIED: removing the `if s.key.provenance ==
// provenanceReported { continue }` guard from buildAdminProvenanceViews
// (admin.go) made this test fail — zeta.Provenance gained a leaked
// "reported" entry (Requests=1, Tokens=15) that must never appear here.
// Reverted before committing.
func TestAdminOverview_ProvenanceSummary_ExcludesReportedIncludesEstimatedAndUnbilled(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)

	// zeta: one reported observation (must never surface below) and one
	// estimated observation.
	gw.provenance.record("zeta", provenanceReported, 15)
	gw.provenance.record("zeta", provenanceEstimated, 7)

	// alpha: two unbilled observations, seeded separately to prove
	// requests accumulates across calls rather than overwriting.
	gw.provenance.record("alpha", provenanceUnbilled, 0)
	gw.provenance.record("alpha", provenanceUnbilled, 0)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var zeta, alpha *adminProviderView
	for i := range got.Providers {
		switch got.Providers[i].Name {
		case "zeta":
			zeta = &got.Providers[i]
		case "alpha":
			alpha = &got.Providers[i]
		}
	}
	if zeta == nil || alpha == nil {
		t.Fatalf("providers = %+v, want entries named zeta and alpha", got.Providers)
	}

	if _, ok := zeta.Provenance[provenanceReported]; ok {
		t.Errorf(`zeta.Provenance has a %q entry: %+v — "reported" must never surface on this compact admin summary`, provenanceReported, zeta.Provenance)
	}
	est, ok := zeta.Provenance[provenanceEstimated]
	if !ok {
		t.Fatalf("zeta.Provenance missing %q entry: %+v", provenanceEstimated, zeta.Provenance)
	}
	if est.Requests != 1 || est.Tokens != 7 {
		t.Errorf("zeta estimated = %+v, want {Requests:1 Tokens:7}", est)
	}

	unb, ok := alpha.Provenance[provenanceUnbilled]
	if !ok {
		t.Fatalf("alpha.Provenance missing %q entry: %+v", provenanceUnbilled, alpha.Provenance)
	}
	if unb.Requests != 2 || unb.Tokens != 0 {
		t.Errorf("alpha unbilled = %+v, want {Requests:2 Tokens:0}", unb)
	}
	if _, ok := alpha.Provenance[provenanceEstimated]; ok {
		t.Errorf(`alpha.Provenance has a %q entry despite only unbilled observations being seeded: %+v — estimated and unbilled must never conflate`, provenanceEstimated, alpha.Provenance)
	}
}

// --- usage: math against seeded counters ---

func TestAdminUsage_MathAgainstSeededCounters(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	// Seed alice's (user) and agroup's (group) counters directly, bypassing
	// checkAndCount/account's own request-shaped call pattern so the
	// seeded values are exact and independent of admin1's own traffic.
	gw.limiter.incrCounter("user", "alice", metricReq, windowMin, fixedNow, 3, minWindowTTL)
	gw.limiter.incrCounter("user", "alice", metricReq, windowDay, fixedNow, 7, dayWindowTTL)
	gw.limiter.account([]limitScope{{kind: "user", id: "alice"}}, usage{prompt: 40, completion: 10}, 2_500_000)

	gw.limiter.incrCounter("group", "agroup", metricReq, windowMin, fixedNow, 5, minWindowTTL)
	gw.limiter.incrCounter("group", "agroup", metricReq, windowDay, fixedNow, 9, dayWindowTTL)
	gw.limiter.account([]limitScope{{kind: "group", id: "agroup"}}, usage{prompt: 100, completion: 50}, 9_000_000)

	// Seed the synthetic total scope directly too (v0.2 data-layer task):
	// buildAdminUsage always includes it, independent of whether any
	// route's withTotalScope call ever ran in this test.
	gw.limiter.incrCounter(totalScopeKind, totalScopeID, metricReq, windowDay, fixedNow, 16, dayWindowTTL)
	gw.limiter.account([]limitScope{{kind: totalScopeKind, id: totalScopeID}}, usage{prompt: 140, completion: 60}, 11_500_000)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsagePath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got adminUsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	alice := findUsageEntry(t, got.Users, "alice")
	if alice.RequestsPerMinute != 3 || alice.RequestsPerDay != 7 {
		t.Errorf("alice requests = min:%d day:%d, want min:3 day:7", alice.RequestsPerMinute, alice.RequestsPerDay)
	}
	if alice.TokensInPerDay != 40 || alice.TokensOutPerDay != 10 || alice.TokensInPerMonth != 40 || alice.TokensOutPerMonth != 10 {
		t.Errorf("alice tokens = in/day:%d out/day:%d in/month:%d out/month:%d, want 40/10/40/10",
			alice.TokensInPerDay, alice.TokensOutPerDay, alice.TokensInPerMonth, alice.TokensOutPerMonth)
	}
	if alice.CostPerDayMicroUSD != 2_500_000 || alice.CostPerMonthMicroUSD != 2_500_000 {
		t.Errorf("alice cost = day:%d month:%d, want 2500000/2500000", alice.CostPerDayMicroUSD, alice.CostPerMonthMicroUSD)
	}
	if alice.Limits == nil || alice.Limits.RequestsPerDay != 100 {
		t.Errorf("alice limits = %+v, want RequestsPerDay 100 echoed", alice.Limits)
	}
	if alice.GroupName != "agroup" {
		t.Errorf("alice groupName = %q, want agroup", alice.GroupName)
	}
	if alice.StoreDown {
		t.Error("alice.storeDown must be false")
	}

	agroup := findUsageEntry(t, got.Groups, "agroup")
	if agroup.RequestsPerMinute != 5 || agroup.RequestsPerDay != 9 {
		t.Errorf("agroup requests = min:%d day:%d, want min:5 day:9", agroup.RequestsPerMinute, agroup.RequestsPerDay)
	}
	if agroup.TokensInPerDay != 100 || agroup.TokensOutPerDay != 50 || agroup.TokensInPerMonth != 100 || agroup.TokensOutPerMonth != 50 {
		t.Errorf("agroup tokens = in/day:%d out/day:%d in/month:%d out/month:%d, want 100/50/100/50",
			agroup.TokensInPerDay, agroup.TokensOutPerDay, agroup.TokensInPerMonth, agroup.TokensOutPerMonth)
	}
	if agroup.CostPerDayMicroUSD != 9_000_000 {
		t.Errorf("agroup cost/day = %d, want 9000000", agroup.CostPerDayMicroUSD)
	}

	// admin1 itself must also be listed (buildAdminUsage lists every
	// active user/group, not just ones with seeded traffic): this test
	// never seeds any counter for admin1 (only alice/agroup/total above),
	// so its row reads genuinely zero — but the row must still be present.
	// This is unrelated to buildLimitScopes (routes_unified.go): since the
	// v0.21 accounting fix, that function always builds a scope regardless
	// of limits, and separately, handleAdminAPI itself never calls
	// checkAndCount for ANY admin route caller (admin.go's own doc
	// comment) — admin1's zero here is simply "no traffic was ever
	// generated for it in this test".
	admin1 := findUsageEntry(t, got.Users, "admin1")
	if admin1.RequestsPerMinute != 0 {
		t.Errorf("admin1 requestsPerMinute = %d, want 0 (no traffic was seeded for admin1 in this test)", admin1.RequestsPerMinute)
	}
	if admin1.StoreDown {
		t.Error("admin1.storeDown must be false")
	}

	// The synthetic total scope row (v0.2 data-layer task): present,
	// carries no limit (limits.go's totalScopeKind is never evaluated),
	// and reflects exactly the counters seeded above.
	if got.Total.Kind != totalScopeKind || got.Total.ID != totalScopeID {
		t.Errorf("total kind/id = %q/%q, want %q/%q", got.Total.Kind, got.Total.ID, totalScopeKind, totalScopeID)
	}
	if got.Total.Limits != nil {
		t.Errorf("total limits = %+v, want nil (the total scope is never evaluated)", got.Total.Limits)
	}
	if got.Total.RequestsPerDay != 16 {
		t.Errorf("total requestsPerDay = %d, want 16", got.Total.RequestsPerDay)
	}
	if got.Total.TokensInPerDay != 140 || got.Total.TokensOutPerDay != 60 {
		t.Errorf("total tokens = in/day:%d out/day:%d, want 140/60", got.Total.TokensInPerDay, got.Total.TokensOutPerDay)
	}
	if got.Total.CostPerDayMicroUSD != 11_500_000 {
		t.Errorf("total cost/day = %d, want 11500000", got.Total.CostPerDayMicroUSD)
	}
	if got.Total.StoreDown {
		t.Error("total.storeDown must be false")
	}
}

func findUsageEntry(t *testing.T, entries []adminUsageEntryView, id string) adminUsageEntryView {
	t.Helper()
	for _, e := range entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("no usage entry for id %q in %+v", id, entries)
	return adminUsageEntryView{}
}

// TestAdminUsage_LimitlessUserAndGroup_AccumulateRealTraffic is the
// regression test for the production accounting bug (v0.21, root-caused
// live): buildLimitScopes (routes_unified.go) used to build a counter scope
// for a user or group ONLY when that entity had its own Limits configured,
// so a limit-less user/group never accumulated usage at all — the
// dashboard's total kept climbing while every such row stayed zero, until
// an operator worked around it by adding phantom, deliberately-unreachable
// requestsPerDay limits everywhere. "nolim" and "nolimgroup" here carry no
// Limits whatsoever (both nil, never set), yet after one real, end-to-end
// chat-completion request — driven through the actual unified route, not a
// direct counter seed — their rows in GET /admin/api/usage must already
// show real, non-zero request and token counters.
func TestAdminUsage_LimitlessUserAndGroup_AccumulateRealTraffic(t *testing.T) {
	t.Parallel()
	const respBody = `{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{
		"nolimgroup": {}, // no Limits configured
		"admingroup": {},
	}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "nolim", Group: "nolimgroup", APIKey: "sk-nolim"}, // no Limits configured
		{Name: "admin1", Group: "admingroup", APIKey: "sk-admin1", Admin: true},
	}}
	h, _ := newAdminGatewayHandle(t, cfg)

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	chatReq := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-nolim", body)
	chatRec := httptest.NewRecorder()
	h.ServeHTTP(chatRec, chatReq)
	if chatRec.Code != http.StatusOK {
		t.Fatalf("chat completion status = %d, want 200, body=%s", chatRec.Code, chatRec.Body.String())
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsagePath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin/api/usage status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminUsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	nolim := findUsageEntry(t, got.Users, "nolim")
	if nolim.Limits != nil {
		t.Errorf("nolim.Limits = %+v, want nil (no limits configured)", nolim.Limits)
	}
	if nolim.RequestsPerDay != 1 || nolim.RequestsPerMinute != 1 {
		t.Errorf("nolim requests = min:%d day:%d, want 1/1 (the bug this test guards: a limit-less user never accumulated usage)", nolim.RequestsPerMinute, nolim.RequestsPerDay)
	}
	if nolim.TokensInPerDay != 7 || nolim.TokensOutPerDay != 3 {
		t.Errorf("nolim tokens = in/day:%d out/day:%d, want 7/3", nolim.TokensInPerDay, nolim.TokensOutPerDay)
	}

	nolimgroup := findUsageEntry(t, got.Groups, "nolimgroup")
	if nolimgroup.Limits != nil {
		t.Errorf("nolimgroup.Limits = %+v, want nil (no limits configured)", nolimgroup.Limits)
	}
	if nolimgroup.RequestsPerDay != 1 || nolimgroup.RequestsPerMinute != 1 {
		t.Errorf("nolimgroup requests = min:%d day:%d, want 1/1 (the bug this test guards: a limit-less group never accumulated usage)", nolimgroup.RequestsPerMinute, nolimgroup.RequestsPerDay)
	}
	if nolimgroup.TokensInPerDay != 7 || nolimgroup.TokensOutPerDay != 3 {
		t.Errorf("nolimgroup tokens = in/day:%d out/day:%d, want 7/3", nolimgroup.TokensInPerDay, nolimgroup.TokensOutPerDay)
	}
}

// accessListKeys are adminUsageEntryView's four group-access JSON keys
// (admin.go) — the set TestAdminUsage_GroupAccessLists checks both at the
// typed-struct level and, since a nil Go slice and an absent JSON key
// decode identically into a struct field either way, at the raw-JSON
// level too (assertRawAccessList below): a struct-only assertion would
// stay green even if the "omitempty" json tags were removed, since
// encoding/json would then emit "null" for an unset field and
// json.Unmarshal reads a JSON null back into a nil []string all the same.
var accessListKeys = []string{"providers", "models", "mcpServers", "agents"}

// findRawUsageEntry finds row id's raw JSON object (one element of GET
// /admin/api/usage's users/groups array, or the total object) among rows —
// the raw-JSON counterpart to findUsageEntry, used where a typed-struct
// decode cannot distinguish "key absent" from "key present but null" (see
// accessListKeys' own doc comment).
func findRawUsageEntry(t *testing.T, rows []map[string]json.RawMessage, id string) map[string]json.RawMessage {
	t.Helper()
	for _, row := range rows {
		var rowID string
		if err := json.Unmarshal(row["id"], &rowID); err != nil {
			t.Fatalf("decode id: %v", err)
		}
		if rowID == id {
			return row
		}
	}
	t.Fatalf("no raw usage entry for id %q", id)
	return nil
}

// assertRawAccessList asserts one access-list key's raw JSON presence and
// value on row: want == nil means the key must be ENTIRELY ABSENT from the
// JSON object (not present-as-null, not present-as-"[]") — the omitempty
// contract adminUsageEntryView's doc comment promises; a non-nil want means
// the key must be present with exactly that value.
func assertRawAccessList(t *testing.T, row map[string]json.RawMessage, key string, want []string) {
	t.Helper()
	raw, present := row[key]
	if want == nil {
		if present {
			t.Errorf("key %q = %s, want entirely absent (unrestricted/non-group row)", key, raw)
		}
		return
	}
	if !present {
		t.Errorf("key %q absent, want present with value %v", key, want)
		return
	}
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode %q: %v", key, err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("key %q = %v, want %v", key, got, want)
	}
}

// TestAdminUsage_GroupAccessLists proves GET /admin/api/usage's per-group
// rows (group-access-display task) echo GroupConfig's Providers/Models/
// MCPServers/Agents exactly as configured — no server-side expansion to the
// full provider/model catalog — and that a group with none of those set
// carries the keys entirely absent (json:",omitempty") rather than present
// with a resolved "everything" set, matching group.allowsX's own
// empty-means-all contract (auth.go). A user row and the synthetic total
// row must never carry these fields at all — they are group-only, same
// convention as GroupName's own user-only field. Assertions run at both the
// typed-struct level (adminUsageResponse) and the raw-JSON level
// (assertRawAccessList) — the typed level alone cannot prove the field was
// actually omitted from the wire, only that it decoded to nil, which a
// present-but-null field would do identically.
func TestAdminUsage_GroupAccessLists(t *testing.T) {
	t.Parallel()
	cfg := CreateConfig()
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{
		"alpha": {Type: "openai", BaseURL: "http://alpha.invalid", APIKey: "sk-alpha", Models: []string{"a-model-1"}},
	}
	cfg.MCPServers = map[string]*TargetConfig{"srv1": {URL: "http://srv1.invalid"}}
	cfg.Agents = map[string]*AgentConfig{"agent1": {URL: "http://agent1.invalid"}}
	cfg.Groups = map[string]*GroupConfig{
		"restricted": {
			Providers:  []string{"alpha"},
			Models:     []string{"alpha/a-model-1", "z-model"},
			MCPServers: []string{"srv1"},
			Agents:     []string{"agent1"},
		},
		"open": {}, // no Providers/Models/MCPServers/Agents set: unrestricted
	}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "admin1", Group: "open", APIKey: "sk-admin1", Admin: true},
	}}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsagePath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminUsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var raw struct {
		Total  map[string]json.RawMessage   `json:"total"`
		Users  []map[string]json.RawMessage `json:"users"`
		Groups []map[string]json.RawMessage `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}

	tests := []struct {
		name           string
		group          string
		wantProviders  []string
		wantModels     []string
		wantMCPServers []string
		wantAgents     []string
	}{
		{
			name:           "restricted group carries its exact configured lists",
			group:          "restricted",
			wantProviders:  []string{"alpha"},
			wantModels:     []string{"alpha/a-model-1", "z-model"},
			wantMCPServers: []string{"srv1"},
			wantAgents:     []string{"agent1"},
		},
		{
			name:  "unrestricted group carries empty or omitted lists",
			group: "open",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := findUsageEntry(t, got.Groups, tc.group)
			if !slices.Equal(entry.Providers, tc.wantProviders) {
				t.Errorf("providers = %v, want %v", entry.Providers, tc.wantProviders)
			}
			if !slices.Equal(entry.Models, tc.wantModels) {
				t.Errorf("models = %v, want %v", entry.Models, tc.wantModels)
			}
			if !slices.Equal(entry.MCPServers, tc.wantMCPServers) {
				t.Errorf("mcpServers = %v, want %v", entry.MCPServers, tc.wantMCPServers)
			}
			if !slices.Equal(entry.Agents, tc.wantAgents) {
				t.Errorf("agents = %v, want %v", entry.Agents, tc.wantAgents)
			}

			rawEntry := findRawUsageEntry(t, raw.Groups, tc.group)
			assertRawAccessList(t, rawEntry, "providers", tc.wantProviders)
			assertRawAccessList(t, rawEntry, "models", tc.wantModels)
			assertRawAccessList(t, rawEntry, "mcpServers", tc.wantMCPServers)
			assertRawAccessList(t, rawEntry, "agents", tc.wantAgents)
		})
	}

	admin1 := findUsageEntry(t, got.Users, "admin1")
	if admin1.Providers != nil || admin1.Models != nil || admin1.MCPServers != nil || admin1.Agents != nil {
		t.Errorf("user entry must never carry group access lists, got providers=%v models=%v mcpServers=%v agents=%v",
			admin1.Providers, admin1.Models, admin1.MCPServers, admin1.Agents)
	}
	if got.Total.Providers != nil || got.Total.Models != nil || got.Total.MCPServers != nil || got.Total.Agents != nil {
		t.Errorf("total entry must never carry group access lists, got providers=%v models=%v mcpServers=%v agents=%v",
			got.Total.Providers, got.Total.Models, got.Total.MCPServers, got.Total.Agents)
	}

	rawAdmin1 := findRawUsageEntry(t, raw.Users, "admin1")
	rawTotal := raw.Total
	for _, key := range accessListKeys {
		assertRawAccessList(t, rawAdmin1, key, nil)
		assertRawAccessList(t, rawTotal, key, nil)
	}
}

// TestLimiterCurrentUsage_StoreDown drives limiter.currentUsage's
// fail-closed path directly: a configured store whose every operation
// errors, with failOpen false, must report storeDown and zero values
// rather than a silently wrong reading. alwaysErrStore/errStoreDownStub
// are defined in routes_unified_test.go.
func TestLimiterCurrentUsage_StoreDown(t *testing.T) {
	t.Parallel()
	l := newLimiter(alwaysErrStore{}, false)
	scopes := []limitScope{{kind: "user", id: "x", limits: &LimitsConfig{RequestsPerMinute: 10}}}

	got := l.currentUsage(scopes)
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	su := got[0]
	if !su.storeDown {
		t.Fatal("want storeDown true when the configured store errors and failOpen is false")
	}
	if su.requestsPerMinute != 0 || su.requestsPerDay != 0 || su.tokensInPerDay != 0 ||
		su.tokensInPerMonth != 0 || su.tokensOutPerDay != 0 || su.tokensOutPerMonth != 0 ||
		su.costPerDayMicros != 0 || su.costPerMonthMicros != 0 {
		t.Errorf("storeDown entry must report zero values, got %+v", su)
	}
}

// --- admin routes never rate-limit and never move usage counters (operator directive, 2026-08-21) ---

// TestAdmin_NeverRateLimitedAndCountersUntouched proves the operator
// directive (progress ledger, 2026-08-21 — "admin requests must NOT touch
// req/min-req/day statistics"): GET /admin/api/overview, GET
// /admin/api/usage, and GET /admin/api/targets never call checkAndCount,
// so an admin key with a requestsPerMinute limit tight enough to 429 on
// any other authenticated route never 429s here, no matter how many
// requests it makes — and its own req/min counter stays at exactly 0
// throughout. This inverts the pre-directive TestAdmin_RateLimitApplies,
// which asserted the opposite (a 2nd request 429ing); it mirrors
// TestAdminUsageHistory_DoesNotCountStats, which already proved this for
// the third admin route from the start.
func TestAdmin_NeverRateLimitedAndCountersUntouched(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Users.Inline[1].Limits = &LimitsConfig{RequestsPerMinute: 1} // admin1
	h, gw := newAdminGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	for _, p := range []string{adminOverviewPath, adminUsagePath, adminTargetsPath} {
		for i := 0; i < 5; i++ {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, p, "sk-admin1"))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s request %d: status = %d, want 200 (never 429), body=%s", p, i, rec.Code, rec.Body.String())
			}
		}
	}

	reqCount, ok := gw.limiter.getCounter("user", "admin1", metricReq, windowMin, fixedNow)
	if !ok || reqCount != 0 {
		t.Errorf("admin1 req/min counter = %d (ok=%v), want 0 (admin routes never call checkAndCount)", reqCount, ok)
	}
}

// --- HTML shell: CSP header, references the built Vue app, no external assets ---

// TestAdminPage_CSPHeaderAndFetchURLs proves GET /admin serves the built
// Vue app's generated index.html (admin_assets_gen.go) with the tightened
// CSP (script-src/style-src 'self', no 'unsafe-inline' — the vanilla-JS
// page's inline script/style are gone, replaced by an external
// /admin/assets/*.js and *.css the browser loads itself) and that the
// shell references those assets rather than embedding any logic inline.
// This replaces the pre-Vue version of this test, which asserted the
// opposite (inline script content like "setInterval"/"sessionStorage")
// and the old CSP string — see also TestAdminAssets_GeneratedFilePresent
// below for the generated-file sanity checks this test doesn't cover.
func TestAdminPage_CSPHeaderAndFetchURLs(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	// Unauthenticated: GET /admin is served without auth (see
	// TestAdminPage_ServedWithoutAuth), so no key is presented here.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminPagePath, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	wantCSP := "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("CSP = %q, want %q", got, wantCSP)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "LLM Gateway") {
		t.Error("admin page body does not look like the dashboard shell (missing \"LLM Gateway\")")
	}
	if !strings.Contains(body, adminAssetsPathPrefix) {
		t.Errorf("admin page body does not reference any %q asset", adminAssetsPathPrefix)
	}
	// script-src/style-src 'self' means every actual asset reference must be
	// a same-origin path, never an absolute http(s):// URL to another host.
	// The inlined favicon is the one place "http" legitimately appears at
	// all: a data: URI SVG whose xmlns attribute names the (never
	// dereferenced) XML namespace URL http://www.w3.org/2000/svg — not a
	// resource the browser fetches — so this checks specifically for a
	// src=/href= attribute pointing off-host, not a blanket substring ban.
	for _, attr := range []string{`src="http`, `src='http`, `href="http`, `href='http`} {
		if strings.Contains(body, attr) {
			t.Errorf("admin page loads an external asset via %s...", attr)
		}
	}
}

// --- static assets: content type, immutable cache header, 404 on an unknown name ---

// TestAdminAsset_Serving drives GET /admin/assets/{hashedname} against
// one real entry from admin_assets_gen.go's adminAssets map — proving the
// route serves the exact declared Content-Type, a long-lived `immutable`
// Cache-Control, and the exact bytes generate.mjs baked in, byte for
// byte.
func TestAdminAsset_Serving(t *testing.T) {
	t.Parallel()
	if len(adminAssets) == 0 {
		t.Fatal("adminAssets is empty — run `make admin-ui` before running this test")
	}
	var name string
	var asset adminAsset
	for name, asset = range adminAssets {
		break
	}

	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	// Unauthenticated, like GET /admin itself (handleAdmin's doc comment):
	// the browser must be able to load the page's own script/style before
	// it can ever attach an x-api-key header.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminAssetsPathPrefix+name, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != asset.contentType {
		t.Errorf("Content-Type = %q, want %q", got, asset.contentType)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != adminCSP {
		t.Errorf("Content-Security-Policy = %q, want %q", got, adminCSP)
	}
	cc := rec.Header().Get("Cache-Control")
	if !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
		t.Errorf("Cache-Control = %q, want a long-lived immutable directive", cc)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, asset.body) {
		t.Errorf("body length = %d, want %d (byte-for-byte match against adminAssets[%q])", len(got), len(asset.body), name)
	}
}

// TestAdminAsset_UnknownReturns404 proves an unrecognized asset name — a
// stale bookmark, a probe — is a 404 in the same OAI-shaped error
// envelope every other unknown route on this gateway returns, not a bare
// empty body.
func TestAdminAsset_UnknownReturns404(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminAssetsPathPrefix+"does-not-exist.js", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
	assertOAIErrorEnvelope(t, rec)
}

// --- generated-file sanity: present, non-empty, no leftover template markers ---

// TestAdminAssets_GeneratedFilePresent proves admin_assets_gen.go
// (webui/generate.mjs's output, committed to the repo) actually decoded
// into real content: a non-empty index.html shell and at least one
// non-empty static asset, with no stray Go-template placeholder
// ("{{", "}}") or generator-marker text ("Code generated") leaking into
// the decoded bytes — the strongest signal the base64 round-trip
// (generate.mjs's encode, decodeAdminAsset's decode) actually worked,
// rather than the map merely being non-empty by accident.
func TestAdminAssets_GeneratedFilePresent(t *testing.T) {
	t.Parallel()
	if len(adminIndexHTML) == 0 {
		t.Fatal("adminIndexHTML is empty — run `make admin-ui`")
	}
	if !strings.Contains(string(adminIndexHTML), "<!doctype html") {
		t.Errorf("adminIndexHTML does not look like an HTML document: %q", truncateForTest(adminIndexHTML, 120))
	}
	for _, marker := range []string{"{{", "}}", "Code generated"} {
		if strings.Contains(string(adminIndexHTML), marker) {
			t.Errorf("adminIndexHTML contains leftover template marker %q", marker)
		}
	}

	if len(adminAssets) == 0 {
		t.Fatal("adminAssets is empty — run `make admin-ui`")
	}
	for name, asset := range adminAssets {
		if len(asset.body) == 0 {
			t.Errorf("adminAssets[%q].body is empty", name)
		}
		if asset.contentType == "" {
			t.Errorf("adminAssets[%q].contentType is empty", name)
		}
	}
}

// adminInlineScriptRe matches every <script ...> opening tag, capturing its
// attribute text — hasInlineScript checks each capture against
// adminSrcAttrRe for a real src= attribute.
var adminInlineScriptRe = regexp.MustCompile(`(?i)<script\b([^>]*)>`)

// adminSrcAttrRe matches a real src= attribute, anchored to the start of an
// attribute (start-of-string or preceding whitespace) rather than a bare
// \b word boundary: \b alone matches inside "data-src=" too (the "-" before
// "src" is itself a non-word character, so \b sits right before "src"),
// which would wrongly treat a data-src/nosrc/whatever-src attribute as the
// real src= that makes a <script> external. Mirrors
// webui/generate.mjs's assertNoInlineAssets, which guards the identical
// invariant at build time.
var adminSrcAttrRe = regexp.MustCompile(`(?i)(^|\s)src\s*=`)

// hasInlineScript returns every <script> opening tag in html with no real
// src= attribute — an inline script, forbidden under the admin CSP's
// script-src 'self' (no 'unsafe-inline').
func hasInlineScript(html string) []string {
	var offenders []string
	for _, m := range adminInlineScriptRe.FindAllStringSubmatch(html, -1) {
		if !adminSrcAttrRe.MatchString(m[1]) {
			offenders = append(offenders, m[0])
		}
	}
	return offenders
}

// TestAdminIndexHTML_NoInlineAssets asserts, server-side, the exact CSP
// invariant generate.mjs's own assertNoInlineAssets checks at build time
// (webui/generate.mjs): the decoded adminIndexHTML — what admin.go's
// serveAdminPage actually serves — contains no inline <script> (a
// <script> tag with no src= attribute) and no <style> tag anywhere. The
// admin CSP (adminCSP, above) ships script-src/style-src 'self' with no
// 'unsafe-inline'; either violation would silently break under that
// policy. This test exists as a second, independent guard: it catches a
// regression even if admin_assets_gen.go were regenerated with a
// modified copy of generate.mjs that dropped or weakened its own check.
func TestAdminIndexHTML_NoInlineAssets(t *testing.T) {
	t.Parallel()
	html := string(adminIndexHTML)

	for _, tag := range hasInlineScript(html) {
		t.Errorf("adminIndexHTML contains an inline <script> (no src= attribute): %q", tag)
	}
	if strings.Contains(strings.ToLower(html), "<style") {
		t.Error("adminIndexHTML contains a <style> tag — every style must be an external <link rel=\"stylesheet\">")
	}
}

// TestHasInlineScript_DataSrcIsNotExternal is the negative case
// hasInlineScript's adminSrcAttrRe anchoring exists for: a <script
// data-src="..."> tag has no REAL src= attribute — data-src is a
// distinct, non-standard attribute name, not the src that makes a
// <script> external — and must still be flagged as inline. A naive
// strings.Contains(attrs, "src=") or a bare \bsrc\s*=\b regexp would both
// wrongly treat this tag as external, since "src=" appears as a literal
// substring of "data-src=".
func TestHasInlineScript_DataSrcIsNotExternal(t *testing.T) {
	t.Parallel()
	got := hasInlineScript(`<script data-src="x">console.log(1)</script>`)
	if len(got) != 1 {
		t.Fatalf("hasInlineScript = %v, want exactly 1 offender (data-src is not src=)", got)
	}
}

func truncateForTest(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// --- JSON routes: nosniff/no-store/CSP headers (review sweep, 2026-08-20) ---

// TestAdmin_JSONSecurityHeaders proves every /admin/api/* route carries
// X-Content-Type-Options: nosniff and Cache-Control: no-store alongside
// the same Content-Security-Policy the HTML page sends
// (setAdminJSONHeaders, admin.go) — folded review item, 2026-08-20
// review, moved from a bare doc-comment claim to an assertion here.
func TestAdmin_JSONSecurityHeaders(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	wantCSP := "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
	for _, p := range []string{adminOverviewPath, adminUsagePath, adminTargetsPath} {
		t.Run(p, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, p, "sk-admin1"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want %q", got, "nosniff")
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want %q", got, "no-store")
			}
			if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
				t.Errorf("Content-Security-Policy = %q, want %q", got, wantCSP)
			}
		})
	}
}

// --- redaction: no secret literal, "apiKey", or key digest ever appears ---

func TestAdmin_SecretRedaction(t *testing.T) {
	t.Parallel()
	const (
		providerKey = "SECRET-CANARY-PROVIDERKEY"
		redisPass   = "SECRET-CANARY-REDISPW"
		aliceKey    = "SECRET-CANARY-USERKEY"
		adminKey    = "SECRET-CANARY-ADMINKEY"
	)

	cfg := CreateConfig()
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: "http://openai.invalid", APIKey: providerKey, Models: []string{"gpt-test"}},
	}
	cfg.Redis = &RedisConfig{Address: "127.0.0.1:1", Password: redisPass}
	cfg.Groups = map[string]*GroupConfig{"g": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "g", APIKey: aliceKey, Limits: &LimitsConfig{}},
		{Name: "admin1", Group: "g", APIKey: adminKey, Admin: true},
	}}

	h, _ := newAdminGatewayHandle(t, cfg)

	// GET /admin is unauthenticated by design (controller-approved
	// amendment, 2026-08-20 review): assert it still returns 200 and
	// still leaks nothing.
	pageRec := httptest.NewRecorder()
	h.ServeHTTP(pageRec, adminRequest(http.MethodGet, adminPagePath, ""))
	if pageRec.Code != http.StatusOK {
		t.Fatalf("page status = %d, want 200 (unauthenticated)", pageRec.Code)
	}

	overviewRec := httptest.NewRecorder()
	h.ServeHTTP(overviewRec, adminRequest(http.MethodGet, adminOverviewPath, adminKey))
	if overviewRec.Code != http.StatusOK {
		t.Fatalf("overview status = %d, want 200, body=%s", overviewRec.Code, overviewRec.Body.String())
	}

	usageRec := httptest.NewRecorder()
	h.ServeHTTP(usageRec, adminRequest(http.MethodGet, adminUsagePath, adminKey))
	if usageRec.Code != http.StatusOK {
		t.Fatalf("usage status = %d, want 200, body=%s", usageRec.Code, usageRec.Body.String())
	}

	canaries := []string{providerKey, redisPass, aliceKey, adminKey}
	for _, name := range []string{"page", "overview", "usage"} {
		var body string
		switch name {
		case "page":
			body = pageRec.Body.String()
		case "usage":
			body = usageRec.Body.String()
		default:
			body = overviewRec.Body.String()
		}
		for _, secret := range canaries {
			if strings.Contains(body, secret) {
				t.Errorf("%s response leaks secret literal %q", name, secret)
			}
			digest := sha256.Sum256([]byte(secret))
			if strings.Contains(body, hex.EncodeToString(digest[:])) {
				t.Errorf("%s response leaks sha256 digest of secret %q", name, secret)
			}
		}
		if strings.Contains(strings.ToLower(body), "apikey") {
			t.Errorf("%s response contains an apiKey field", name)
		}
	}
}

// --- overview: baseUrl strips userinfo and query (folded item 3, 2026-08-20 review) ---

func TestAdminOverview_BaseURLStripsCredentials(t *testing.T) {
	t.Parallel()

	// Built via net/url rather than a literal string constant: a scheme
	// plus embedded username-colon-password-at-host literal in source
	// trips generic secret scanners (trufflehog's URI detector) even
	// though this is a fabricated test fixture, not a real credential.
	// Assembling it programmatically exercises the identical code path
	// (sanitizeBaseURL parses and strips it exactly the same either way)
	// without putting a credential-shaped string literal in the diff.
	credentialBaseURL := (&url.URL{
		Scheme:   "https",
		User:     url.UserPassword("embeddeduser", "embeddedpass"),
		Host:     "openai.invalid",
		RawQuery: "api-key=leakedquerysecret",
	}).String()

	cfg := CreateConfig()
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{
		// Discovery is left false: nothing ever dials this URL, it exists
		// purely to prove sanitizeBaseURL strips it before echoing.
		"openai": {Type: "openai", BaseURL: credentialBaseURL, APIKey: "sk", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"g": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "admin1", Group: "g", APIKey: "sk-admin1", Admin: true}}}

	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	for _, leaked := range []string{"embeddeduser", "embeddedpass", "leakedquerysecret", "api-key="} {
		if strings.Contains(body, leaked) {
			t.Errorf("overview response leaks base URL credential/query material %q", leaked)
		}
	}

	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("providers = %+v, want 1 entry", got.Providers)
	}
	wantBaseURL := "https://openai.invalid"
	if got.Providers[0].BaseURL != wantBaseURL {
		t.Errorf("baseUrl = %q, want %q (userinfo and query stripped)", got.Providers[0].BaseURL, wantBaseURL)
	}
}

// --- usage: batched reads (important item 2, 2026-08-20 review) ---

// countingMultiStore is a counterStore stub whose getMulti serves a fixed
// value per key and counts how many times it was called — it proves
// limiter.currentUsage flattens every scope's usageKeysPerScope counter
// reads into ONE getMulti call total (v0.2 final review wave,
// 2026-08-20), rather than usageKeysPerScope separate get calls or even
// one getMulti call per scope. incrBy/get are never exercised by
// currentUsage and just return zero values.
type countingMultiStore struct {
	values        map[string]int64
	getMultiCalls int
}

func (s *countingMultiStore) incrBy(string, int64, time.Duration) (int64, error) {
	return 0, nil
}
func (s *countingMultiStore) get(string) (int64, error) { return 0, nil }
func (s *countingMultiStore) getMulti(keys []string) ([]int64, error) {
	s.getMultiCalls++
	out := make([]int64, len(keys))
	for i, k := range keys {
		out[i] = s.values[k]
	}
	return out, nil
}
func (s *countingMultiStore) incrMulti(entries []counterIncr) ([]int64, error) {
	return make([]int64, len(entries)), nil
}
func (s *countingMultiStore) incrAndGetMulti(entries []counterIncr, reads []string) ([]int64, []int64, error) {
	readVals, err := s.getMulti(reads)
	return make([]int64, len(entries)), readVals, err
}

func TestLimiterCurrentUsage_BatchesOneGetMultiCallForAllScopes(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	store := &countingMultiStore{values: map[string]int64{
		windowKey("user", "alice", metricReq, windowMin, fixedNow):      3,
		windowKey("user", "alice", metricReq, windowDay, fixedNow):      7,
		windowKey("user", "alice", metricTokIn, windowDay, fixedNow):    50,
		windowKey("user", "alice", metricTokIn, windowMonth, fixedNow):  60,
		windowKey("user", "alice", metricTokOut, windowDay, fixedNow):   20,
		windowKey("user", "alice", metricTokOut, windowMonth, fixedNow): 25,
		windowKey("user", "alice", metricCost, windowDay, fixedNow):     1_000,
		windowKey("user", "alice", metricCost, windowMonth, fixedNow):   2_000,
		windowKey("group", "g1", metricReq, windowMin, fixedNow):        1,
	}}
	l := newLimiter(store, true)
	l.nowFn = func() time.Time { return fixedNow }

	scopes := []limitScope{
		{kind: "user", id: "alice", limits: &LimitsConfig{}},
		{kind: "group", id: "g1", limits: &LimitsConfig{}},
	}
	got := l.currentUsage(scopes)

	if store.getMultiCalls != 1 {
		t.Errorf("getMultiCalls = %d, want 1 (all scopes' keys flattened into one pipelined getMulti call)", store.getMultiCalls)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	alice := got[0]
	if alice.storeDown {
		t.Fatal("alice.storeDown must be false")
	}
	if alice.requestsPerMinute != 3 || alice.requestsPerDay != 7 || alice.tokensInPerDay != 50 ||
		alice.tokensInPerMonth != 60 || alice.tokensOutPerDay != 20 || alice.tokensOutPerMonth != 25 ||
		alice.costPerDayMicros != 1_000 || alice.costPerMonthMicros != 2_000 {
		t.Errorf("alice usage = %+v, want the seeded values", alice)
	}
	g1 := got[1]
	if g1.requestsPerMinute != 1 {
		t.Errorf("g1 requestsPerMinute = %d, want 1", g1.requestsPerMinute)
	}
}

// --- security audit finding 4: admin usage batch chunking ---

// TestChunkedCurrentUsage_SplitsIntoMultipleRoundTrips is the regression
// for finding 4: buildAdminUsage previously handed limiter.currentUsage
// the WHOLE scopes slice in one call, so one round trip's own pipeline
// size (and how long it holds the shared, mutex-guarded store connection)
// scaled with catalog size with no ceiling at all. With more scopes than
// adminUsageChunkScopes, chunkedCurrentUsage must issue more than one
// getMulti call — proving chunking actually happens, not merely that the
// constant exists.
func TestChunkedCurrentUsage_SplitsIntoMultipleRoundTrips(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	const n = adminUsageChunkScopes + 50 // over one chunk, under two

	store := &countingMultiStore{values: make(map[string]int64, n)}
	scopes := make([]limitScope, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("user%d", i)
		scopes[i] = limitScope{kind: "user", id: id, limits: &LimitsConfig{}}
		store.values[windowKey("user", id, metricReq, windowMin, fixedNow)] = int64(i)
	}

	l := newLimiter(store, true)
	l.nowFn = func() time.Time { return fixedNow }
	gw := &Gateway{limiter: l}

	got := gw.chunkedCurrentUsage(scopes)

	if store.getMultiCalls != 2 {
		t.Errorf("getMultiCalls = %d, want 2 (%d scopes over one %d-scope chunk)", store.getMultiCalls, n, adminUsageChunkScopes)
	}
	if len(got) != n {
		t.Fatalf("len(got) = %d, want %d", len(got), n)
	}
	// Order must be preserved across the chunk boundary — spot-check the
	// first scope of the SECOND chunk, whose value only exists if the
	// concatenation kept scopes[adminUsageChunkScopes] aligned with
	// got[adminUsageChunkScopes].
	boundary := got[adminUsageChunkScopes]
	if boundary.id != fmt.Sprintf("user%d", adminUsageChunkScopes) || boundary.requestsPerMinute != int64(adminUsageChunkScopes) {
		t.Errorf("scope at the chunk boundary = %+v, want id=%q requestsPerMinute=%d (order preserved across chunks)",
			boundary, fmt.Sprintf("user%d", adminUsageChunkScopes), adminUsageChunkScopes)
	}
}

// TestChunkedCurrentUsage_AtOrBelowChunkSize_MakesExactlyOneCall proves
// chunking is a no-op difference for any deployment at or under
// adminUsageChunkScopes — including the live cluster's 5 friends + 4 home
// users (project brief) — matching currentUsage's own pre-existing
// single-round-trip behavior exactly.
func TestChunkedCurrentUsage_AtOrBelowChunkSize_MakesExactlyOneCall(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
	store := &countingMultiStore{values: map[string]int64{}}
	scopes := []limitScope{
		{kind: "user", id: "alice", limits: &LimitsConfig{}},
		{kind: "user", id: "bob", limits: &LimitsConfig{}},
		{kind: "group", id: "eng", limits: &LimitsConfig{}},
	}
	l := newLimiter(store, true)
	l.nowFn = func() time.Time { return fixedNow }
	gw := &Gateway{limiter: l}

	got := gw.chunkedCurrentUsage(scopes)

	if store.getMultiCalls != 1 {
		t.Errorf("getMultiCalls = %d, want 1 (scope count under adminUsageChunkScopes)", store.getMultiCalls)
	}
	if len(got) != len(scopes) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(scopes))
	}
}

// TestChunkedCurrentUsage_EmptyScopes proves the edge case a real caller
// never hits directly (buildAdminUsage always appends the synthetic total
// scope) still behaves like currentUsage's own empty-input contract:
// no call, no panic, empty result.
func TestChunkedCurrentUsage_EmptyScopes(t *testing.T) {
	t.Parallel()
	store := &countingMultiStore{values: map[string]int64{}}
	l := newLimiter(store, true)
	gw := &Gateway{limiter: l}

	got := gw.chunkedCurrentUsage(nil)

	if store.getMultiCalls != 0 {
		t.Errorf("getMultiCalls = %d, want 0 for an empty scopes slice", store.getMultiCalls)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

// --- overview: redis lastErr carries a timestamp (folded item 4, 2026-08-20 review) ---

func TestAdminOverview_RedisLastErrAt(t *testing.T) {
	t.Parallel()
	cfg := CreateConfig()
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: "http://openai.invalid", APIKey: "sk", Models: []string{"gpt-test"}},
	}
	// Nothing listens on 127.0.0.1:1: every operation against it fails
	// fast (connection refused), driving a real store failure without a
	// live Redis dependency.
	cfg.Redis = &RedisConfig{Address: "127.0.0.1:1"}
	cfg.Groups = map[string]*GroupConfig{"g": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "admin1", Group: "g", APIKey: "sk-admin1", Admin: true}}}

	h, _ := newAdminGatewayHandle(t, cfg)

	// buildAdminUsage lists admin1 even with no seeded traffic (spec's
	// "for every user and group"), so this alone triggers a real
	// storeGetMulti call against the unreachable store.
	usageRec := httptest.NewRecorder()
	h.ServeHTTP(usageRec, adminRequest(http.MethodGet, adminUsagePath, "sk-admin1"))
	if usageRec.Code != http.StatusOK {
		t.Fatalf("usage status = %d, want 200, body=%s", usageRec.Code, usageRec.Body.String())
	}

	overviewRec := httptest.NewRecorder()
	h.ServeHTTP(overviewRec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if overviewRec.Code != http.StatusOK {
		t.Fatalf("overview status = %d, want 200, body=%s", overviewRec.Code, overviewRec.Body.String())
	}
	var got adminOverviewResponse
	if err := json.Unmarshal(overviewRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Redis.Configured {
		t.Error("redis.configured must be true")
	}
	if got.Redis.LastErr == "" {
		t.Error("redis.lastErr must be non-empty after a real store failure")
	}
	if got.Redis.LastErrAt.IsZero() {
		t.Fatal("redis.lastErrAt must be set after a real store failure")
	}
	if age := time.Since(got.Redis.LastErrAt); age < 0 || age > 30*time.Second {
		t.Errorf("redis.lastErrAt age = %v, want within 30s of now", age)
	}
}

// --- file-sourced admin user (folded item 6, 2026-08-20 review) ---

// TestAdmin_FileUserAdminFlag_GrantsAccess proves UserConfig.Admin flows
// through the file-users hot-reload path (authStore.replaceFileUsers,
// auth.go) exactly like an inline user's — buildEntry is shared by both,
// but this had never been exercised end-to-end through /admin/api/*
// before. File-users granting admin is operator-controlled via the
// Secret backing Users.File, an accepted trust boundary per
// UserConfig.Admin's own doc comment (llmgateway.go).
func TestAdmin_FileUserAdminFlag_GrantsAccess(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)

	fileUsers := []*UserConfig{
		{Name: "fileadmin", Group: "agroup", APIKey: "sk-filedmin", Admin: true},
	}
	if err := gw.auth.replaceFileUsers(fileUsers); err != nil {
		t.Fatalf("replaceFileUsers: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-filedmin"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a file-sourced admin user, body=%s", rec.Code, rec.Body.String())
	}
}

// --- usage/history: gate, validation, shape, batching, storeDown (v0.2 data-layer task) ---

// TestAdminUsageHistory_GateMatrix mirrors TestAdmin_GateMatrix for the
// third /admin/api/* route: unauthenticated → 401, authenticated
// non-admin → 403, admin → 200.
func TestAdminUsageHistory_GateMatrix(t *testing.T) {
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)
	query := adminUsageHistoryPath + "?scope=total&metric=req&window=hour"

	t.Run("unauthenticated", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(http.MethodGet, query, ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		assertOAIErrorEnvelope(t, rec)
	})
	t.Run("authenticated_non_admin", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(http.MethodGet, query, "sk-alice"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		assertOAIErrorEnvelope(t, rec)
	})
	t.Run("admin", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(http.MethodGet, query, "sk-admin1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestAdminUsageHistory_DoesNotCountStats proves GET
// /admin/api/usage/history skips checkAndCount entirely (unlike
// overview/usage — handleAdminAPI's own doc comment, admin.go): repeated
// requests never move admin1's own req/min counter, and the route stays
// reachable even once admin1 is already pinned at a requestsPerMinute
// limit tight enough to 429 on the other two admin routes.
func TestAdminUsageHistory_DoesNotCountStats(t *testing.T) {
	cfg := newAdminTestConfig()
	cfg.Users.Inline[1].Limits = &LimitsConfig{RequestsPerMinute: 1} // admin1
	h, gw := newAdminGatewayHandle(t, cfg)

	query := adminUsageHistoryPath + "?scope=total&metric=req&window=hour"
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(http.MethodGet, query, "sk-admin1"))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (never 429), body=%s", i, rec.Code, rec.Body.String())
		}
	}
	reqCount, ok := gw.limiter.getCounter("user", "admin1", metricReq, windowMin, time.Now())
	if !ok || reqCount != 0 {
		t.Errorf("admin1 req/min counter = %d (ok=%v), want 0 (history route never calls checkAndCount)", reqCount, ok)
	}
}

// TestAdminUsageHistory_ValidationTable drives GET
// /admin/api/usage/history's query-parameter validation end to end: an
// unrecognized scope kind/metric/window or an out-of-range span is 400,
// an unknown user/group id is 404, and every well-formed combination
// (including span pinned exactly at its window's max) is 200.
func TestAdminUsageHistory_ValidationTable(t *testing.T) {
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"unknown scope kind", "scope=nope&metric=req&window=hour", http.StatusBadRequest},
		{"total scope rejects an id suffix", "scope=total:all&metric=req&window=hour", http.StatusBadRequest},
		{"scope missing a colon", "scope=user&metric=req&window=hour", http.StatusBadRequest},
		{"scope with an empty id", "scope=user:&metric=req&window=hour", http.StatusBadRequest},
		{"unknown metric", "scope=total&metric=nope&window=hour", http.StatusBadRequest},
		{"unknown window", "scope=total&metric=req&window=nope", http.StatusBadRequest},
		{"span not an integer", "scope=total&metric=req&window=hour&span=abc", http.StatusBadRequest},
		{"span zero", "scope=total&metric=req&window=hour&span=0", http.StatusBadRequest},
		{"span negative", "scope=total&metric=req&window=hour&span=-1", http.StatusBadRequest},
		{"span beyond hour max (48)", "scope=total&metric=req&window=hour&span=49", http.StatusBadRequest},
		{"span beyond day max (35)", "scope=total&metric=req&window=day&span=36", http.StatusBadRequest},
		{"span beyond month max (13)", "scope=total&metric=req&window=month&span=14", http.StatusBadRequest},
		{"span pinned exactly at the hour max is valid", "scope=total&metric=req&window=hour&span=48", http.StatusOK},
		{"unknown user id", "scope=user:ghost&metric=req&window=hour", http.StatusNotFound},
		{"unknown group id", "scope=group:ghost&metric=req&window=hour", http.StatusNotFound},
		{"valid total/req/hour, default span", "scope=total&metric=req&window=hour", http.StatusOK},
		{"valid user/tokin/day", "scope=user:alice&metric=tokin&window=day", http.StatusOK},
		{"valid group/tokout/month", "scope=group:agroup&metric=tokout&window=month", http.StatusOK},
		{"valid cost metric", "scope=total&metric=cost&window=day", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageHistoryPath+"?"+c.query, "sk-admin1"))
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d, body=%s", rec.Code, c.want, rec.Body.String())
			}
			if c.want >= 400 {
				assertOAIErrorEnvelope(t, rec)
			}
		})
	}
}

// TestAdminUsageHistory_DefaultSpanIsMax asserts an omitted span defaults
// to each window's own historyMaxSpan.
func TestAdminUsageHistory_DefaultSpanIsMax(t *testing.T) {
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	cases := []struct {
		window string
		want   int
	}{
		{windowHour, 48},
		{windowDay, 35},
		{windowMonth, 13},
	}
	for _, c := range cases {
		t.Run(c.window, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageHistoryPath+"?scope=total&metric=req&window="+c.window, "sk-admin1"))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
			}
			var got usageHistoryResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got.Points) != c.want {
				t.Errorf("len(points) = %d, want %d (default span = max)", len(got.Points), c.want)
			}
		})
	}
}

// TestAdminUsageHistory_ShapeOldestFirstOneBatchCall drives the route
// against a limiter whose store is swapped for a counting stub: it must
// decode to the documented {"scope","metric","window","points":[...]}
// shape, points must be oldest-first with the current (possibly partial)
// bucket last, and the whole span must cost exactly ONE getMulti call —
// never one call per bucket.
func TestAdminUsageHistory_ShapeOldestFirstOneBatchCall(t *testing.T) {
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	countStore := &countingMultiStore{values: map[string]int64{
		windowKey(totalScopeKind, totalScopeID, metricReq, windowHour, fixedNow.Add(-2*time.Hour)): 5,
		windowKey(totalScopeKind, totalScopeID, metricReq, windowHour, fixedNow.Add(-1*time.Hour)): 7,
		windowKey(totalScopeKind, totalScopeID, metricReq, windowHour, fixedNow):                   9,
	}}
	gw.limiter = newLimiter(countStore, true)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	rec := httptest.NewRecorder()
	query := adminUsageHistoryPath + "?scope=total&metric=req&window=hour&span=3"
	h.ServeHTTP(rec, adminRequest(http.MethodGet, query, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if countStore.getMultiCalls != 1 {
		t.Errorf("getMultiCalls = %d, want 1 (one batch for the whole span)", countStore.getMultiCalls)
	}

	var got usageHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Scope != "total" || got.Metric != "req" || got.Window != "hour" {
		t.Errorf("echoed scope/metric/window = %q/%q/%q, want total/req/hour", got.Scope, got.Metric, got.Window)
	}
	if len(got.Points) != 3 {
		t.Fatalf("len(points) = %d, want 3", len(got.Points))
	}
	wantValues := []int64{5, 7, 9}
	for i, want := range wantValues {
		if got.Points[i].Value != want {
			t.Errorf("points[%d].value = %d, want %d (oldest-first)", i, got.Points[i].Value, want)
		}
	}
	if got.Points[2].Bucket != bucketFor(fixedNow, windowHour) {
		t.Errorf("points[2].bucket = %q, want the current bucket %q", got.Points[2].Bucket, bucketFor(fixedNow, windowHour))
	}
}

// TestAdminUsageHistory_StoreDown503 asserts a fail-closed store error
// answers 503 with the standard OAI-shaped error envelope, never a
// silently-zero series — data unavailable beats a chart that lies about
// zero usage.
func TestAdminUsageHistory_StoreDown503(t *testing.T) {
	cfg := newAdminTestConfig()
	failOpen := false
	cfg.Redis = &RedisConfig{Address: "127.0.0.1:1", FailOpen: &failOpen}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	query := adminUsageHistoryPath + "?scope=total&metric=req&window=hour"
	h.ServeHTTP(rec, adminRequest(http.MethodGet, query, "sk-admin1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
	assertOAIErrorEnvelope(t, rec)
}

// TestAdminUsageHistory_MemoryStoreFallbackWorks proves the history API
// serves real data through the in-process memoryStore fallback too — a
// deployment with no Redis configured at all still gets charts of
// whatever the process itself accumulated (spec's "memoryStore works
// too" requirement).
func TestAdminUsageHistory_MemoryStoreFallbackWorks(t *testing.T) {
	cfg := newAdminTestConfig() // cfg.Redis left nil: limiter.store is nil, every op uses the fallback
	h, gw := newAdminGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }
	gw.limiter.account([]limitScope{{kind: totalScopeKind, id: totalScopeID}}, usage{prompt: 12, completion: 4}, 0)

	rec := httptest.NewRecorder()
	query := adminUsageHistoryPath + "?scope=total&metric=tokin&window=hour&span=1"
	h.ServeHTTP(rec, adminRequest(http.MethodGet, query, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got usageHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Points) != 1 || got.Points[0].Value != 12 {
		t.Errorf("points = %+v, want one point with value 12", got.Points)
	}
}

// --- targets: GET /admin/api/targets (Feature B, v0.21) ---

// TestAdminTargets_NoneConfigured_EmptyArraysNotNull proves an empty
// GET /admin/api/targets response marshals mcpServers/agents as "[]", not
// omitted or "null" — mirroring adminProviderView.Models' own "always an
// array, never nil" convention (admin.go) — since newAdminTestConfig
// configures neither.
func TestAdminTargets_NoneConfigured_EmptyArraysNotNull(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminTargetsPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"mcpServers":[]`) || !strings.Contains(rec.Body.String(), `"agents":[]`) {
		t.Errorf("body = %s, want mcpServers and agents both present as empty arrays, not omitted or null", rec.Body.String())
	}
}

// TestAdminTargets_AccessListsMatchEnforcement_AndCountersReflectTraffic
// is the shape test for GET /admin/api/targets: two MCP servers and two
// agents, three groups with deliberately different mcpServers/agents glob
// restrictions, and pre-seeded per-target counters. It asserts:
//   - sorted-by-name order (alpha/beta; bot1/bot2)
//   - URL sanitization (sanitizeBaseURL strips embedded credentials, same
//     as adminProviderView.BaseURL — TestAdminOverview_BaseURLStripsCredentials
//     is this test's sibling for the provider view)
//   - access is computed via the exact SAME matchesGlob call
//     group.allowsMCP/allowsAgent themselves use (auth.go): a target every
//     configured group can reach collapses to an omitted/nil access list
//     ("empty meaning all"), one only some groups can reach lists exactly
//     those group names, sorted
//   - counters echo limiter.targetUsage's read of the exact keys
//     countTargetRequest (limits.go) writes, at min/day/month
//
// "restricted" explicitly lists both alpha (mcpServers) and bot1 (agents);
// "wide" and "admingroup" both leave their own mcpServers/agents fields
// unset (empty pattern list = allow-all, matchesGlob's own contract) — so
// alpha and bot1 are reachable by all three configured groups (access
// collapses to nil), while beta and bot2 are reachable by "wide" and
// "admingroup" only, never "restricted" (access = ["admingroup","wide"]).
func TestAdminTargets_AccessListsMatchEnforcement_AndCountersReflectTraffic(t *testing.T) {
	t.Parallel()

	// Built via net/url, not a literal string, so a generic secret scanner
	// never flags a credential-shaped literal in this test's own source —
	// same convention as TestAdminOverview_BaseURLStripsCredentials above.
	alphaURL := (&url.URL{Scheme: "http", User: url.UserPassword("mcpuser", "mcppass"), Host: "mcp-alpha.internal"}).String()

	cfg := CreateConfig()
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.MCPServers = map[string]*TargetConfig{
		"alpha": {URL: alphaURL},
		"beta":  {URL: "http://mcp-beta.internal"},
	}
	cfg.Agents = map[string]*AgentConfig{
		"bot1": {URL: "http://agent-bot1.internal"},
		"bot2": {URL: "http://agent-bot2.internal"},
	}
	cfg.Groups = map[string]*GroupConfig{
		"restricted": {MCPServers: []string{"alpha"}, Agents: []string{"bot1"}},
		"wide":       {},
		"admingroup": {},
	}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "admin1", Group: "admingroup", APIKey: "sk-admin1", Admin: true},
	}}
	h, gw := newAdminGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }
	gw.limiter.countTargetRequest(targetKindMCP, "alpha")
	gw.limiter.countTargetRequest(targetKindMCP, "alpha")
	gw.limiter.countTargetRequest(scopeKindAgent, "bot1")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminTargetsPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminTargetsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got.MCPServers) != 2 || got.MCPServers[0].Name != "alpha" || got.MCPServers[1].Name != "beta" {
		t.Fatalf("mcpServers not sorted by name: %+v", got.MCPServers)
	}
	if len(got.Agents) != 2 || got.Agents[0].Name != "bot1" || got.Agents[1].Name != "bot2" {
		t.Fatalf("agents not sorted by name: %+v", got.Agents)
	}

	alpha := got.MCPServers[0]
	if alpha.URL != "http://mcp-alpha.internal" {
		t.Errorf("alpha.URL = %q, want credentials stripped to http://mcp-alpha.internal", alpha.URL)
	}
	if alpha.Access != nil {
		t.Errorf("alpha.Access = %v, want nil (every configured group can reach it — 'empty meaning all')", alpha.Access)
	}
	if alpha.Counters.RequestsPerMinute != 2 || alpha.Counters.RequestsPerDay != 2 || alpha.Counters.RequestsPerMonth != 2 {
		t.Errorf("alpha.Counters = %+v, want min/day/month all 2", alpha.Counters)
	}

	beta := got.MCPServers[1]
	if want := []string{"admingroup", "wide"}; !slices.Equal(beta.Access, want) {
		t.Errorf("beta.Access = %v, want %v (restricted's own mcpServers list excludes it)", beta.Access, want)
	}
	if beta.Counters != (adminTargetCountersView{}) {
		t.Errorf("beta.Counters = %+v, want all zero (never proxied to in this test)", beta.Counters)
	}

	bot1 := got.Agents[0]
	if bot1.Access != nil {
		t.Errorf("bot1.Access = %v, want nil (every configured group can reach it)", bot1.Access)
	}
	if bot1.Counters.RequestsPerMinute != 1 || bot1.Counters.RequestsPerDay != 1 || bot1.Counters.RequestsPerMonth != 1 {
		t.Errorf("bot1.Counters = %+v, want min/day/month all 1", bot1.Counters)
	}

	bot2 := got.Agents[1]
	if want := []string{"admingroup", "wide"}; !slices.Equal(bot2.Access, want) {
		t.Errorf("bot2.Access = %v, want %v (restricted's own agents list excludes it)", bot2.Access, want)
	}
}

// --- default-preserving: the live cluster's real shape (security audit, 2026-08-22) ---

// TestDefaultPreserving_LiveClusterShape_LoadsAndServesIdentically models
// the live production shape this round's fixes must not break (project
// brief): 5 "friend" users plus 4 "home" users (steve-cw and kevinsandom
// are named live users; the rest are representative — real names this
// test has no visibility into, which is exactly why finding 5's fix stops
// short of restricting the name charset, see buildEntry's own doc
// comment, auth.go) and 11 MCP servers. A2A agents are out of scope here:
// none of this round's fixes touch mcp_a2a.go or any A2A-specific code
// path, so there is no mechanism by which they could regress agent
// handling.
//
// It proves construction succeeds unchanged (finding 5's new empty/
// duplicate-name checks accept every one of these real names), a
// friends-group user's configured requests-per-minute budget still buys
// EXACTLY that many tools/list calls against all 11 servers — not
// budget/11 — regressing security review round 2's critical finding 3
// (weighting tools/list by backend count was reverted specifically
// because it was not default-preserving at this exact shape: both live
// groups have an empty MCP allow-list, i.e. all 11 servers, so a naive
// per-backend weight would have silently cut every configured
// requests-per-minute budget by ~11x here), and the admin dashboard's
// usage poll still returns exactly one row per user and per group in a
// single store round trip (finding 4's chunking is a no-op at 10 users,
// well under adminUsageChunkScopes).
func TestDefaultPreserving_LiveClusterShape_LoadsAndServesIdentically(t *testing.T) {
	friendNames := []string{"steve-cw", "kevinsandom", "friend3", "friend4", "friend5"}
	homeNames := []string{"home1", "home2", "home3", "home4"}
	const friendsRPM = 60

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Groups = map[string]*GroupConfig{
		"friends": {Limits: &LimitsConfig{RequestsPerMinute: friendsRPM}},
		"home":    {Limits: &LimitsConfig{RequestsPerMinute: 120}},
	}

	cfg.MCPServers = make(map[string]*TargetConfig, 11)
	for i := 0; i < 11; i++ {
		srv := newMockJSONRPCServer(t, []mcpTool{{Name: "lookup"}})
		cfg.MCPServers[fmt.Sprintf("mcp%d", i)] = &TargetConfig{URL: srv.srv.URL}
	}

	inline := []*UserConfig{{Name: "admin1", Group: "home", Admin: true, APIKey: "sk-admin1"}}
	for _, name := range friendNames {
		inline = append(inline, &UserConfig{Name: name, Group: "friends", APIKey: "sk-" + name})
	}
	for _, name := range homeNames {
		inline = append(inline, &UserConfig{Name: name, Group: "home", APIKey: "sk-" + name})
	}
	cfg.Users = &UsersConfig{Inline: inline}

	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: want the realistic live-cluster config to load unchanged, got error: %v", err)
	}

	// Drives friendsRPM calls, not just one (security review round 2:
	// a single-call assertion cannot distinguish "budget buys N calls"
	// from "budget buys N/11 calls" — exactly the blind spot that let the
	// weighted-charge regression ship undetected).
	for i := 1; i <= friendsRPM; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newFederatedRequest(t, "sk-steve-cw", jsonrpcRequest{JSONRPC: jsonrpcVersion, Method: "tools/list", ID: json.RawMessage(fmt.Sprintf("%d", i))}))
		if rec.Code != http.StatusOK {
			t.Fatalf("tools/list call %d/%d status = %d, want 200 (budget must buy all %d calls against 11 servers), body=%s", i, friendsRPM, rec.Code, friendsRPM, rec.Body.String())
		}
	}

	usageReq := httptest.NewRequest("GET", adminUsagePath, nil)
	usageReq.Header.Set("Authorization", "Bearer sk-admin1")
	usageRec := httptest.NewRecorder()
	h.ServeHTTP(usageRec, usageReq)
	if usageRec.Code != http.StatusOK {
		t.Fatalf("admin usage status = %d, want 200, body=%s", usageRec.Code, usageRec.Body.String())
	}
	var usage adminUsageResponse
	if err := json.Unmarshal(usageRec.Body.Bytes(), &usage); err != nil {
		t.Fatalf("decode admin usage: %v", err)
	}
	wantUsers := len(friendNames) + len(homeNames) + 1 // + admin1
	if len(usage.Users) != wantUsers {
		t.Errorf("admin usage users = %d, want %d", len(usage.Users), wantUsers)
	}
	if len(usage.Groups) != 2 {
		t.Errorf("admin usage groups = %d, want 2", len(usage.Groups))
	}
}

// --- usage/models: the Charts view's model ranking ---

// seedModelCounter seeds one kindModel counter for the ranking tests.
func seedModelCounter(gw *Gateway, id, metric, window string, now time.Time, n int64) {
	gw.limiter.incrCounter(kindModel, id, metric, window, now, n, dayWindowTTL)
}

// TestAdminUsageModels_RanksNonZeroOnly is the operator requirement in one
// test: a catalog model with no traffic must not appear at all (the real
// catalog runs to hundreds), and what does appear is ordered by value,
// descending.
func TestAdminUsageModels_RanksNonZeroOnly(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	// alpha/a-model-2 is deliberately left at zero.
	seedModelCounter(gw, "alpha/a-model-1", metricCost, windowDay, fixedNow, 900)
	seedModelCounter(gw, "zeta/z-model", metricCost, windowDay, fixedNow, 4_100)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageModelsPath+"?metric=cost&window=day", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got adminUsageModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("models = %+v, want exactly the 2 non-zero entries", got.Models)
	}
	if got.Models[0].ID != "zeta/z-model" || got.Models[0].Value != 4_100 {
		t.Errorf("models[0] = %+v, want zeta/z-model at 4100 (highest first)", got.Models[0])
	}
	if got.Models[1].ID != "alpha/a-model-1" || got.Models[1].Value != 900 {
		t.Errorf("models[1] = %+v, want alpha/a-model-1 at 900", got.Models[1])
	}
	for _, m := range got.Models {
		if m.ID == "alpha/a-model-2" {
			t.Errorf("zero-usage model alpha/a-model-2 must not be listed: %+v", got.Models)
		}
	}
}

// TestAdminUsageModels_TieBreaksOnIDAscending pins the stability rule: two
// models on the same value must not reshuffle between polls.
func TestAdminUsageModels_TieBreaksOnIDAscending(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	seedModelCounter(gw, "zeta/z-model", metricReq, windowDay, fixedNow, 7)
	seedModelCounter(gw, "alpha/a-model-1", metricReq, windowDay, fixedNow, 7)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageModelsPath+"?metric=req&window=day", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminUsageModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Models) != 2 || got.Models[0].ID != "alpha/a-model-1" || got.Models[1].ID != "zeta/z-model" {
		t.Errorf("models = %+v, want equal values ordered by id ascending", got.Models)
	}
}

// TestAdminUsageModels_Limit checks the limit truncates AFTER ranking, so
// a limit of 1 returns the single busiest model, not an arbitrary one.
func TestAdminUsageModels_Limit(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	seedModelCounter(gw, "alpha/a-model-1", metricCost, windowDay, fixedNow, 10)
	seedModelCounter(gw, "zeta/z-model", metricCost, windowDay, fixedNow, 99)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageModelsPath+"?metric=cost&window=day&limit=1", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminUsageModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Models) != 1 || got.Models[0].ID != "zeta/z-model" {
		t.Errorf("models = %+v, want only the busiest model", got.Models)
	}
}

// TestAdminUsageModels_ValidatesParameters covers every 400 path.
func TestAdminUsageModels_ValidatesParameters(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	for _, tc := range []struct{ name, query string }{
		{"unknown metric", "?metric=nope&window=day"},
		{"missing metric", "?window=day"},
		{"unknown window", "?metric=cost&window=decade"},
		{"missing window", "?metric=cost"},
		{"limit zero", "?metric=cost&window=day&limit=0"},
		{"limit negative", "?metric=cost&window=day&limit=-3"},
		{"limit over max", "?metric=cost&window=day&limit=101"},
		{"limit not a number", "?metric=cost&window=day&limit=lots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageModelsPath+tc.query, "sk-admin1"))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestAdminUsageModels_StoreDownIs503 pins the rule the whole feature rests
// on: an unreachable store must never render as "no model was used".
func TestAdminUsageModels_StoreDownIs503(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.limiter = newLimiter(alwaysErrStore{}, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageModelsPath+"?metric=cost&window=day", "sk-admin1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
}

// --- usage/history: the model scope ---

// TestAdminUsageHistory_ModelScope drives a model through the SAME history
// endpoint the user/group scopes use, which is the point of keying model
// usage as an ordinary scope kind.
func TestAdminUsageHistory_ModelScope(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }
	gw.limiter.incrCounter(kindModel, "alpha/a-model-1", metricCost, windowDay, fixedNow, 1_234, dayWindowTTL)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet,
		adminUsageHistoryPath+"?scope=model:alpha/a-model-1&metric=cost&window=day&span=1", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got usageHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Points) != 1 || got.Points[0].Value != 1_234 {
		t.Errorf("points = %+v, want the seeded 1234", got.Points)
	}
}

// TestAdminUsageHistory_UnknownModelIs404 keeps the model scope honest: an
// id outside the live catalog is not a silently-empty series.
func TestAdminUsageHistory_UnknownModelIs404(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet,
		adminUsageHistoryPath+"?scope=model:alpha/no-such-model&metric=cost&window=day&span=1", "sk-admin1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestParseHistoryScope_ModelIDKeepsLaterSeparators guards the id shapes a
// real catalog contains: a "/" always, and sometimes a ":" (an upstream
// ":free" suffix). Only the FIRST colon separates kind from id.
func TestParseHistoryScope_ModelIDKeepsLaterSeparators(t *testing.T) {
	t.Parallel()
	kind, id, ok := parseHistoryScope("model:openrouter/some-model:free")
	if !ok || kind != kindModel || id != "openrouter/some-model:free" {
		t.Errorf("parseHistoryScope = (%q, %q, %v), want the full model id preserved", kind, id, ok)
	}
}

// --- feat/target-health: GET /admin/api/targets "health" field ---

// TestAdminTargets_Health_NeverObserved_UnknownWithOmittedFields proves
// the exact JSON contract for a target this tracker has never observed:
// "state":"unknown", consecutiveFailures present as 0, and every other
// health field (lastCheck/lastError/latencyMs/source) omitted entirely.
func TestAdminTargets_Health_NeverObserved_UnknownWithOmittedFields(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: "http://mcp-alpha.internal"}}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminTargetsPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"health":{"state":"unknown","consecutiveFailures":0}`) {
		t.Errorf("body = %s, want an unknown target's health object to carry only state and consecutiveFailures", body)
	}
}

// TestAdminTargets_Health_Recorded_AllFieldsPresent proves a target with
// at least one recorded observation carries every health field, and that
// state/source/consecutiveFailures reflect the tracker exactly.
func TestAdminTargets_Health_Recorded_AllFieldsPresent(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: "http://mcp-alpha.internal"}}
	h, gw := newAdminGatewayHandle(t, cfg)

	gw.targetHealth.record(targetKindMCP, "alpha", true, nil, 12*time.Millisecond, targetHealthSourceTraffic)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminTargetsPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminTargetsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.MCPServers) != 1 {
		t.Fatalf("mcpServers = %+v, want exactly 1", got.MCPServers)
	}
	health := got.MCPServers[0].Health
	if health.State != targetHealthHealthy {
		t.Errorf("state = %q, want healthy", health.State)
	}
	if health.LastCheck == "" {
		t.Error("want a non-empty lastCheck (RFC3339)")
	}
	if _, err := time.Parse(time.RFC3339, health.LastCheck); err != nil {
		t.Errorf("lastCheck = %q, want RFC3339: %v", health.LastCheck, err)
	}
	if health.Source != targetHealthSourceTraffic {
		t.Errorf("source = %q, want %q", health.Source, targetHealthSourceTraffic)
	}
	if health.LatencyMs == nil || *health.LatencyMs != 12 {
		t.Errorf("latencyMs = %v, want *12", health.LatencyMs)
	}
	if health.ConsecutiveFailures != 0 {
		t.Errorf("consecutiveFailures = %d, want 0", health.ConsecutiveFailures)
	}
	if health.LastError != "" {
		t.Errorf("lastError = %q, want empty (omitted) after a success", health.LastError)
	}
}

// TestAdminTargets_Health_Unhealthy_LastErrorPresent proves an unhealthy
// target's lastError is populated and non-empty.
func TestAdminTargets_Health_Unhealthy_LastErrorPresent(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.MCPServers = map[string]*TargetConfig{"alpha": {URL: "http://mcp-alpha.internal"}}
	cfg.TargetHealth = TargetHealthConfig{FailureThreshold: 1}
	h, gw := newAdminGatewayHandle(t, cfg)

	gw.targetHealth.record(targetKindMCP, "alpha", false, errors.New("dial tcp: connection refused"), 3*time.Millisecond, targetHealthSourceProbe)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminTargetsPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminTargetsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	health := got.MCPServers[0].Health
	if health.State != targetHealthUnhealthy {
		t.Errorf("state = %q, want unhealthy", health.State)
	}
	if health.LastError != "dial tcp: connection refused" {
		t.Errorf("lastError = %q, want the recorded error text", health.LastError)
	}
	if health.Source != targetHealthSourceProbe {
		t.Errorf("source = %q, want %q", health.Source, targetHealthSourceProbe)
	}
}

// TestAdminTargets_Health_AgentUsesAgentKind proves an agent row's health
// is read under targetKindAgent, not the mcp kind.
func TestAdminTargets_Health_AgentUsesAgentKind(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Agents = map[string]*AgentConfig{"bot1": {URL: "http://agent-bot1.internal"}}
	h, gw := newAdminGatewayHandle(t, cfg)

	gw.targetHealth.record(targetKindAgent, "bot1", true, nil, time.Millisecond, targetHealthSourceTraffic)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminTargetsPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminTargetsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Agents) != 1 || got.Agents[0].Health.State != targetHealthHealthy {
		t.Fatalf("agents = %+v, want bot1 healthy", got.Agents)
	}
}
