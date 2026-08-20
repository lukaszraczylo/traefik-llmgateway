package traefikllmgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

	for _, p := range []string{adminPagePath, adminOverviewPath, adminUsagePath} {
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

// TestAdmin_GateMatrix covers the two /admin/api/* routes only — GET
// /admin itself is unauthenticated by design (TestAdminPage_
// ServedWithoutAuth above).
func TestAdmin_GateMatrix(t *testing.T) {
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	paths := []string{adminOverviewPath, adminUsagePath}
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

	if got.Version != pluginVersion {
		t.Errorf("version = %q, want %q", got.Version, pluginVersion)
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
	if p.LastRefresh.IsZero() {
		t.Error("lastRefresh must be set even on a failed refresh (finishRefresh always advances it)")
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
	if alice.TokensPerDay != 50 || alice.TokensPerMonth != 50 {
		t.Errorf("alice tokens = day:%d month:%d, want 50/50", alice.TokensPerDay, alice.TokensPerMonth)
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
	if agroup.TokensPerDay != 150 || agroup.TokensPerMonth != 150 {
		t.Errorf("agroup tokens = day:%d month:%d, want 150/150", agroup.TokensPerDay, agroup.TokensPerMonth)
	}
	if agroup.CostPerDayMicroUSD != 9_000_000 {
		t.Errorf("agroup cost/day = %d, want 9000000", agroup.CostPerDayMicroUSD)
	}

	// admin1 itself must also be listed (buildAdminUsage lists every
	// active user/group, not just ones with seeded traffic): admin1 has
	// no user-level limits configured, so buildLimitScopes never builds a
	// "user"/admin1 scope for its own requests (ruling e) and its own
	// counters stay at zero — but the row must still be present.
	admin1 := findUsageEntry(t, got.Users, "admin1")
	if admin1.RequestsPerMinute != 0 {
		t.Errorf("admin1 requestsPerMinute = %d, want 0 (admin1 has no user-level limits, so its own requests never build a user scope)", admin1.RequestsPerMinute)
	}
	if admin1.StoreDown {
		t.Error("admin1.storeDown must be false")
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
	if su.requestsPerMinute != 0 || su.requestsPerDay != 0 || su.tokensPerDay != 0 ||
		su.tokensPerMonth != 0 || su.costPerDayMicros != 0 || su.costPerMonthMicros != 0 {
		t.Errorf("storeDown entry must report zero values, got %+v", su)
	}
}

// --- rate limit applies to admin routes ---

func TestAdmin_RateLimitApplies(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Users.Inline[1].Limits = &LimitsConfig{RequestsPerMinute: 1} // admin1
	h, gw := newAdminGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("1st request status = %d, want 200, body=%s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("2nd request status = %d, want 429 (admin over req/min limit)", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("429 response must carry a Retry-After header")
	}
}

// --- HTML page: CSP header + fetch URLs, no external assets ---

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
	wantCSP := "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'"
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("CSP = %q, want %q", got, wantCSP)
	}

	body := rec.Body.String()
	for _, want := range []string{adminOverviewPath, adminUsagePath, "setInterval", "sessionStorage", "x-api-key"} {
		if !strings.Contains(body, want) {
			t.Errorf("admin page body missing %q", want)
		}
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Error("admin page must load zero external assets (no http(s):// reference of any kind)")
	}
}

// --- JSON routes: nosniff/no-store/CSP headers (review sweep, 2026-08-20) ---

// TestAdmin_JSONSecurityHeaders proves both /admin/api/* routes carry
// X-Content-Type-Options: nosniff and Cache-Control: no-store alongside
// the same Content-Security-Policy the HTML page sends
// (setAdminJSONHeaders, admin.go) — folded review item, 2026-08-20
// review, moved from a bare doc-comment claim to an assertion here.
func TestAdmin_JSONSecurityHeaders(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	wantCSP := "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'"
	for _, p := range []string{adminOverviewPath, adminUsagePath} {
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
// limiter.currentUsage batches a scope's six counter reads into one
// getMulti call rather than issuing them as six separate get calls.
// incrBy/get are never exercised by currentUsage and just return zero
// values.
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

func TestLimiterCurrentUsage_BatchesOneGetMultiCallPerScope(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	store := &countingMultiStore{values: map[string]int64{
		windowKey("user", "alice", metricReq, windowMin, fixedNow):    3,
		windowKey("user", "alice", metricReq, windowDay, fixedNow):    7,
		windowKey("user", "alice", metricTok, windowDay, fixedNow):    50,
		windowKey("user", "alice", metricTok, windowMonth, fixedNow):  60,
		windowKey("user", "alice", metricCost, windowDay, fixedNow):   1_000,
		windowKey("user", "alice", metricCost, windowMonth, fixedNow): 2_000,
		windowKey("group", "g1", metricReq, windowMin, fixedNow):      1,
	}}
	l := newLimiter(store, true)
	l.nowFn = func() time.Time { return fixedNow }

	scopes := []limitScope{
		{kind: "user", id: "alice", limits: &LimitsConfig{}},
		{kind: "group", id: "g1", limits: &LimitsConfig{}},
	}
	got := l.currentUsage(scopes)

	if store.getMultiCalls != len(scopes) {
		t.Errorf("getMultiCalls = %d, want %d (one pipelined getMulti per scope, not six separate get calls)", store.getMultiCalls, len(scopes))
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	alice := got[0]
	if alice.storeDown {
		t.Fatal("alice.storeDown must be false")
	}
	if alice.requestsPerMinute != 3 || alice.requestsPerDay != 7 || alice.tokensPerDay != 50 ||
		alice.tokensPerMonth != 60 || alice.costPerDayMicros != 1_000 || alice.costPerMonthMicros != 2_000 {
		t.Errorf("alice usage = %+v, want the seeded values", alice)
	}
	g1 := got[1]
	if g1.requestsPerMinute != 1 {
		t.Errorf("g1 requestsPerMinute = %d, want 1", g1.requestsPerMinute)
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
