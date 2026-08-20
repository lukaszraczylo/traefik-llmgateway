package traefikllmgateway

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// --- cacheKey: stability, stripped fields, key material (task 2) ---

// TestCacheKey_StableAcrossMapInsertionOrder proves two requests with
// identical fields, built in different map-literal order, hash to the
// same key — Go's json.Marshal always sorts map[string]any keys, which is
// what canonical-JSON depends on.
func TestCacheKey_StableAcrossMapInsertionOrder(t *testing.T) {
	req1 := map[string]any{"model": "gpt-4", "messages": []any{"a"}, "temperature": 0.7}
	req2 := map[string]any{"temperature": 0.7, "model": "gpt-4", "messages": []any{"a"}}

	k1 := cacheKey("openai", "gpt-4", req1)
	k2 := cacheKey("openai", "gpt-4", req2)
	if k1 != k2 {
		t.Errorf("cacheKey differs by map insertion order: %q vs %q", k1, k2)
	}
}

// TestCacheKey_StripsStreamOptionsAndUser proves stream_options and user
// never influence the key — spec §2's "minus stream_options and user" —
// while every other field still does.
func TestCacheKey_StripsStreamOptionsAndUser(t *testing.T) {
	base := map[string]any{"model": "gpt-4", "messages": []any{"hi"}}

	withExtras := map[string]any{
		"model":          "gpt-4",
		"messages":       []any{"hi"},
		"stream_options": map[string]any{"include_usage": true},
		"user":           "alice",
	}

	if got, want := cacheKey("openai", "gpt-4", base), cacheKey("openai", "gpt-4", withExtras); got != want {
		t.Errorf("cacheKey changed by stream_options/user: %q vs %q", got, want)
	}

	// A genuinely different "user" value must still change nothing.
	otherUser := map[string]any{"model": "gpt-4", "messages": []any{"hi"}, "user": "bob"}
	if got, want := cacheKey("openai", "gpt-4", withExtras), cacheKey("openai", "gpt-4", otherUser); got != want {
		t.Errorf("cacheKey changed by a different user value: %q vs %q", got, want)
	}
}

// TestCacheKey_NeverMutatesLiveRequestMap asserts cacheKey's internal
// stripping works on a copy — the brief's "never mutate the live req".
func TestCacheKey_NeverMutatesLiveRequestMap(t *testing.T) {
	req := map[string]any{"model": "gpt-4", "user": "alice", "stream_options": map[string]any{}}
	_ = cacheKey("openai", "gpt-4", req)

	if _, ok := req["user"]; !ok {
		t.Error("cacheKey removed \"user\" from the live request map; want it left untouched")
	}
	if _, ok := req["stream_options"]; !ok {
		t.Error("cacheKey removed \"stream_options\" from the live request map; want it left untouched")
	}
}

// TestCacheKey_DifferentProviderOrModel_DifferentKey proves provider and
// upstreamModel are real key material, not just documentation: an
// identical req under a different provider or model must hash
// differently, and a well-formed provider/model boundary must not
// collide with an equivalent unseparated string (the \x00 separator's
// entire purpose).
func TestCacheKey_DifferentProviderOrModel_DifferentKey(t *testing.T) {
	req := map[string]any{"model": "gpt-4", "messages": []any{"hi"}}

	base := cacheKey("openai", "gpt-4", req)
	if got := cacheKey("azure", "gpt-4", req); got == base {
		t.Error("cacheKey identical across different providers")
	}
	if got := cacheKey("openai", "gpt-4o", req); got == base {
		t.Error("cacheKey identical across different upstream models")
	}
	// "ab"+"c" vs "a"+"bc" must not collide despite concatenating to the
	// same string — proves the \x00 separator actually separates.
	if got := cacheKey("ab", "c", req); got == cacheKey("a", "bc", req) {
		t.Error("cacheKey collides across a provider/model boundary shift (missing separator)")
	}
}

// TestCacheKey_HasExpectedPrefix pins the key's wire format: spec §2's
// "llmgw:cache:" + hex SHA-256 (64 hex chars).
func TestCacheKey_HasExpectedPrefix(t *testing.T) {
	k := cacheKey("openai", "gpt-4", map[string]any{"model": "gpt-4"})
	if !strings.HasPrefix(k, cacheKeyPrefix) {
		t.Fatalf("cacheKey = %q, want prefix %q", k, cacheKeyPrefix)
	}
	hexPart := strings.TrimPrefix(k, cacheKeyPrefix)
	if len(hexPart) != 64 {
		t.Errorf("hex digest length = %d, want 64 (SHA-256)", len(hexPart))
	}
}

// --- validateCacheConfig: defaults and validation ---

