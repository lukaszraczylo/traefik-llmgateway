package traefikllmgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// cacheKeyPrefix prefixes every response-cache key (spec §2).
const cacheKeyPrefix = "llmgw:cache:"

// defaultCacheTTL and defaultCacheMaxBodyBytes are the values
// validateCacheConfig applies when a Cache config block is Enabled but
// leaves TTL or MaxBodyBytes at its zero value — spec §2's stated
// defaults.
const (
	defaultCacheTTL          = "5m"
	defaultCacheMaxBodyBytes = 1 << 20 // 1MiB
)

// maxCacheMaxBodyBytes is the highest CacheConfig.MaxBodyBytes
// validateCacheConfig accepts — spec §2's "maxBodyBytes ... max 8MB".
const maxCacheMaxBodyBytes = 8 << 20 // 8MiB

// validateCacheConfig validates cc and returns the ttl and maxBodyBytes
// buildResponseCache passes to newResponseCache. Called only when
// cc.Enabled — an unset TTL/MaxBodyBytes on a disabled block is never an
// error, since neither value is ever read, matching newRetryPolicy's
// identical disabled-skips-validation shape (retry.go).
func validateCacheConfig(cc CacheConfig) (time.Duration, int, error) {
	ttlStr := cc.TTL
	if ttlStr == "" {
		ttlStr = defaultCacheTTL
	}
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		return 0, 0, fmt.Errorf("llmgateway: cache.ttl: %w", err)
	}

	maxBody := cc.MaxBodyBytes
	if maxBody == 0 {
		maxBody = defaultCacheMaxBodyBytes
	}
	if maxBody < 0 {
		return 0, 0, fmt.Errorf("llmgateway: cache.maxBodyBytes must be positive, got %d", maxBody)
	}
	if maxBody > maxCacheMaxBodyBytes {
		return 0, 0, fmt.Errorf("llmgateway: cache.maxBodyBytes must not exceed %d, got %d", maxCacheMaxBodyBytes, maxBody)
	}
	return ttl, maxBody, nil
}

// cacheStore is the minimal interface responseCache needs from its Redis
// backend — satisfied structurally by *respClient (resp.go), with no
// changes needed there. Abstracted so tests can inject a call-counting
// stub in place of a real respClient, to prove the down-latch below
// (recordFailure/latched) skips the network entirely rather than merely
// tolerating repeated failures.
type cacheStore interface {
	setEx(key string, val []byte, ttl time.Duration) error
	getBytes(key string) ([]byte, bool, error)
}

// buildResponseCache constructs the *responseCache newGateway attaches to
// its Gateway, or nil when caching is not configured or not usable:
//
//   - cc.Enabled is false (the default): nil, no error — v0.1 behavior.
//   - cc.Enabled is true but its TTL/MaxBodyBytes fail validation: a
//     construction error, matching every other malformed-config path.
//   - cc.Enabled is true and valid, but client is nil (config.Redis was
//     absent): nil, with one warning logged via logf — per spec §2,
//     "requires Redis... no Redis configured → caching silently disabled
//     with one warn at construction (memory fallback is NOT used for the
//     cache: per-replica caches would serve divergent responses)". This
//     is deliberately NOT a construction error, unlike the case above.
//   - cc.Enabled is true and valid and client is non-nil: a working
//     *responseCache over client.
func buildResponseCache(cc CacheConfig, client *respClient, logf, errorf func(format string, args ...any)) (*responseCache, error) {
	if !cc.Enabled {
		return nil, nil
	}
	ttl, maxBody, err := validateCacheConfig(cc)
	if err != nil {
		return nil, err
	}
	if client == nil {
		logf("cache: enabled but redis is not configured; response caching disabled")
		return nil, nil
	}
	return newResponseCache(client, ttl, maxBody, logf, errorf), nil
}

// groupCacheEnabled resolves whether grp's requests are cacheable, given
// that caching is already known to be globally enabled and usable — every
// call site checks g.cache != nil first (routes_unified.go), and g.cache
// is non-nil only when buildResponseCache's cc.Enabled was true, so
// "inherit the global setting" here is unconditionally true rather than a
// second config lookup. grp.cache is GroupConfig.Cache carried onto the
// group by newAuthStore (auth.go): nil inherits (true), non-nil overrides.
func groupCacheEnabled(grp *group) bool {
	if grp.cache != nil {
		return *grp.cache
	}
	return true
}

