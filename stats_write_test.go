package traefikllmgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// This file measures counterStore round trips per request — the concrete
// evidence for admin-redesign WP-A's own hot-path promise (COMMON3.md,
// plan §2's command table): "round trips per request unchanged (2 sync +
// 1 async); cache hit +1 async RT; defaults (umodel/latency off) add <=3
// INCRBY to account and <=2 SET/min to admission." Every test below drives
// a real Gateway (New(), the same constructor production uses) through a
// single-group chat request and counts calls to the limiter's own
// counterStore — never entries within a call, since a redisStore call
// pipelines its whole batch into ONE network round trip regardless of how
// many entries it carries (counterStore's own doc comment, limits.go), so
// counting CALLS is exactly counting round trips.

// roundTripCountingStore wraps a real counterStore (a fresh memoryStore,
// so behavior is otherwise identical to a Redis-less deployment) and
// counts every call to its three methods.
type roundTripCountingStore struct {
	delegate counterStore
	mu       sync.Mutex
	calls    int
}

func (s *roundTripCountingStore) getMulti(keys []string) ([]int64, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.delegate.getMulti(keys)
}

func (s *roundTripCountingStore) incrMulti(entries []counterIncr) ([]int64, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.delegate.incrMulti(entries)
}

func (s *roundTripCountingStore) incrAndGetMulti(entries []counterIncr, reads []string) ([]int64, []int64, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.delegate.incrAndGetMulti(entries, reads)
}

func (s *roundTripCountingStore) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// buildRoundTripTestGateway constructs a real Gateway for a single-group,
// single-user, single-provider chat deployment — cfgFn, when non-nil, may
// layer on admin/stats/cache/metrics config before construction — then
// swaps the limiter's store for a call-counting wrapper and forces spawn
// synchronous, so an async write (recordProviderAttempt, recordCacheHit)
// lands deterministically before the test inspects the count. The swap
// happens strictly AFTER New() returns: construction itself never calls a
// counterStore method, so it is never counted either way.
func buildRoundTripTestGateway(t *testing.T, srv *httptest.Server, cfgFn func(*Config)) (*Gateway, *roundTripCountingStore) {
	t.Helper()
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	if cfgFn != nil {
		cfgFn(cfg)
	}

	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	counting := &roundTripCountingStore{delegate: newMemoryStore()}
	gw.limiter.store = counting
	gw.limiter.spawn = func(f func()) { f() } // land every async write synchronously before this test inspects counts
	return gw, counting
}

const roundTripTestRespBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`

func roundTripTestUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(roundTripTestRespBody))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func roundTripChatRequest(t *testing.T) *http.Request {
	t.Helper()
	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	return newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
}

// TestRoundTrips_SingleGroupChat_Defaults_TwoSyncOneAsync is the
// command-table baseline: a single-group chat request with every admin
// stat off pays exactly 2 SYNCHRONOUS counterStore round trips
// (checkAndCount's storeIncrAndGetMulti, account's storeIncrMulti) plus 1
// ASYNCHRONOUS one (recordProviderAttempt's spawned storeIncrMulti,
// forced synchronous here for a deterministic count) — 3 counterStore
// calls total. This is BEFORE, in the same sense the task's own
// instructions ask for: the number every "all stats on" comparison below
// must not exceed.
func TestRoundTrips_SingleGroupChat_Defaults_TwoSyncOneAsync(t *testing.T) {
	srv := roundTripTestUpstream(t)
	gw, counting := buildRoundTripTestGateway(t, srv, nil)

	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, roundTripChatRequest(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	got := counting.total()
	t.Logf("BEFORE (defaults, no admin stats): %d counterStore round trips for one single-group chat request", got)
	if got != 3 {
		t.Errorf("round trips = %d, want 3 (2 sync + 1 async, command table)", got)
	}
}

// TestRoundTrips_SingleGroupChat_AllStatsOn_NoNewSyncRoundTrips is this
// task's own deliverable: turning EVERY new admin stat on (userModel,
// latency; admin enabled; metrics enabled to also exercise the latency
// recorder's metrics-side wiring) for a single-group chat request that is
// neither a cache hit nor a failover adds ZERO new SYNCHRONOUS counterStore
// round trips — every new counter family this task adds to the request's
// own two synchronous calls (checkAndCount, accountWith) rides an EXISTING
// batch (accountWith's own umodel/latency extras blocks), never a new one.
//
// It DOES add exactly one new ASYNCHRONOUS round trip, recordLastSeen's own
// fire-and-forget write (P9 fix, admin dashboard redesign verify round):
// last-seen used to ride checkAndCount's own synchronous batch
// unconditionally, which meant a REJECTED request still moved that scope's
// last-seen timestamp forward. It is now recorded only from the "admission
// confirmed" branch, off the request's hot path via countAsync/l.spawn —
// exactly like recordProviderAttempt's own pre-existing async write — so
// this is a SEPARATE async call, not a batch this one rides. 2 sync + 2
// async = 4 total, once per admitted request from a scope l.lastSeen.due
// has not already throttled within lastSeenGateThrottle (60s) — a fresh
// limiter's first request for a scope, as this test drives.
func TestRoundTrips_SingleGroupChat_AllStatsOn_NoNewSyncRoundTrips(t *testing.T) {
	srv := roundTripTestUpstream(t)
	gw, counting := buildRoundTripTestGateway(t, srv, func(cfg *Config) {
		cfg.Admin = &AdminConfig{Enabled: true, Stats: &AdminStatsConfig{UserModel: true, Latency: true}}
		cfg.Metrics = &MetricsConfig{Enabled: true}
	})

	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, roundTripChatRequest(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	got := counting.total()
	t.Logf("AFTER (admin.stats.userModel=true, admin.stats.latency=true, metrics on, no cache): %d counterStore round trips for the SAME single-group chat request", got)
	if got != 4 {
		t.Errorf("round trips with all stats on = %d, want 4 (2 sync + 2 async: checkAndCount, accountWith, recordProviderAttempt, recordLastSeen) — every new counter family rides an EXISTING sync batch except last-seen, which is its own async write by design (P9 fix)", got)
	}
}

// TestRoundTrips_SingleGroupChat_AdminEnabledStatsOff_FourRoundTripsWhenLastSeenDue
// is N4 (verify-redesign-final.md): README's own round-trip table used to
// fold "admin.enabled: false" and "admin.enabled: true with both
// admin.stats opt-ins off" into one "Defaults" row claiming 3 round trips
// for both. That is only true for the first: l.statsAdmin is
// adminEnabled(cfg) alone (llmgateway.go, newConfiguredLimiter) —
// independent of admin.stats.userModel/latency — so admin.enabled: true
// with BOTH opt-ins off still gates every "always-on-with-admin" family
// on, last-seen included. For a scope whose last-seen is DUE (this
// test's case: a fresh limiter, first request for the scope), that is
// recordLastSeen's own async write same as
// TestRoundTrips_SingleGroupChat_AllStatsOn_NoNewSyncRoundTrips — 4 round
// trips, not 3, even though neither opt-in stat is on and there is no
// umodel/latency entry in any batch.
func TestRoundTrips_SingleGroupChat_AdminEnabledStatsOff_FourRoundTripsWhenLastSeenDue(t *testing.T) {
	srv := roundTripTestUpstream(t)
	gw, counting := buildRoundTripTestGateway(t, srv, func(cfg *Config) {
		cfg.Admin = &AdminConfig{Enabled: true} // Stats left nil: userModel/latency both default off
	})

	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, roundTripChatRequest(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	got := counting.total()
	t.Logf("admin.enabled=true, stats.userModel/latency both off, last-seen due: %d counterStore round trips", got)
	if got != 4 {
		t.Errorf("round trips = %d, want 4 (2 sync + 2 async: checkAndCount, accountWith, recordProviderAttempt, recordLastSeen) — admin.enabled alone gates last-seen, not the opt-in stats", got)
	}
}

// TestRoundTrips_SingleGroupChat_CacheHit_AddsExactlyOneAsyncRoundTrip is
// the command table's one documented exception ("cache hit +1 async RT"):
// a response-cache HIT costs checkAndCount's own admission round trip
// (every metered route enforces request-rate limits before it even knows
// whether the response will be a hit) PLUS recordCacheHit's own async
// write — 2 counterStore calls, never account()'s or
// recordProviderAttempt's own (the hit branch returns before ever
// reaching either, since no upstream call was made). This is +1 over
// this feature's own pre-task baseline of "a hit costs only
// checkAndCount, 1 call" — cache stats did not exist before this task.
func TestRoundTrips_SingleGroupChat_CacheHit_AddsExactlyOneAsyncRoundTrip(t *testing.T) {
	srv := roundTripTestUpstream(t)
	redisLn := newBehavioralRedisServer(t)
	gw, counting := buildRoundTripTestGateway(t, srv, func(cfg *Config) {
		cfg.Redis = &RedisConfig{Address: redisLn.Addr().String()}
		cfg.Cache = CacheConfig{Enabled: true, TTL: "1m"}
		cfg.Admin = &AdminConfig{Enabled: true}
	})
	// buildRoundTripTestGateway already swapped gw.limiter.store for a
	// counting wrapper over a fresh memoryStore — the response CACHE
	// itself is a *responseCache (cache.go), not a counterStore: its own
	// Redis calls (via cfg.Redis's real behavioral fake) are never
	// counted here, matching the command table's own "counterStore round
	// trips" scope exactly.

	req1 := roundTripChatRequest(t)
	rec1 := httptest.NewRecorder()
	gw.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK || rec1.Header().Get("X-Llmgw-Cache") != "miss" {
		t.Fatalf("first request: status=%d cache=%q, want 200/miss", rec1.Code, rec1.Header().Get("X-Llmgw-Cache"))
	}
	baseline := counting.total()
	t.Logf("miss (cache+admin on): %d cumulative counterStore round trips", baseline)

	req2 := roundTripChatRequest(t)
	rec2 := httptest.NewRecorder()
	gw.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK || rec2.Header().Get("X-Llmgw-Cache") != "hit" {
		t.Fatalf("second request: status=%d cache=%q, want 200/hit", rec2.Code, rec2.Header().Get("X-Llmgw-Cache"))
	}

	hitOwnCalls := counting.total() - baseline
	t.Logf("hit (cache+admin on): %d counterStore round trips for this ONE request (checkAndCount + recordCacheHit)", hitOwnCalls)
	if hitOwnCalls != 2 {
		t.Errorf("cache-hit round trips = %d, want 2 (checkAndCount + recordCacheHit; no account(), no recordProviderAttempt)", hitOwnCalls)
	}
}