func TestValidateCacheConfig_DefaultsAppliedWhenZero(t *testing.T) {
	ttl, maxBody, err := validateCacheConfig(CacheConfig{Enabled: true})
	if err != nil {
		t.Fatalf("validateCacheConfig: %v", err)
	}
	if want, _ := time.ParseDuration(defaultCacheTTL); ttl != want {
		t.Errorf("ttl = %v, want default %v", ttl, want)
	}
	if maxBody != defaultCacheMaxBodyBytes {
		t.Errorf("maxBody = %d, want default %d", maxBody, defaultCacheMaxBodyBytes)
	}
}

func TestValidateCacheConfig_CustomValuesHonored(t *testing.T) {
	ttl, maxBody, err := validateCacheConfig(CacheConfig{Enabled: true, TTL: "30s", MaxBodyBytes: 2048})
	if err != nil {
		t.Fatalf("validateCacheConfig: %v", err)
	}
	if ttl != 30*time.Second {
		t.Errorf("ttl = %v, want 30s", ttl)
	}
	if maxBody != 2048 {
		t.Errorf("maxBody = %d, want 2048", maxBody)
	}
}

func TestValidateCacheConfig_MalformedTTL_ReturnsError(t *testing.T) {
	if _, _, err := validateCacheConfig(CacheConfig{Enabled: true, TTL: "not-a-duration"}); err == nil {
		t.Fatal("want an error for a malformed TTL")
	}
}

func TestValidateCacheConfig_NegativeMaxBodyBytes_ReturnsError(t *testing.T) {
	if _, _, err := validateCacheConfig(CacheConfig{Enabled: true, MaxBodyBytes: -1}); err == nil {
		t.Fatal("want an error for a negative maxBodyBytes")
	}
}

func TestValidateCacheConfig_MaxBodyBytesOverCap_ReturnsError(t *testing.T) {
	if _, _, err := validateCacheConfig(CacheConfig{Enabled: true, MaxBodyBytes: maxCacheMaxBodyBytes + 1}); err == nil {
		t.Fatal("want an error for maxBodyBytes exceeding the 8MB cap")
	}
}

func TestValidateCacheConfig_MaxBodyBytesAtCap_Allowed(t *testing.T) {
	if _, _, err := validateCacheConfig(CacheConfig{Enabled: true, MaxBodyBytes: maxCacheMaxBodyBytes}); err != nil {
		t.Errorf("validateCacheConfig at the exact 8MB cap: %v, want no error", err)
	}
}

// --- buildResponseCache: disabled / enabled-no-redis-warns / enabled-invalid / working ---

func TestBuildResponseCache_Disabled_ReturnsNilNoError(t *testing.T) {
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	c, err := buildResponseCache(CacheConfig{Enabled: false}, nil, logf, logf)
	if err != nil {
		t.Fatalf("buildResponseCache: %v", err)
	}
	if c != nil {
		t.Error("want a nil *responseCache when Enabled is false")
	}
	if len(logged) != 0 {
		t.Errorf("logged = %v, want no warning when caching is disabled", logged)
	}
}

// TestBuildResponseCache_EnabledNoRedis_WarnsAndDisables is spec §2's
// "no Redis configured → caching silently disabled with one warn at
// construction" — NOT a construction error.
func TestBuildResponseCache_EnabledNoRedis_WarnsAndDisables(t *testing.T) {
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	errorf := func(format string, args ...any) { t.Errorf("unexpected errorf call: "+format, args...) }

	c, err := buildResponseCache(CacheConfig{Enabled: true}, nil, logf, errorf)
	if err != nil {
		t.Fatalf("buildResponseCache: %v, want no error (warn-and-disable, not a constructor error)", err)
	}
	if c != nil {
		t.Error("want a nil *responseCache when Redis is not configured")
	}
	if len(logged) != 1 {
		t.Fatalf("logged = %d lines, want exactly 1 warning", len(logged))
	}
}

func TestBuildResponseCache_EnabledInvalidTTL_ReturnsConstructorError(t *testing.T) {
	noop := func(string, ...any) {}
	if _, err := buildResponseCache(CacheConfig{Enabled: true, TTL: "nope"}, nil, noop, noop); err == nil {
		t.Fatal("want a construction error for an invalid TTL, even with no Redis configured")
	}
}

func TestBuildResponseCache_EnabledWithRedis_ReturnsWorkingCache(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
	})
	client := newRESPClient(ln.Addr().String(), "", 0)

	noop := func(string, ...any) {}
	c, err := buildResponseCache(CacheConfig{Enabled: true, TTL: "1m"}, client, noop, noop)
	if err != nil {
		t.Fatalf("buildResponseCache: %v", err)
	}
	if c == nil {
		t.Fatal("want a non-nil *responseCache when Redis is configured and config is valid")
	}
	if c.ttl != time.Minute {
		t.Errorf("c.ttl = %v, want 1m", c.ttl)
	}
}

