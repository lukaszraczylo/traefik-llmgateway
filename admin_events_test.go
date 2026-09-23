package traefikllmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newEventsTestConfig builds a Config with Admin enabled, one openai-type
// provider pointed at upstreamURL, and two users: "alice" (an ordinary
// caller, Limits left to the test to set) and "admin1" (Admin: true, for
// reading GET /admin/api/events). Callers may mutate the returned Config
// (in particular cfg.Users.Inline[0].Limits) before calling New.
func newEventsTestConfig(upstreamURL string) *Config {
	cfg := CreateConfig()
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: upstreamURL, APIKey: "sk-up", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
		{Name: "admin1", Group: "default", APIKey: "sk-admin1", Admin: true},
	}}
	return cfg
}

// getAdminEvents drives GET /admin/api/events?limit=... as sk-admin1 and
// decodes the response.
func getAdminEvents(t *testing.T, h http.Handler, query string) adminEventsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, adminEventsPath+query, nil)
	req.Header.Set("Authorization", "Bearer sk-admin1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s%s: status = %d, want 200, body=%s", adminEventsPath, query, rec.Code, rec.Body.String())
	}
	var got adminEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// --- gate (spec §4, same as every other /admin/api/* route) ---

func TestAdminEvents_Gate(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	cfg := newEventsTestConfig(srv.URL)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, adminEventsPath, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: status = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, adminEventsPath, nil)
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin: status = %d, want 403", rec.Code)
	}

	got := getAdminEvents(t, h, "")
	if got.Events == nil {
		t.Error("Events must never be nil")
	}
	if got.Capacity != eventRingCap {
		t.Errorf("Capacity = %d, want %d", got.Capacity, eventRingCap)
	}
}

// --- limit validation ---

func TestAdminEvents_LimitValidation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	cfg := newEventsTestConfig(srv.URL)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct{ name, query string }{
		{"zero", "?limit=0"},
		{"negative", "?limit=-1"},
		{"over max", fmt.Sprintf("?limit=%d", adminEventsMaxLimit+1)},
		{"not a number", "?limit=lots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, adminEventsPath+tc.query, nil)
			req.Header.Set("Authorization", "Bearer sk-admin1")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	got := getAdminEvents(t, h, fmt.Sprintf("?limit=%d", adminEventsMaxLimit))
	if got.Events == nil {
		t.Error("Events must never be nil at the max limit")
	}
}

// --- hook 1: rate_limit / budget events via a real admission rejection ---

func TestAdminEvents_RateLimitEvent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	cfg := newEventsTestConfig(srv.URL)
	cfg.Users.Inline[0].Limits = &LimitsConfig{RequestsPerMinute: 1}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	if rec1.Code != http.StatusOK {
		t.Fatalf("1st call: status = %d, want 200, body=%s", rec1.Code, rec1.Body.String())
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("2nd call: status = %d, want 429 (over requestsPerMinute:1), body=%s", rec2.Code, rec2.Body.String())
	}

	got := getAdminEvents(t, h, "?limit=10")
	if len(got.Events) == 0 {
		t.Fatal("want at least one event")
	}
	ev := got.Events[0] // newest first
	if ev.Kind != eventKindRateLimit {
		t.Errorf("Kind = %q, want %q", ev.Kind, eventKindRateLimit)
	}
	if ev.Route != routeChatCompletions {
		t.Errorf("Route = %q, want %q", ev.Route, routeChatCompletions)
	}
	if ev.User != "alice" {
		t.Errorf("User = %q, want %q", ev.User, "alice")
	}
	if ev.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d, want 429", ev.Status)
	}
}

func TestAdminEvents_BudgetEvent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":5}}`))
	}))
	defer srv.Close()
	cfg := newEventsTestConfig(srv.URL)
	cfg.Users.Inline[0].Limits = &LimitsConfig{TokensPerDay: 1} // the 1st call's own 10 reported tokens already exceeds this
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	if rec1.Code != http.StatusOK {
		t.Fatalf("1st call: status = %d, want 200, body=%s", rec1.Code, rec1.Body.String())
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("2nd call: status = %d, want 429 (over tokensPerDay:1 budget), body=%s", rec2.Code, rec2.Body.String())
	}

	got := getAdminEvents(t, h, "?limit=10")
	if len(got.Events) == 0 {
		t.Fatal("want at least one event")
	}
	if got.Events[0].Kind != eventKindBudget {
		t.Errorf("Kind = %q, want %q", got.Events[0].Kind, eventKindBudget)
	}
}

