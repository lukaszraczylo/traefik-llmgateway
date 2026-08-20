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
type responseCache struct {
	client *respClient
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
	// lastLogAt and logMu rate-limit logCacheError to once per
	// storeErrorLogEvery (limits.go), mirroring limiter.logStoreError —
	// the same constant, so a Redis outage affecting both the limiter and
	// the cache does not double the log volume a limiter-only outage
	// would produce.
	lastLogAt    time.Time
	ttl          time.Duration
	maxBodyBytes int
	logMu        sync.Mutex
}

// newResponseCache returns a responseCache backed by client, storing
// entries for ttl and refusing to store a body larger than maxBodyBytes.
func newResponseCache(client *respClient, ttl time.Duration, maxBodyBytes int, logf, errorf func(format string, args ...any)) *responseCache {
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

// lookup returns the cached response for key. ok is false on a cache
// miss — a real miss (key absent), a Redis error (logged, rate-limited),
// or a corrupted stored value (also logged) — none of which distinguish
// themselves to the caller: per spec §2, "redis errors during cache ops →
// treat as miss ... never fail the request because the cache is down",
// and a corrupted entry is handled the same way for the same reason.
func (c *responseCache) lookup(key string) (*cachedResponse, bool) {
	raw, found, err := c.client.getBytes(key)
	if err != nil {
		c.logCacheError(err)
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

// store saves status/contentType/body under key with the cache's
// configured TTL. A body exceeding maxBodyBytes is skipped (logged, not
// treated as an error), and a Redis error writing it is logged
// (rate-limited via logCacheError) rather than surfaced — a cache write
// must never fail a request that already succeeded and was already
// written to the client.
func (c *responseCache) store(key string, status int, contentType string, body []byte) {
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

	val, err := json.Marshal(cachedResponse{Status: status, ContentType: contentType, Body: body})
	if err != nil {
		// cachedResponse's fields are a plain int, string, and []byte —
		// every one of json.Marshal's own always-succeeds types. Reaching
		// here would be a programming error, not a runtime condition to
		// recover from.
		panic(fmt.Sprintf("llmgateway: cache store: marshal cached response: %v", err))
	}

	if err := c.client.setEx(key, val, c.ttl); err != nil {
		c.logCacheError(err)
	}
}

// cacheKey derives the response-cache key for one request: spec §2's
// "llmgw:cache: + hex SHA-256 of provider \x00 upstreamModel \x00
// canonical-JSON(request body minus stream_options and user)". req is
// never mutated — stripping builds a fresh copy — and canonical-JSON is
// plain json.Marshal of that copy: encoding/json always marshals a
// map[string]any's keys in sorted order, which is what makes the result
// deterministic across requests carrying the same fields in different
// insertion order.
//
// Callers must pass req before injecting gatewayAliasKey
// (routes_unified.go): that key is a purely internal echo-back mechanism
// with no bearing on upstream request equivalence, so two different
// aliases resolving to the same upstream model must hash identically —
// stripping stream_options and user, as this function does, is not
// enough on its own if the alias key were still present.
func cacheKey(provider, upstreamModel string, req map[string]any) string {
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
	_, _ = h.Write(canonical)
	return cacheKeyPrefix + hex.EncodeToString(h.Sum(nil))
}