// --- responseCache.lookup / store against a fake RESP server ---

// newTestResponseCache builds a responseCache over ln's fake server, with
// nowFn pinned to a fixed instant so logCacheError's rate-limit window is
// deterministic. logged collects every logf call's formatted message.
func newTestResponseCache(t *testing.T, ln net.Listener) (c *responseCache, logged *[]string) {
	t.Helper()
	client := newRESPClient(ln.Addr().String(), "", 0)
	msgs := []string{}
	spy := func(format string, args ...any) { msgs = append(msgs, fmt.Sprintf(format, args...)) }
	c = newResponseCache(client, time.Minute, defaultCacheMaxBodyBytes, spy, spy)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	c.nowFn = func() time.Time { return now }
	return c, &msgs
}

func TestResponseCache_Lookup_Hit(t *testing.T) {
	ln := newFakeListener(t)
	stored := `{"status":200,"contentType":"application/json","body":"aGVsbG8="}` // "hello" base64
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "llmgw:cache:k"}, reply: []byte(fmt.Sprintf("$%d\r\n%s\r\n", len(stored), stored))},
	})
	c, _ := newTestResponseCache(t, ln)

	cr, ok := c.lookup("llmgw:cache:k")
	if !ok {
		t.Fatal("lookup ok = false, want true (hit)")
	}
	if cr.Status != 200 || cr.ContentType != "application/json" || string(cr.Body) != "hello" {
		t.Errorf("cr = %+v, want status=200 contentType=application/json body=hello", cr)
	}
}

func TestResponseCache_Lookup_Miss(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "llmgw:cache:missing"}, reply: []byte("$-1\r\n")},
	})
	c, logged := newTestResponseCache(t, ln)

	if _, ok := c.lookup("llmgw:cache:missing"); ok {
		t.Fatal("lookup ok = true, want false (miss)")
	}
	if len(*logged) != 0 {
		t.Errorf("logged = %v, want no log for a plain miss", *logged)
	}
}

// TestResponseCache_Lookup_RedisDown_MissWithRateLimitedLog is spec §2's
// "redis errors during cache ops → treat as miss, rate-limited log" —
// mirroring TestLimiter_LogsStoreErrorOncePerRateLimit's exact shape
// (limits_test.go).
func TestResponseCache_Lookup_RedisDown_MissWithRateLimitedLog(t *testing.T) {
	ln := newFakeListener(t)
	deadAddr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	client := newRESPClient(deadAddr, "", 0)
	var logged []string
	errorf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	noop := func(string, ...any) {}
	c := newResponseCache(client, time.Minute, defaultCacheMaxBodyBytes, noop, errorf)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	c.nowFn = func() time.Time { return now }

	if _, ok := c.lookup("llmgw:cache:k"); ok {
		t.Fatal("lookup ok = true, want false when redis is unreachable")
	}
	if len(logged) != 1 {
		t.Fatalf("logged = %d lines after the first failed lookup, want 1", len(logged))
	}

	if _, ok := c.lookup("llmgw:cache:k"); ok {
		t.Fatal("lookup ok = true, want false on a second failed lookup")
	}
	if len(logged) != 1 {
		t.Fatalf("logged = %d lines still inside the rate-limit window, want 1", len(logged))
	}

	now = now.Add(storeErrorLogEvery)
	if _, ok := c.lookup("llmgw:cache:k"); ok {
		t.Fatal("lookup ok = true, want false on a third failed lookup")
	}
	if len(logged) != 2 {
		t.Fatalf("logged = %d lines after the rate-limit window elapsed, want 2", len(logged))
	}
}

func TestResponseCache_Lookup_CorruptedValue_MissWithLog(t *testing.T) {
	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"GET", "k"}, reply: []byte("$8\r\nnot-json\r\n")},
	})
	c, logged := newTestResponseCache(t, ln)

	if _, ok := c.lookup("k"); ok {
		t.Fatal("lookup ok = true, want false for a corrupted stored value")
	}
	if len(*logged) != 1 {
		t.Fatalf("logged = %d lines, want 1 for a corrupted value", len(*logged))
	}
}