// --- hook 2: upstream event via a real 500 from the provider ---

func TestAdminEvents_UpstreamEvent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	cfg := newEventsTestConfig(srv.URL)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (provider's own status forwarded), body=%s", rec.Code, rec.Body.String())
	}

	got := getAdminEvents(t, h, "?limit=10")
	if len(got.Events) == 0 {
		t.Fatal("want at least one event")
	}
	ev := got.Events[0]
	if ev.Kind != eventKindUpstream {
		t.Errorf("Kind = %q, want %q", ev.Kind, eventKindUpstream)
	}
	if ev.Status != http.StatusInternalServerError {
		t.Errorf("Status = %d, want 500", ev.Status)
	}
	if ev.Provider != "openai" {
		t.Errorf("Provider = %q, want %q", ev.Provider, "openai")
	}
	if ev.User != "alice" {
		t.Errorf("User = %q, want %q", ev.User, "alice")
	}
}

// --- hook 4: unpriced event via pricingWarn ---

func TestAdminEvents_UnpricedEvent(t *testing.T) {
	// warnedModels is a process-global dedup set (pricing.go) — reset
	// around this test exactly like TestGateway_PricingWarn_LogsUnderOwnInstance
	// (llmgateway_test.go) does, so a prior test's own warning for the
	// same model id can't suppress this one.
	warnedModelsMu.Lock()
	prevWarned, prevCapNotified := warnedModels, warnCapNotified
	warnedModels = map[string]bool{}
	warnCapNotified = false
	warnedModelsMu.Unlock()
	t.Cleanup(func() {
		warnedModelsMu.Lock()
		warnedModels, warnCapNotified = prevWarned, prevCapNotified
		warnedModelsMu.Unlock()
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	cfg := newEventsTestConfig(srv.URL)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	const model = "totally-unpriced-model-for-admin-events-test"
	costMicrosKnownFor(model, usage{prompt: 1}, nil, gw.pricingWarn)

	got := getAdminEvents(t, h, "?limit=10")
	if len(got.Events) == 0 {
		t.Fatal("want at least one event")
	}
	ev := got.Events[0]
	if ev.Kind != eventKindUnpriced {
		t.Errorf("Kind = %q, want %q", ev.Kind, eventKindUnpriced)
	}
	if ev.Route != routePricing {
		t.Errorf("Route = %q, want %q", ev.Route, routePricing)
	}
	if ev.Model != model {
		t.Errorf("Model = %q, want %q", ev.Model, model)
	}
}

// --- source/degraded: local ring vs Redis-backed list ---

// TestAdminEvents_RingFallback_NoRedisConfigured proves the local ring
// always answers, labeled source "replica", not degraded, when Redis is
// simply not configured (the common single-replica/no-Redis deployment).
func TestAdminEvents_RingFallback_NoRedisConfigured(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	cfg := newEventsTestConfig(srv.URL)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	gw.events.ring.add(gatewayEvent{Route: routeCapacity, Kind: eventKindCapacity, Message: "seed"})

	got := getAdminEvents(t, h, "")
	if got.Source != "replica" {
		t.Errorf("Source = %q, want %q", got.Source, "replica")
	}
	if got.Degraded {
		t.Error("Degraded must be false when Redis was never configured")
	}
	if len(got.Events) != 1 || got.Events[0].Message != "seed" {
		t.Errorf("Events = %+v, want the ring's own seeded entry", got.Events)
	}
}

// TestAdminEvents_RedisSource drives a real GET /admin/api/events against
// a scripted fake RESP server (F3, v0.3 dashboard task): the response
// carries a fixture event whose Replica ("other-pod") differs from this
// process's own — proving the multi-replica point of the feature: a
// dashboard reading through ANY replica sees every replica's events via
// the shared Redis-backed list, not just its own local ring.
func TestAdminEvents_RedisSource(t *testing.T) {
	t.Parallel()
	fixture := gatewayEvent{
		Time: time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC), Replica: "other-pod", Instance: "llmgw-other",
		User: "bob", Route: routeChatCompletions, Kind: eventKindRateLimit,
		Message: `user "bob" exceeded requests-per-minute limit (1)`, Status: http.StatusTooManyRequests,
	}
	payload, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"LRANGE", eventsRedisKey, "0", "49"}, reply: []byte(fmt.Sprintf("*1\r\n$%d\r\n%s\r\n", len(payload), payload))},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	cfg := newEventsTestConfig(srv.URL)
	cfg.Redis = &RedisConfig{Address: ln.Addr().String()}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := getAdminEvents(t, h, "")
	if got.Source != "redis" {
		t.Errorf("Source = %q, want %q", got.Source, "redis")
	}
	if got.Degraded {
		t.Error("Degraded must be false on a successful redis read")
	}
	if len(got.Events) != 1 {
		t.Fatalf("Events = %+v, want exactly the 1 fixture entry", got.Events)
	}
	if got.Events[0].Replica != "other-pod" || got.Events[0].Message != fixture.Message {
		t.Errorf("Events[0] = %+v, want the fixture read back from redis", got.Events[0])
	}
}

