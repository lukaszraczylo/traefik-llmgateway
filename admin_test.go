package traefikllmgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
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
// req/min-req/day statistics"): GET /admin/api/overview and GET
// /admin/api/usage never call checkAndCount, so an admin key with a
// requestsPerMinute limit tight enough to 429 on any other authenticated
// route never 429s here, no matter how many requests it makes — and its
// own req/min counter stays at exactly 0 throughout. This inverts the
// pre-directive TestAdmin_RateLimitApplies, which asserted the opposite
// (a 2nd request 429ing); it mirrors TestAdminUsageHistory_DoesNotCountStats,
// which already proved this for the third admin route from the start.
func TestAdmin_NeverRateLimitedAndCountersUntouched(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Users.Inline[1].Limits = &LimitsConfig{RequestsPerMinute: 1} // admin1
	h, gw := newAdminGatewayHandle(t, cfg)

	fixedNow := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	for _, p := range []string{adminOverviewPath, adminUsagePath} {
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

func truncateForTest(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
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

	wantCSP := "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"
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