// groupCacheTTL parses and validates a GroupConfig.CacheTTL string for
// group name, returning the resolved time.Duration — 0 when raw is empty,
// meaning "inherit the global cache TTL" (effectiveTTL below). cacheEnabled
// is cfg.Cache.Enabled: a non-empty raw while the global cache block is not
// configured is a constructor error, since there is then no global TTL to
// override — the identical "nothing to inherit from" reasoning newAuthStore
// already applies to Cache:true (auth.go). Called once per group by
// newAuthStore (auth.go), independent of that group's own Cache setting.
func groupCacheTTL(name, raw string, cacheEnabled bool) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	if !cacheEnabled {
		return 0, fmt.Errorf("llmgateway: group %q sets cacheTTL but global cache is not configured", name)
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("llmgateway: group %q: cacheTTL: %w", name, err)
	}
	if ttl <= 0 {
		return 0, fmt.Errorf("llmgateway: group %q: cacheTTL must be positive, got %q", name, raw)
	}
	return ttl, nil
}

// effectiveTTL resolves the TTL to use for grp's cache entries: grp's own
// resolved cacheTTL when set (non-zero — GroupConfig.CacheTTL parsed and
// validated at construction, groupCacheTTL above), otherwise c's global TTL
// (buildResponseCache's cc.TTL, validateCacheConfig). Called by
// routes_unified.go's runUnified immediately before store, so every stored
// entry's Redis-native expiry (setEx's ttl argument, resp.go) already
// reflects the group-specific override — no separate "shrink the group's
// TTL later" step exists.
func effectiveTTL(c *responseCache, grp *group) time.Duration {
	if grp.cacheTTL > 0 {
		return grp.cacheTTL
	}
	return c.ttl
}

// cachedResponse is the value responseCache stores per key: enough to
// replay a cached response verbatim. json.Marshal/Unmarshal base64-encode
// Body automatically (it is a []byte field), matching spec §2's "value:
// JSON {status, contentType, body(base64)}".
type cachedResponse struct {
	ContentType string `json:"contentType"`
	Body        []byte `json:"body"`
	Status      int    `json:"status"`
}

// responseCache is the opt-in, Redis-backed cache for unified
// non-streaming chat/embeddings responses (spec §2). A nil *responseCache
// on *Gateway means caching is off; every method below assumes a non-nil
// receiver, so every call site (routes_unified.go) checks g.cache != nil
// first rather than this type having its own always-disabled behavior.
//
// Concurrent identical misses (a stampede) are not deduplicated: N
// requests for the same uncached key that arrive close together all miss,
// all call the upstream, and all SET the same key — accepted for v0.2. A
// singleflight-style dedup would save upstream calls under bursty
// concurrent identical traffic, but adds real complexity (a per-key
// in-flight map, cancellation semantics if the leader request's client
// disconnects) for a case that only wastes cost/latency, never
// correctness — every request still gets a right answer, just not always
// a cached one.
type responseCache struct {
	// client is a cacheStore, not a concrete *respClient: buildResponseCache
	// always passes a *respClient (Go satisfies the interface implicitly),
	// but tests inject a call-counting stub to prove the down-latch below
	// skips the network entirely.
	client cacheStore
	// logf is informational — construction warnings (buildResponseCache)
	// and store's oversize-skip notice — matching spec §2's "construction-
	// time WARN via logf" and "skip store silently-with-debug-log": both
	// are routine, expected outcomes, not failures.
	logf func(format string, args ...any)
	// errorf is for logCacheError only: a genuine Redis failure, logged
	// at error level and rate-limited, mirroring limiter.logStoreError's
	// "existing pattern" (limits.go) — g.errorf there, g.errorf here.
	errorf func(format string, args ...any)
	nowFn  func() time.Time // injected for tests; defaults to time.Now
	// lastLogAt, lastFailure, and logMu together implement the same two
	// mechanisms as limiter's logStoreError/storeLatched (limits.go),
	// reusing storeErrorLogEvery and storeDownLatchFor rather than
	// duplicating them:
	//   - lastLogAt rate-limits logCacheError to once per
	//     storeErrorLogEvery, so a Redis outage does not flood the log.
	//   - lastFailure opens the down-latch (see latched/recordFailure):
	//     for storeDownLatchFor after any GET/SET error, lookup/store
	//     skip the network call entirely rather than paying a fresh
	//     respCallTimeout to rediscover the same outage on every request
	//     — the amplification a shared-connection cache and limiter would
	//     otherwise cause together during an outage.
	lastLogAt    time.Time
	lastFailure  time.Time
	ttl          time.Duration
	maxBodyBytes int
	logMu        sync.Mutex
}