// TestAdminEvents_Degraded_OnLRangeError proves a Redis error reply to
// LRANGE (not just a dead connection) falls back to the local ring,
// labeled source "replica" with Degraded true — Redis IS configured, this
// particular read just failed, which is exactly what Degraded means
// (never "Redis not configured at all", eventLog.read's own doc comment).
func TestAdminEvents_Degraded_OnLRangeError(t *testing.T) {
	t.Parallel()
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"LRANGE", eventsRedisKey, "0", "49"}, reply: []byte("-ERR wrongtype or similar\r\n")},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	cfg := newEventsTestConfig(srv.URL)
	cfg.Redis = &RedisConfig{Address: ln.Addr().String()}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	gw.events.ring.add(gatewayEvent{Route: routeCapacity, Kind: eventKindCapacity, Message: "local-fallback-seed"})

	got := getAdminEvents(t, h, "")
	if got.Source != "replica" {
		t.Errorf("Source = %q, want %q (LRANGE errored, fall back to the local ring)", got.Source, "replica")
	}
	if !got.Degraded {
		t.Error("Degraded must be true: redis IS configured, but this read failed")
	}
	if len(got.Events) != 1 || got.Events[0].Message != "local-fallback-seed" {
		t.Errorf("Events = %+v, want the ring's own seeded entry", got.Events)
	}
}

// TestAdminEvents_Latched_SkipsLRANGE proves GET /admin/api/events answers
// from the local ring (source "replica", Degraded true) WITHOUT ever
// issuing LRANGE while the limiter's own store-down latch (limits.go's
// storeLatched) is open — item 3 fix (verify-dash-backend.md's lower-
// severity observation): before this fix, every admin poll during a
// store outage still paid LRANGE's own respCallTimeout before falling
// back, exactly like recordEvent already avoids paying for its own
// Redis mirror push while latched. The fake Redis server is scripted to
// answer LRANGE SUCCESSFULLY with a distinguishable fixture event, so a
// wrongly-issued LRANGE call would surface as source "redis" carrying
// that fixture — not silently indistinguishable from the correct skip,
// which also happens to report source "replica"/Degraded true.
func TestAdminEvents_Latched_SkipsLRANGE(t *testing.T) {
	t.Parallel()
	fixture := gatewayEvent{
		Time: time.Now(), Replica: "other-pod", Route: routeChatCompletions, Kind: eventKindRateLimit,
		Message: "SHOULD-NOT-BE-SEEN: LRANGE was issued while the store was latched down",
	}
	payload, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"LRANGE", eventsRedisKey, "0", "49"}, reply: []byte(fmt.Sprintf("*1\r\n$%d\r\n%s\r\n", len(payload), payload))},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	cfg := newEventsTestConfig(srv.URL)
	cfg.Redis = &RedisConfig{Address: ln.Addr().String()}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)
	gw.events.ring.add(gatewayEvent{Route: routeCapacity, Kind: eventKindCapacity, Message: "local-fallback-seed"})
	// Opens the SAME latch storeIncrMulti/storeGetMulti already respect
	// (limits.go's own storeLatched doc comment) — recordStoreFailure is
	// the real trigger a live store outage uses, not a hand-set field.
	gw.limiter.recordStoreFailure(errors.New("simulated store outage for this test"))

	got := getAdminEvents(t, h, "")
	if got.Source != "replica" {
		t.Errorf("Source = %q, want %q (latched: LRANGE must be skipped entirely, not merely tolerated if it failed)", got.Source, "replica")
	}
	if !got.Degraded {
		t.Error("Degraded must be true while the store is latched down")
	}
	if len(got.Events) != 1 || got.Events[0].Message != "local-fallback-seed" {
		t.Errorf("Events = %+v, want only the ring's own seeded entry — the fixture LRANGE would have answered must never appear", got.Events)
	}
}