// TestResponseCache_Store_SendsSETWithTTL proves store sends a SET...EX
// carrying the cache's configured ttl and a JSON value whose "body" field
// is the base64 of the bytes given — the exact wire shape a real Redis
// (or this same client's getBytes+json.Unmarshal) would read back.
func TestResponseCache_Store_SendsSETWithTTL(t *testing.T) {
	wantVal, err := json.Marshal(cachedResponse{Status: 200, ContentType: "application/json", Body: []byte(`{"ok":true}`)})
	if err != nil {
		t.Fatalf("json.Marshal(cachedResponse): %v", err)
	}

	ln := newFakeListener(t)
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
		{wantArgs: []string{"SET", "llmgw:cache:k", string(wantVal), "EX", "90"}, reply: []byte("+OK\r\n")},
	})

	client := newRESPClient(ln.Addr().String(), "", 0)
	noop := func(string, ...any) {}
	c := newResponseCache(client, 90*time.Second, defaultCacheMaxBodyBytes, noop, noop)
	c.nowFn = time.Now

	c.store("llmgw:cache:k", 200, "application/json", []byte(`{"ok":true}`))
}

func TestResponseCache_Store_OversizeBody_SkipsSilentlyLogged(t *testing.T) {
	ln := newFakeListener(t)
	// No SET step scripted at all: if store() sent one, runFakeRESPServer
	// would report a read-command error once the script runs out of
	// steps, failing the test.
	runFakeRESPServer(t, ln, []respStep{
		{wantArgs: []string{"SELECT", "0"}, reply: []byte("+OK\r\n")},
	})
	client := newRESPClient(ln.Addr().String(), "", 0)
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	errorf := func(format string, args ...any) { t.Errorf("unexpected errorf call: "+format, args...) }
	c := newResponseCache(client, time.Minute, 4, logf, errorf) // maxBodyBytes=4
	c.nowFn = time.Now

	c.store("k", 200, "text/plain", []byte("way too big"))

	if len(logged) != 1 {
		t.Fatalf("logged = %d lines, want 1 (oversize skip is logged via logf, not errorf)", len(logged))
	}
}

func TestResponseCache_Store_RedisDown_LoggedRateLimited(t *testing.T) {
	ln := newFakeListener(t)
	deadAddr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	client := newRESPClient(deadAddr, "", 0)
	var logged []string
	errorf := func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	noop := func(string, ...any) {}
	c := newResponseCache(client, time.Minute, defaultCacheMaxBodyBytes, noop, errorf)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	c.nowFn = func() time.Time { return now }

	c.store("k", 200, "application/json", []byte(`{}`))
	if len(logged) != 1 {
		t.Fatalf("logged = %d lines after the first failed store, want 1", len(logged))
	}

	c.store("k", 200, "application/json", []byte(`{}`))
	if len(logged) != 1 {
		t.Fatalf("logged = %d lines still inside the rate-limit window, want 1", len(logged))
	}
}

// --- groupCacheEnabled ---

func TestGroupCacheEnabled_NilInheritsGlobal(t *testing.T) {
	grp := &group{name: "g", cache: nil}
	if !groupCacheEnabled(grp) {
		t.Error("groupCacheEnabled = false, want true (nil inherits the global setting, already known enabled)")
	}
}

func TestGroupCacheEnabled_ExplicitFalse_OptsOut(t *testing.T) {
	no := false
	grp := &group{name: "g", cache: &no}
	if groupCacheEnabled(grp) {
		t.Error("groupCacheEnabled = true, want false (explicit opt-out)")
	}
}

func TestGroupCacheEnabled_ExplicitTrue_OptsIn(t *testing.T) {
	yes := true
	grp := &group{name: "g", cache: &yes}
	if !groupCacheEnabled(grp) {
		t.Error("groupCacheEnabled = false, want true (explicit opt-in)")
	}
}

// --- group-cache-true-without-global-cache: constructor error (auth.go) ---

func TestNewAuthStore_GroupCacheTrueWithoutGlobalCache_ReturnsConstructorError(t *testing.T) {
	yes := true
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"default": {Cache: &yes}},
		// Cache left at its zero value: Enabled=false.
	}
	if _, err := newAuthStore(cfg); err == nil {
		t.Fatal("want a constructor error: group enables cache but global cache is not configured")
	}
}

func TestNewAuthStore_GroupCacheFalseWithoutGlobalCache_NoError(t *testing.T) {
	no := false
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"default": {Cache: &no}},
	}
	if _, err := newAuthStore(cfg); err != nil {
		t.Fatalf("newAuthStore: %v, want no error (Cache:false never requires the global block)", err)
	}
}

func TestNewAuthStore_GroupCacheTrueWithGlobalCacheEnabled_NoError(t *testing.T) {
	yes := true
	cfg := &Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"default": {Cache: &yes}},
		Cache:     CacheConfig{Enabled: true},
	}
	if _, err := newAuthStore(cfg); err != nil {
		t.Fatalf("newAuthStore: %v, want no error when the global cache block is configured", err)
	}
}