// newResponseCache returns a responseCache backed by client, storing
// entries for ttl and refusing to store a body larger than maxBodyBytes.
func newResponseCache(client cacheStore, ttl time.Duration, maxBodyBytes int, logf, errorf func(format string, args ...any)) *responseCache {
	return &responseCache{client: client, ttl: ttl, maxBodyBytes: maxBodyBytes, logf: logf, errorf: errorf, nowFn: time.Now}
}

// logCacheError logs err via c.errorf, rate-limited to once per
// storeErrorLogEvery regardless of how many cache operations fail in
// between — a Redis outage under load must not flood the log with one
// line per request, the same reasoning limiter.logStoreError (limits.go)
// applies to store errors.
func (c *responseCache) logCacheError(err error) {
	now := c.nowFn()
	c.logMu.Lock()
	shouldLog := now.Sub(c.lastLogAt) >= storeErrorLogEvery
	if shouldLog {
		c.lastLogAt = now
	}
	c.logMu.Unlock()
	if shouldLog {
		c.errorf("response cache: redis error: %v", err)
	}
}

// latched reports whether c is within storeDownLatchFor of its last
// recorded GET/SET failure — mirrors limiter.storeLatched (limits.go).
// While true, lookup/store skip the network call entirely rather than
// paying a fresh respCallTimeout to rediscover an outage already known.
func (c *responseCache) latched() bool {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	return !c.lastFailure.IsZero() && c.nowFn().Sub(c.lastFailure) < storeDownLatchFor
}

// recordFailure logs err (rate-limited, see logCacheError) and opens the
// down-latch: every lookup/store call for the next storeDownLatchFor
// skips the network call entirely — mirrors limiter.recordStoreFailure
// (limits.go). Callers pass only a genuine transport/protocol error from
// c.client (getBytes/setEx) here — a decoded-but-corrupted cached value is
// not a Redis failure and must not latch the whole cache down over one
// bad entry (lookup logs that case via logCacheError directly instead).
func (c *responseCache) recordFailure(err error) {
	c.logCacheError(err)
	c.logMu.Lock()
	c.lastFailure = c.nowFn()
	c.logMu.Unlock()
}

// lookup returns the cached response for key. ok is false on a cache
// miss — a real miss (key absent), the down-latch already open (latched,
// no network call attempted), a Redis error (logged, rate-limited, and
// latches the cache down for storeDownLatchFor), or a corrupted stored
// value (logged, but does NOT latch — the store answered fine, one entry
// was just bad) — none of which distinguish themselves to the caller: per
// spec §2, "redis errors during cache ops → treat as miss ... never fail
// the request because the cache is down".
func (c *responseCache) lookup(key string) (*cachedResponse, bool) {
	if c.latched() {
		return nil, false
	}

	raw, found, err := c.client.getBytes(key)
	if err != nil {
		c.recordFailure(err)
		return nil, false
	}
	if !found {
		return nil, false
	}

	var cr cachedResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		c.logCacheError(fmt.Errorf("decode cached value for %q: %w", key, err))
		return nil, false
	}
	return &cr, true
}

// store saves status/contentType/body under key with ttl (the caller's
// resolved TTL — effectiveTTL above; every existing caller before the
// per-group override passed c.ttl itself, so behavior for a group with no
// override is unchanged). A body exceeding maxBodyBytes is skipped (logged,
// not treated as an error) before ever touching the network; a down-latch
// already open (latched) also skips the network entirely; and a Redis
// error writing it is logged (rate-limited) and opens the latch — none of
// which surface to the caller: a cache write must never fail a request
// that already succeeded and was already written to the client.
func (c *responseCache) store(key string, status int, contentType string, body []byte, ttl time.Duration) {
	if len(body) > c.maxBodyBytes {
		// Pre-formatted with fmt.Sprintf, then logged as a single "%s"
		// argument, not c.logf(format, key, len(body), c.maxBodyBytes)
		// directly: yaegi v0.16.1's CFG builder panics ("index out of
		// range") compiling a call to a struct FIELD of variadic func
		// type with 2+ variadic arguments (see modelRegistry.log's
		// identical note, registry.go) — c.logf is exactly such a field.
		msg := fmt.Sprintf("response cache: skipping store for %q: body %d bytes exceeds maxBodyBytes %d", key, len(body), c.maxBodyBytes)
		c.logf("%s", msg)
		return
	}
	if c.latched() {
		return
	}

	val, err := json.Marshal(cachedResponse{Status: status, ContentType: contentType, Body: body})
	if err != nil {
		// cachedResponse's fields are a plain int, string, and []byte —
		// every one of json.Marshal's own always-succeeds types. Reaching
		// here would be a programming error, not a runtime condition to
		// recover from.
		panic(fmt.Sprintf("llmgateway: cache store: marshal cached response: %v", err))
	}

	if err := c.client.setEx(key, val, ttl); err != nil {
		c.recordFailure(err)
	}
}

// cacheEndpointChat and cacheEndpointEmbeddings are cacheKey's endpoint
// argument, one per cacheable route (routes_unified.go's handleChat/
// handleEmbeddings) — keeping /v1/chat/completions and /v1/embeddings
// requests that happen to canonicalize identically (same provider,
// upstream model, and stripped body) from colliding into one cache entry.
const (
	cacheEndpointChat       = "chat"
	cacheEndpointEmbeddings = "embeddings"
)

// cacheKey derives the response-cache key for one request: controller-
// approved amendment to spec §2's original definition (design doc §2),
// adding requestedModel and endpoint as two further \x00-separated
// components ahead of the canonical-JSON: "llmgw:cache: + hex SHA-256 of
// provider \x00 upstreamModel \x00 requestedModel \x00 endpoint \x00
// canonical-JSON(request body minus stream_options and user)".
//
// requestedModel is the client's own, exactly-as-sent model string (e.g.
// "claude-x" or "anthropic/claude-x") — NOT stripped from req, and passed
// as a separate argument rather than left in req, because a translating
// adapter (anthropic, gemini) bakes gatewayAliasKey's echoed alias
// straight into the cached response body's own "model" field
// (translate_anthropic.go, translate_gemini.go): two clients requesting
// the same upstream model under different alias forms must never collide
// into one entry, or the second client would receive a response whose
// "model" field is an id it never sent. endpoint distinguishes
// /v1/chat/completions from /v1/embeddings, so a canonically-identical
// body under each route (however unlikely) still keys separately.
//
// req is never mutated — stripping builds a fresh copy — and
// canonical-JSON is plain json.Marshal of that copy: encoding/json always
// marshals a map[string]any's keys in sorted order, which is what makes
// the result deterministic across requests carrying the same fields in
// different insertion order.
//
// Callers must pass req before injecting gatewayAliasKey
// (routes_unified.go): that key is a purely internal echo-back mechanism
// with no bearing on upstream request equivalence beyond what
// requestedModel above already captures, so leaving it in req would only
// add noise, not distinguishing power.
func cacheKey(provider, upstreamModel, requestedModel, endpoint string, req map[string]any) string {
	stripped := make(map[string]any, len(req))
	for k, v := range req {
		if k == "stream_options" || k == "user" {
			continue
		}
		stripped[k] = v
	}

	canonical, err := json.Marshal(stripped)
	if err != nil {
		// stripped holds only values encoding/json.Unmarshal ever
		// produces into a map[string]any (nil, bool, float64, string,
		// []any, map[string]any) — every one of Marshal's own
		// always-succeeds types. Reaching here would be a programming
		// error, not a runtime condition to recover from.
		panic(fmt.Sprintf("llmgateway: cacheKey: marshal stripped request: %v", err))
	}

	h := sha256.New() // hash.Hash.Write never returns an error, per its doc
	_, _ = h.Write([]byte(provider))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(upstreamModel))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(requestedModel))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(endpoint))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(canonical)
	return cacheKeyPrefix + hex.EncodeToString(h.Sum(nil))
}
