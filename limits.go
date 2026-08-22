package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"
)

// storeErrorLogEvery bounds how often the limiter logs a configured
// store's operation error: once per this interval, regardless of how many
// operations fail in between. A backend outage under load must not flood
// the log with one line per request.
const storeErrorLogEvery = 30 * time.Second

// storeDownLatchFor bounds how long the limiter avoids a configured store
// after any operation against it fails: for this long after the failure,
// every store operation short-circuits straight to the fail-open/
// fail-closed policy without attempting the network call at all. Each
// respClient call is already bounded to ~respCallTimeout on its own
// (resp.go), but one request can still touch several counters (its own
// and its group's, request/token/cost, day/month) — without this latch,
// every one of those pays a fresh bounded wait against a store that just
// told the previous op it was unreachable. After the window, the next
// operation probes the real store again: a natural half-open retry, no
// separate recovery state needed.
const storeDownLatchFor = 5 * time.Second

// Window identifiers used throughout windowKey, counter metrics, and
// retry-after calculation. windowHour is stats-only (v0.2 data-layer
// task, see hourWindowTTL): checkAndCount/account both write it, but no
// LimitsConfig field ever names it, so checkAndCount has nothing to
// evaluate it against — GET /admin/api/usage/history (admin.go) is its
// only reader.
const (
	windowHour  = "hour"
	windowMin   = "min"
	windowDay   = "day"
	windowMonth = "month"
)

// Counter metric names embedded in windowKey. metricTokIn/metricTokOut
// replace a single combined "tok" metric (v0.2 data-layer task, folding
// the operator's tokens-in/tokens-out split directive): account records
// usage.prompt under metricTokIn and usage.completion under metricTokOut
// separately, so the admin usage/history APIs can chart each direction on
// its own. A TokensPerDay/TokensPerMonth limit still enforces a TOTAL
// budget across both — see checkAndCount's own probe loop and
// buildBudgetProbes' tokens field, below.
const (
	metricReq    = "req"
	metricTokIn  = "tokin"
	metricTokOut = "tokout"
	metricCost   = "cost"
)

// TTLs applied to counter keys. They exceed their window's natural length
// so a key stays readable for the whole window it belongs to; the bucket
// embedded in the key (see windowKey) already makes a rolled-over window
// use a different key, so these TTLs only bound how long a stale key
// lingers before an opportunistic sweep (memoryStore) or Redis itself
// reclaims it. dayWindowTTL/monthWindowTTL were bumped from their
// original 25h/32d (v0.2 data-layer task): the admin usage-history API's
// day/month charts need a bucket to stay readable for the whole
// retention span a chart can request (historyMaxSpan, admin.go — 35
// daily, 13 monthly buckets), not just long enough to survive its own
// single rollover. The TTL > window-length invariant this comment
// already documented still holds for these consts exactly as applied to
// Redis (35d > 1d, 400d > ~31d, 48h > 1h, 2m > 1m).
//
// That invariant does NOT automatically extend to what memoryStore
// actually applies, though: its own ceiling (memoryStoreMaxTTL, 48h,
// applied only to bound the in-process fallback's live-key count) is
// shorter than a day or month window's natural length on its own. A
// round-1 fix (perf review, 2026-08-21) clamped every fallback TTL to
// that ceiling unconditionally, which broke the invariant for month —
// silently turning TokensPerMonth/CostPerMonthUSD into a rolling ~48h
// budget under the fallback. Round 2 restores it: memoryStore's clampTTL
// never cuts a TTL below enforceTTLFor's window-specific floor, so the
// EFFECTIVE TTL memoryStore ever applies still exceeds its window's
// natural length, exactly like these consts do for Redis — see
// clampTTL/enforceTTLFor/memoryStoreMaxTTL's own doc comments for the
// mechanism.
const (
	minWindowTTL   = 2 * time.Minute
	hourWindowTTL  = 48 * time.Hour
	dayWindowTTL   = 35 * 24 * time.Hour
	monthWindowTTL = 400 * 24 * time.Hour
)

// usdToMicroFactor scales a USD amount to micro-USD (1e6 micro-USD per
// USD), matching the integer accounting unit used by account and the
// counterStore.
const usdToMicroFactor = 1_000_000

// usdToMicros converts a USD amount to micro-USD, rounded to the nearest
// micro-USD. A bare int64(usd*usdToMicroFactor) truncates rather than
// rounds — float64 can represent a value like 8.2 as very slightly under
// its true value, so a truncating cast turns an intended $8.20 limit into
// 8_199_999 micro-USD instead of 8_200_000, one micro-USD too strict.
func usdToMicros(usd float64) int64 {
	return int64(math.Round(usd * usdToMicroFactor))
}

// validate reports an error if lc holds a negative int64 count limit, or a
// cost limit that is negative, NaN, or ±Inf. A nil lc (no limits
// configured) is valid. Without this check, a negative int64 limit would
// silently behave as unlimited — every enforcement path below treats
// limit<=0 as "no limit" — and a NaN/Inf cost limit would propagate
// through usdToMicros into meaningless comparisons instead of failing
// loudly at construction time. Called by newAuthStore for every group's
// and every user's LimitsConfig, so a malformed limit is a constructor
// error rather than a silent misconfiguration.
func (lc *LimitsConfig) validate() error {
	if lc == nil {
		return nil
	}
	counts := []struct {
		name string
		v    int64
	}{
		{"requestsPerMinute", lc.RequestsPerMinute},
		{"requestsPerDay", lc.RequestsPerDay},
		{"tokensPerDay", lc.TokensPerDay},
		{"tokensPerMonth", lc.TokensPerMonth},
	}
	for _, c := range counts {
		if c.v < 0 {
			return fmt.Errorf("llmgateway: limits: %s must not be negative, got %d", c.name, c.v)
		}
	}

	costs := []struct {
		name string
		v    float64
	}{
		{"costPerDayUSD", lc.CostPerDayUSD},
		{"costPerMonthUSD", lc.CostPerMonthUSD},
	}
	for _, c := range costs {
		if math.IsNaN(c.v) || math.IsInf(c.v, 0) {
			return fmt.Errorf("llmgateway: limits: %s must be a finite number, got %v", c.name, c.v)
		}
		if c.v < 0 {
			return fmt.Errorf("llmgateway: limits: %s must not be negative, got %v", c.name, c.v)
		}
	}
	return nil
}

// counterIncr is one entry in an incrMulti batch: increment key by delta,
// creating it with an expiry of ttl from now if it does not exist or has
// expired — the same per-entry contract incrBy applies to a single key.
type counterIncr struct {
	key   string
	delta int64
	ttl   time.Duration
	// enforceTTL is the minimum TTL window's own enforcement correctness
	// requires (enforceTTLFor) — the floor memoryStore's clampTTL never
	// cuts below, even when memoryStoreMaxTTL's history-retention ceiling
	// is lower (review round 2, 2026-08-21, fixing a bug in round 1's
	// single-floor clamp: unconditionally capping every TTL at 48h turned
	// TokensPerMonth/CostPerMonthUSD into a rolling ~48h budget under the
	// fallback, since a month counter's own key would expire and reset
	// roughly every two days instead of once a month). redisStore ignores
	// this field entirely — ttl is applied in full there; the ceiling and
	// this floor only ever matter for the in-process fallback.
	enforceTTL time.Duration
}

// newCounterIncr builds one counterIncr for (kind, id, metric, window) at
// t: key from windowKey, delta and the history-retention ttl exactly as
// the caller gives them, and enforceTTL derived from window itself
// (enforceTTLFor). checkAndCount and account build every entry through
// this one helper so a (window, ttl) pair can never reach incrMulti
// without its matching enforcement floor.
func newCounterIncr(kind, id, metric, window string, t time.Time, delta int64, ttl time.Duration) counterIncr {
	return counterIncr{key: windowKey(kind, id, metric, window, t), delta: delta, ttl: ttl, enforceTTL: enforceTTLFor(window)}
}

// counterStore is the storage backend the limiter uses for atomic windowed
// counters. memoryStore (below) is the in-process fallback; a distributed
// (e.g. Redis-backed) implementation is wired in a later task.
type counterStore interface {
	// incrBy adds n to key's counter, creating it with an expiry of ttl
	// from now if it does not exist or has expired, and returns the
	// counter's new value. incrBy has no production caller since
	// checkAndCount/account moved to the batched incrMulti below (perf
	// review, 2026-08-21) — it is exercised directly by 11 test call
	// sites (memoryStore.incrBy and redisStore.incrBy, across
	// limits_test.go and redis_store_test.go) and kept for counterStore
	// interface conformance. The limiter-level single-key seeding helper
	// is incrCounter (5 call sites, all in admin_test.go's
	// TestAdminUsage_MathAgainstSeededCounters).
	incrBy(key string, n int64, ttl time.Duration) (int64, error)
	// get returns key's current counter value, or 0 if it does not exist
	// or has expired.
	get(key string) (int64, error)
	// getMulti reads every key in keys in one round trip where the
	// backend supports it (redisStore pipelines via respClient.getBatch;
	// memoryStore's in-process map needs no such optimization but
	// implements the same contract for interface conformance), returning
	// one value per key in the same order — 0 for a key that does not
	// exist or has expired, matching get's contract. An error fails the
	// whole batch (mirrored by the limiter's storeGetMulti as a single
	// fail-open/fail-closed decision), never a partial result.
	getMulti(keys []string) ([]int64, error)
	// incrMulti applies incrBy's own per-entry contract to every entry in
	// entries in one round trip where the backend supports it (redisStore
	// pipelines one INCRBY+EXPIRE pair per entry into a single pipeline
	// call; memoryStore's in-process map needs no such optimization but
	// implements the same contract for interface conformance), returning
	// each entry's new counter value in the same order (perf review,
	// 2026-08-21: account previously paid one round trip per counter — up
	// to 27 for a 3-scope request with all three metrics nonzero — this
	// collapses it down to one; checkAndCount's own increments moved on to
	// incrAndGetMulti below, perf review round 3, 2026-08-22). An error
	// fails the whole batch, never a partial result, mirrored by the
	// limiter's storeIncrMulti as a single fail-open/fail-closed decision,
	// the same shape storeGetMulti already applies to a batch read.
	incrMulti(entries []counterIncr) ([]int64, error)
	// incrAndGetMulti applies incrMulti's own per-entry contract to every
	// entry in entries AND reads every key in reads, in ONE round trip
	// where the backend supports it (redisStore pipelines every entry's
	// INCRBY+EXPIRE pair together with every read key's GET into a single
	// pipeline call; memoryStore's in-process map needs no such
	// optimization but implements the same contract for interface
	// conformance) — fusing checkAndCount's request-counter increments
	// with its own token/cost budget reads into the single round trip
	// that used to cost up to 9 separate ones for a 2-scope request, both
	// fully limited (perf review round 3, 2026-08-22: reads carries the
	// tokin/tokout/cost keys checkAndCount's own evaluateScope helper used
	// to read via its own separate storeGetMulti calls — see
	// buildBudgetProbes, below). This is safe because every read key is
	// DISJOINT from every entry's key (req:* vs tokin:*/tokout:*/cost:*),
	// and tokens/cost are only ever WRITTEN by account() after the
	// upstream response completes — never by this call — so there is no
	// read-after-write ordering hazard in reading them alongside the
	// increments. Returns incrMulti's own per-entry post-increment values
	// and getMulti's own per-key values (0 for a missing/expired key),
	// each in entries'/reads' own order. An error fails the whole batch,
	// mirrored by the limiter's storeIncrAndGetMulti as one
	// fail-open/fail-closed decision, the same shape storeIncrMulti/
	// storeGetMulti already apply to their own separate batches.
	incrAndGetMulti(entries []counterIncr, reads []string) (incrVals, readVals []int64, err error)
}

// memoryEntry is one counter's value and expiry in memoryStore.
type memoryEntry struct {
	expiry time.Time
	value  int64
}

// sweepEvery is the minimum real time between incrBy's opportunistic
// expired-entry sweeps, regardless of how many keys memoryStore holds. A
// size-gated sweep (only above N keys) was measured to scan the whole map
// on every incrBy once live keys exceeded that threshold, since a sweep
// that frees nothing (all keys still live) never brings the count back
// down — a 167x incrBy cliff. Gating on elapsed time instead bounds sweep
// frequency independent of key count: worst case is one full-map scan
// every 30s.
const sweepEvery = 30 * time.Second

// memoryStoreMaxTTL caps how long memoryStore keeps a counter alive
// purely for HISTORY-chart purposes, when that is safe to shorten.
// Ruling (perf review round 1, 2026-08-21): the in-process fallback's
// long-tail retention is continuity for charting, not enforcement itself
// — applying the real hourWindowTTL/dayWindowTTL/monthWindowTTL values
// (48h/35d/400d) to the fallback map was measured to grow its live key
// count roughly 30x over the original three-window (min/day/month)
// shape, and sweepEvery's own doc comment above already records that a
// full-map sweep which frees nothing is a "167x incrBy cliff" once live
// keys cross a threshold — growing the map ~30x bigger multiplies that
// same cliff's cost by roughly the same factor.
//
// This ceiling must never win over a window's own enforcement floor,
// though (round 2, 2026-08-21 — corrects a round-1 bug): clampTTL applies
// this ceiling AND enforceTTLFor's floor together, and the floor always
// wins where the two would conflict. Concretely: min/hour/day windows sit
// entirely under 48h already, so they are unaffected; month's own floor
// (enforceTTLFor's 32*24h) sits above this ceiling, so a month counter on
// the fallback keeps its full ~32-day life (1-2 live keys per scope/
// metric — negligible next to the day-key savings that motivated this
// ceiling in the first place) instead of expiring every 48h. Only the
// CHARTING tail — a fallback-only deployment's usage-history buckets
// beyond 48h — is what this ceiling actually shortens; see README's
// "fallback retention" note.
const memoryStoreMaxTTL = 48 * time.Hour

// enforceTTLFor returns the minimum TTL window's own counter must stay
// alive for to enforce correctly, independent of how long history
// retention (hourWindowTTL/dayWindowTTL/monthWindowTTL) asks the same key
// to live for charting. These are windowLength + margin, and not
// coincidentally match dayWindowTTL/monthWindowTTL's ORIGINAL,
// pre-history-retention-bump values (25h/32d, v0.2 data-layer task) —
// that bump only ever existed to serve the usage-history API's charts;
// enforcement itself only ever needed a key to outlive its own window's
// single rollover. Called by newCounterIncr for every checkAndCount/
// account entry; a window not among the four handled here is a
// programming error, mirroring windowKey/windowEnd/bucketFor's own panic
// convention.
func enforceTTLFor(window string) time.Duration {
	switch window {
	case windowMin:
		return minWindowTTL
	case windowHour:
		return 2 * time.Hour
	case windowDay:
		return 25 * time.Hour
	case windowMonth:
		return 32 * 24 * time.Hour
	default:
		panic(fmt.Sprintf("llmgateway: enforceTTLFor: unknown window %q", window))
	}
}

// clampTTL bounds ttl to at most memoryStoreMaxTTL, but never below
// enforceTTL — see both consts'/enforceTTLFor's own doc comments for why
// the floor must always win a conflict with the ceiling. enforceTTL=0
// (incrBy's own single-key path, which has no window to derive a floor
// from) means "ceiling only, no floor" — identical to memoryStore's
// pre-round-2 behavior for that path.
func clampTTL(ttl, enforceTTL time.Duration) time.Duration {
	if ttl > memoryStoreMaxTTL {
		ttl = memoryStoreMaxTTL
	}
	if ttl < enforceTTL {
		ttl = enforceTTL
	}
	return ttl
}

// memoryStore is an in-process counterStore: a mutex-guarded map with
// per-key expiry. It has no background goroutine — a Yaegi middleware
// instance is rebuilt on every config reload, and any ticker or goroutine
// started here would leak on rebuild instead of being collected with the
// rest of the old instance. Expired entries are instead swept
// opportunistically from incrBy, at most once every sweepEvery.
type memoryStore struct {
	data      map[string]*memoryEntry
	nowFn     func() time.Time // injected for tests; defaults to time.Now
	lastSweep time.Time        // guarded by mu; zero value sweeps on the first incrBy
	mu        sync.Mutex
}

// newMemoryStore returns an empty memoryStore.
func newMemoryStore() *memoryStore {
	return &memoryStore{data: make(map[string]*memoryEntry), nowFn: time.Now}
}

// now returns m's current time, via nowFn.
func (m *memoryStore) now() time.Time {
	return m.nowFn()
}

// incrBy implements counterStore. ttl is clamped by clampTTL with no
// enforcement floor (enforceTTL=0) — incrBy has no window to derive one
// from, since it takes a bare key rather than a (kind, id, metric,
// window) tuple. incrBy has had no production caller since checkAndCount/
// account moved to the batched incrMulti below (perf review,
// 2026-08-21); it is exercised directly by 11 test call sites (across
// limits_test.go and redis_store_test.go's own store.incrBy calls) and
// kept for counterStore interface conformance. incrCounter (below) is
// the limiter-level single-key seeding helper tests use instead (5 call
// sites, all in admin_test.go's TestAdminUsage_MathAgainstSeededCounters).
func (m *memoryStore) incrBy(key string, n int64, ttl time.Duration) (int64, error) {
	return m.applyIncr(key, n, clampTTL(ttl, 0)), nil
}

// applyIncr increments key by n — creating it fresh, expiring at
// now+ttl, if absent or already expired — and returns its new value. ttl
// must already be clamped by the caller: incrBy clamps with no
// enforcement floor, incrMulti clamps per entry with that entry's own
// enforceTTL floor (see clampTTL). Sharing this one map-mutation-plus-
// opportunistic-sweep implementation between both callers means the
// sweep logic, and the "already expired counts as absent" rule, exist
// exactly once.
func (m *memoryStore) applyIncr(key string, n int64, ttl time.Duration) int64 {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()

	if now.Sub(m.lastSweep) >= sweepEvery {
		m.sweepLocked(now)
		m.lastSweep = now
	}

	e, ok := m.data[key]
	if !ok || !now.Before(e.expiry) {
		e = &memoryEntry{expiry: now.Add(ttl)}
		m.data[key] = e
	}
	e.value += n
	return e.value
}

// get implements counterStore.
func (m *memoryStore) get(key string) (int64, error) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.data[key]
	if !ok || !now.Before(e.expiry) {
		return 0, nil
	}
	return e.value, nil
}

// getMulti implements counterStore by looping over get: memoryStore's
// in-process map access is already effectively free per key, so there is
// no round-trip cost to batch away — this exists purely so memoryStore
// satisfies counterStore's getMulti contract for the limiter's nil-store
// fallback path (storeGetMulti).
func (m *memoryStore) getMulti(keys []string) ([]int64, error) {
	out := make([]int64, len(keys))
	for i, k := range keys {
		out[i], _ = m.get(k) // memoryStore.get never errors
	}
	return out, nil
}

// incrMulti implements counterStore by looping over applyIncr:
// memoryStore's in-process map access is already effectively free per
// key, so there is no round-trip cost to batch away — this exists purely
// so memoryStore satisfies counterStore's incrMulti contract for the
// limiter's nil-store fallback path (storeIncrMulti). Each entry's own
// ttl is clamped with ITS OWN enforceTTL floor (clampTTL), not incrBy's
// floor-less clamp — this is the window-aware path real enforcement
// (checkAndCount/account) relies on; see counterIncr.enforceTTL's own
// doc comment for the bug this fixes.
func (m *memoryStore) incrMulti(entries []counterIncr) ([]int64, error) {
	out := make([]int64, len(entries))
	for i, e := range entries {
		out[i] = m.applyIncr(e.key, e.delta, clampTTL(e.ttl, e.enforceTTL))
	}
	return out, nil
}

// incrAndGetMulti implements counterStore by composing incrMulti and
// getMulti: memoryStore's in-process map access is already effectively
// free per key, so there is no round-trip cost to fuse away here — this
// exists purely so memoryStore satisfies counterStore's incrAndGetMulti
// contract (perf review round 3, 2026-08-22) for the limiter's nil-store
// fallback path and its own failOpen fallback, mirroring incrMulti/
// getMulti's own "implements the contract, buys nothing locally" role.
func (m *memoryStore) incrAndGetMulti(entries []counterIncr, reads []string) ([]int64, []int64, error) {
	incrVals, _ := m.incrMulti(entries) // memoryStore.incrMulti never errors
	readVals, _ := m.getMulti(reads)    // memoryStore.getMulti never errors
	return incrVals, readVals, nil
}

// sweepLocked deletes every expired entry. Callers must hold m.mu.
func (m *memoryStore) sweepLocked(now time.Time) {
	for k, e := range m.data {
		if !now.Before(e.expiry) {
			delete(m.data, k)
		}
	}
}

// usage holds one request's token counts. estimated marks counts derived
// from a tokenizer estimate rather than the provider's reported usage
// (e.g. for a streaming response cut off before an upstream usage frame
// arrived).
type usage struct {
	prompt, completion int64
	// estimated is true when prompt/completion were derived from a
	// tokenizer estimate (request body size ÷ 4) rather than the
	// provider's own reported usage. routes_unified.go's runUnified reads
	// this to log the estimate explicitly, so a caller reading logs can
	// tell an estimated cost from a provider-reported one, per the spec's
	// "logged as estimated" requirement.
	estimated bool
}

// total returns the request's combined prompt and completion tokens.
func (u usage) total() int64 {
	return u.prompt + u.completion
}

// limitScope is one entity (a user, their group, or the synthetic total
// scope — see totalScopeKind) whose limits apply to a request.
// checkAndCount and account evaluate every scope in the slice they are
// given, so a request is counted and checked against both its user's and
// its group's limits in one call.
type limitScope struct {
	limits *LimitsConfig
	kind   string // "user", "group", or "total"
	id     string
}

// totalScopeKind and totalScopeID name the synthetic "all LLM traffic"
// scope withTotalScope (routes_unified.go) appends to every metered
// route's scopes slice (v0.2 data-layer task): {kind: totalScopeKind, id:
// totalScopeID}, limits always nil. checkAndCount's own nil-limits check
// (below) skips it during evaluation, but still counts it
// like any other scope — its req/tokin/tokout/cost counters accumulate
// every LLM request across all users and groups combined, the admin
// usage-history "total" series — but no configuration can ever throttle
// traffic in its name.
const (
	totalScopeKind = "total"
	totalScopeID   = "all"
)

// limitViolation describes the first limit a request breached.
type limitViolation struct {
	message string
	// retryAfter is the number of seconds until the violated window ends
	// — until the client can plausibly succeed again. Left at its zero
	// value for a storeDown violation, which carries no meaningful window.
	retryAfter int
	// storeDown reports whether the violation was manufactured because the
	// configured counterStore was unreachable and failOpen is false,
	// rather than an actual limit breach. Callers map this to an HTTP 503
	// instead of 429 (Task 12).
	storeDown bool
}

// limiter enforces per-minute/day/month request, token, and cost limits
// using fixed windows keyed by windowKey.
type limiter struct {
	lastLogAt        time.Time        // guarded by logMu; last time a store error was logged
	lastStoreFailure time.Time        // guarded by logMu; zero means the store-down latch is not open (see storeLatched)
	store            counterStore     // configured backend; nil means always use fallback (see newLimiter)
	fallback         *memoryStore     // in-process counter store, always available
	nowFn            func() time.Time // injected for tests; defaults to time.Now
	// logf is a bound method value (g.errorf), injectable for tests; defaults
	// to a no-op. Its own call site (logStoreError) passes exactly one
	// variadic argument — never extend that to two or more without first
	// reading modelRegistry.log's doc comment in registry.go: a struct
	// field of variadic func type crashes Yaegi v0.16.1's CFG builder past
	// one variadic argument, even though the identical call through a
	// method or interface method does not.
	logf func(format string, args ...any)
	// spawn runs f, by default in its own goroutine — the SHOULD-5 ruling
	// (v0.22 review round): recordProviderAttempt's store write is
	// telemetry-only, and must never sit on a streaming response's
	// time-to-first-byte path (a synchronous storeIncrMulti round trip
	// there would delay every first chunk by however long that write
	// takes). newLimiter always wires a production limiter's spawn to a
	// BOUNDED goroutine — see spawnTokens below (FOLDED-1, v0.22 review
	// round, round 2): unbounded `go f()` could pile up an unlimited
	// number of in-flight writes against a slow-but-alive Redis, since
	// nothing here backpressures request volume. A test that needs
	// recordProviderAttempt's write to have already landed by the time it
	// reads a counter back overrides spawn to run f synchronously
	// instead, bypassing spawnTokens entirely — the same dependency-
	// injection idiom nowFn already uses on this struct, and waitFn uses
	// on retryPolicy (retry.go). A goroutine spawned this way outlives
	// the request that triggered it by design: a write still in flight at
	// process shutdown is lost. Accepted — this is telemetry, nothing in
	// the request path gates on it, and the fixed-window counters it
	// writes to are inherently approximate already (a counter reset at a
	// bucket boundary loses whatever was in the previous bucket too).
	spawn func(func())
	// spawnTokens is the buffered-channel semaphore the production spawn
	// closure (newLimiter) acquires from non-blockingly before starting a
	// goroutine, and releases when that goroutine finishes — FOLDED-1
	// (v0.22 review round, round 2): capped at providerAttemptSpawnCap
	// in-flight writes per limiter. Acquiring a token that is not
	// immediately available means the write is DROPPED, never queued or
	// blocked — bounded AND lossy-under-pressure, by design: this is a
	// telemetry counter family (recordProviderAttempt), not enforcement
	// (checkAndCount/account never go through spawn at all), so losing an
	// occasional attempt/failure increment under sustained store pressure
	// is an accepted trade-off against the alternative, an unbounded
	// goroutine pile-up that could itself become the outage. Per-limiter,
	// not a package-level channel: this codebase's own test suite
	// constructs many independent *limiter values in one process
	// (including in parallel, via t.Parallel()), and a shared global
	// semaphore would let one test's write volume starve an unrelated
	// test's — see newLimiter's own construction of it.
	spawnTokens chan struct{}
	// lastErrMsg is the message of the most recent store operation
	// failure, guarded by logMu alongside lastStoreFailure. It is never
	// cleared on a later success — "last store error" for the admin
	// dashboard (spec §4, v0.2) means exactly that, a persisting fact,
	// not "is the store currently failing" (storeLatched already answers
	// that question for the enforcement path).
	lastErrMsg string
	// rejections tracks rate/budget-limit-violation counts per scope
	// (metrics.go's llmgateway_rate_limit_rejections_total). It is a
	// plain in-process, per-process-lifetime counter guarded by its own
	// mutex, deliberately NOT a counterStore key: checkAndCount already
	// pays a Redis round trip on every admission decision, and every
	// number metrics.go otherwise exposes is read back OUT of that
	// existing accounting rather than written fresh — but "how many
	// requests were rejected" has no existing counter to read at all
	// (req:min/req:day count every admission attempt, accepted or not,
	// by design — see checkAndCount's own doc comment). Adding it here
	// as a bare map increment costs one uncontended lock per rejection
	// (already the rare, not-hot-path outcome of checkAndCount), never a
	// second network round trip, and never re-derives a number the
	// store already tracks.
	rejections rejectionCounter
	logMu      sync.Mutex
	failOpen   bool // store-error policy: true falls back to fallback, false refuses the request
}

// rejectionScope is the (kind, id) key rejectionCounter accumulates
// under — a scope's own kind/id pair, exactly as limitScope carries them,
// but held separately so rejectionCounter's map key never aliases a
// limitScope's own `limits` pointer (which plays no part in identity
// here: two limitScope values for the same user with different `limits`
// pointers must still land in the identical bucket).
type rejectionScope struct {
	kind string
	id   string
}

// rejectionCounter accumulates rate/budget-limit-violation counts per
// scope for the lifetime of the process — limiter.rejections' own
// backing store; see its doc comment for why this exists as a bare
// in-process map rather than a counterStore key. Unlike authFailureTracker
// (auth.go), there is no TTL or generation rotation here: a Prometheus
// counter must only ever grow for as long as the process runs, so there
// is nothing to roll over, and cardinality is bounded by the configured
// user/group count (checkAndCount only ever sees scopes for an already-
// authenticated caller's own user/group, never attacker-controlled
// strings), the same bound scopeUsage's own per-scope metrics already
// carry.
type rejectionCounter struct {
	counts map[rejectionScope]int64
	mu     sync.Mutex
}

// increment records one rejection for (kind, id), initializing the
// backing map on first use — rejectionCounter's zero value (as embedded,
// unexported, in limiter) is ready to use without a constructor.
func (c *rejectionCounter) increment(kind, id string) {
	c.mu.Lock()
	if c.counts == nil {
		c.counts = make(map[rejectionScope]int64)
	}
	c.counts[rejectionScope{kind: kind, id: id}]++
	c.mu.Unlock()
}

// rejectionSnapshot is one (kind, id) scope's rate-limit-rejection count
// — rejectionCounter.snapshot's own output, metrics.go's read of it.
type rejectionSnapshot struct {
	kind  string
	id    string
	count int64
}

// snapshot returns every scope with at least one recorded rejection, in
// no particular order (metrics.go sorts its own copy before rendering).
func (c *rejectionCounter) snapshot() []rejectionSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]rejectionSnapshot, 0, len(c.counts))
	for scope, n := range c.counts {
		out = append(out, rejectionSnapshot{kind: scope.kind, id: scope.id, count: n})
	}
	return out
}

// rejectionScopeStoreDown is the synthetic scope id checkAndCount
// records a rejection under when it refuses a request because the
// configured store is unreachable and failOpen is false
// (storeDownViolation) — there is no real limitScope in play at that
// point (the failure happens before any scope is evaluated), so this
// names the event on its own rather than attributing it to whichever
// scope happened to be first in the slice.
const rejectionScopeStoreDown = "store_down"

// rejectionSnapshot (method) reads l.rejections — metrics.go's own entry
// point, named to match l's other read accessors (redisStatus,
// currentUsage, providerUsage) rather than exposing the rejections field
// itself.
func (l *limiter) rejectionSnapshot() []rejectionSnapshot {
	return l.rejections.snapshot()
}

// providerAttemptSpawnCap bounds how many concurrent recordProviderAttempt
// store writes one limiter's production spawn implementation will run at
// once (FOLDED-1, v0.22 review round, round 2) — see limiter.spawnTokens'
// own doc comment for the full rationale. 64 is generous headroom for
// this telemetry path specifically (every real deployment's traffic
// volume is expected to sit well under it in steady state) while still
// bounding the worst case against a store that has gone slow but not
// down.
const providerAttemptSpawnCap = 64

// newLimiter returns a limiter. A nil store means every operation uses the
// limiter's own in-process fallback memoryStore — the plugin still
// enforces limits with no distributed backend configured, just without
// sharing counters across Traefik instances. failOpen governs what happens
// when a non-nil store errors: true silently falls back to the in-process
// memoryStore for that operation, false refuses the request (see
// storeIncrBy / storeGet).
func newLimiter(store counterStore, failOpen bool) *limiter {
	l := &limiter{
		store:       store,
		fallback:    newMemoryStore(),
		nowFn:       time.Now,
		logf:        func(string, ...any) {},
		failOpen:    failOpen,
		spawnTokens: make(chan struct{}, providerAttemptSpawnCap),
	}
	l.spawn = func(f func()) {
		select {
		case l.spawnTokens <- struct{}{}:
			go func() {
				// The deferred recover is required for the same reason
				// modelRegistry.refreshProvider documents (registry.go):
				// this runs off any request's goroutine, so an unrecovered
				// panic has no ServeHTTP caller to unwind into and would
				// crash the whole Traefik process — every router, not just
				// this middleware. A panicking telemetry write must cost
				// exactly one dropped write.
				defer func() {
					if rec := recover(); rec != nil {
						l.logf("limit telemetry write panicked: %v", rec)
					}
					<-l.spawnTokens
				}()
				f()
			}()
		default:
			// Dropped: spawnTokens is full — bounded, lossy-under-pressure
			// by design (FOLDED-1, its own doc comment above). The drop is
			// logged (rate-limited) so a stressed store under-reporting
			// provider health is distinguishable from a healthy provider —
			// silence here would make the two identical.
			l.logSpawnDrop()
		}
	}
	return l
}

// now returns the limiter's current time, via nowFn.
func (l *limiter) now() time.Time {
	return l.nowFn()
}

// logSpawnDrop logs a dropped telemetry write, rate-limited on the same
// clock and interval as logStoreError (sharing lastLogAt deliberately:
// drops and store errors are the same "the store is struggling" story,
// and one line per storeErrorLogEvery is enough to tell it).
func (l *limiter) logSpawnDrop() {
	now := l.now()
	l.logMu.Lock()
	shouldLog := now.Sub(l.lastLogAt) >= storeErrorLogEvery
	if shouldLog {
		l.lastLogAt = now
	}
	l.logMu.Unlock()
	if shouldLog {
		l.logf("limit telemetry write dropped: %d concurrent writes in flight (bounded, lossy under pressure by design)", providerAttemptSpawnCap)
	}
}

// logStoreError logs err via l.logf, rate-limited to once per
// storeErrorLogEvery regardless of how many store operations fail in
// between.
func (l *limiter) logStoreError(err error) {
	now := l.now()
	l.logMu.Lock()
	shouldLog := now.Sub(l.lastLogAt) >= storeErrorLogEvery
	if shouldLog {
		l.lastLogAt = now
	}
	l.logMu.Unlock()
	if shouldLog {
		l.logf("limit store error: %v", err)
	}
}

// storeLatched reports whether l is within storeDownLatchFor of its last
// recorded store failure. When true, storeIncrBy/storeGet must skip the
// network call entirely and go straight to the fail-open/fail-closed
// policy — the same outcome a fresh call would reach anyway, just without
// paying respCallTimeout again to find out. Guarded by logMu, the same
// mutex lastLogAt already uses for this kind of small timestamp state.
func (l *limiter) storeLatched() bool {
	l.logMu.Lock()
	defer l.logMu.Unlock()
	return !l.lastStoreFailure.IsZero() && l.now().Sub(l.lastStoreFailure) < storeDownLatchFor
}

// recordStoreFailure logs err (rate-limited, see logStoreError) and opens
// the store-down latch: every storeIncrBy/storeGet call for the next
// storeDownLatchFor skips the network call and applies the fail-open/
// fail-closed policy directly.
func (l *limiter) recordStoreFailure(err error) {
	l.logStoreError(err)
	l.logMu.Lock()
	l.lastStoreFailure = l.now()
	l.lastErrMsg = err.Error()
	l.logMu.Unlock()
}

// redisStatus reports whether l has a configured (non-fallback-only)
// store, that store's most recent operation failure message, and when
// that failure happened — the admin dashboard's redis status line (spec
// §4, v0.2). lastErr is "" and lastErrAt is the zero time when no store
// operation has ever failed. Returning the timestamp alongside the
// message (controller-approved amendment, 2026-08-20 review) lets the
// dashboard render "last error (12s ago): ..." instead of a bare message
// that reads as "currently broken" even long after a single transient
// blip. Guarded by logMu, the same mutex lastStoreFailure/lastErrMsg
// already use.
func (l *limiter) redisStatus() (configured bool, lastErr string, lastErrAt time.Time) {
	l.logMu.Lock()
	defer l.logMu.Unlock()
	return l.store != nil, l.lastErrMsg, l.lastStoreFailure
}

// storeIncrBy increments key by n with the given ttl, applying the
// limiter's fail-open/fail-closed policy when a configured store errors
// or when the store-down latch (storeLatched) is already open from a
// recent failure. ok is false only in the fail-closed case — a caller
// must refuse the request for that, rather than treating a zero value as
// a real counter reading. storeIncrBy has no production caller since
// checkAndCount/account moved to the batched storeIncrMulti (perf review,
// 2026-08-21) — its only caller, incrCounter below, is itself only
// called from tests, for direct counter seeding.
func (l *limiter) storeIncrBy(key string, n int64, ttl time.Duration) (v int64, ok bool) {
	if l.store == nil {
		v, _ = l.fallback.incrBy(key, n, ttl) // fallback never errors
		return v, true
	}
	if l.storeLatched() {
		return l.failPolicyIncrBy(key, n, ttl)
	}
	v, err := l.store.incrBy(key, n, ttl)
	if err == nil {
		return v, true
	}
	l.recordStoreFailure(err)
	return l.failPolicyIncrBy(key, n, ttl)
}

// storeGet mirrors storeIncrBy for a read: it applies the same fail-open/
// fail-closed/latched handling on a configured store's error.
func (l *limiter) storeGet(key string) (v int64, ok bool) {
	if l.store == nil {
		v, _ = l.fallback.get(key)
		return v, true
	}
	if l.storeLatched() {
		return l.failPolicyGet(key)
	}
	v, err := l.store.get(key)
	if err == nil {
		return v, true
	}
	l.recordStoreFailure(err)
	return l.failPolicyGet(key)
}

// failPolicyIncrBy applies the limiter's fail-open/fail-closed policy for
// an incrBy the caller has decided not to attempt against the configured
// store (it just failed, or the store-down latch is open): failOpen=true
// counts key in the fallback instead; failOpen=false reports the
// operation as failed.
func (l *limiter) failPolicyIncrBy(key string, n int64, ttl time.Duration) (int64, bool) {
	if !l.failOpen {
		return 0, false
	}
	v, _ := l.fallback.incrBy(key, n, ttl)
	return v, true
}

// failPolicyGet mirrors failPolicyIncrBy for a read.
func (l *limiter) failPolicyGet(key string) (int64, bool) {
	if !l.failOpen {
		return 0, false
	}
	v, _ := l.fallback.get(key)
	return v, true
}

// storeGetMulti mirrors storeGet for a batch of keys: one round trip
// against the configured store (or the in-process fallback) for the
// whole slice, applying the identical fail-open/fail-closed/latched
// policy storeGet applies per key. ok is false only in the fail-closed
// case, matching storeGet's contract — a caller must not read the
// returned slice as real values when ok is false.
func (l *limiter) storeGetMulti(keys []string) (v []int64, ok bool) {
	if l.store == nil {
		v, _ = l.fallback.getMulti(keys) // fallback never errors
		return v, true
	}
	if l.storeLatched() {
		return l.failPolicyGetMulti(keys)
	}
	v, err := l.store.getMulti(keys)
	if err == nil {
		return v, true
	}
	l.recordStoreFailure(err)
	return l.failPolicyGetMulti(keys)
}

// failPolicyGetMulti mirrors failPolicyGet for a batch read.
func (l *limiter) failPolicyGetMulti(keys []string) ([]int64, bool) {
	if !l.failOpen {
		return nil, false
	}
	v, _ := l.fallback.getMulti(keys)
	return v, true
}

// storeIncrMulti mirrors storeIncrBy for a batch of increments: one round
// trip against the configured store (or the in-process fallback) for the
// whole slice, applying the identical fail-open/fail-closed/latched
// policy storeIncrBy applies per key. ok is false only in the fail-closed
// case — a caller must drop the sample (account) or skip the write
// (countTargetRequests, recordProviderAttempt) for that, rather than
// treating a nil/short slice as real counter readings. checkAndCount's own
// increments moved to storeIncrAndGetMulti (perf review round 3,
// 2026-08-22), so it is no longer a caller of this method.
func (l *limiter) storeIncrMulti(entries []counterIncr) (v []int64, ok bool) {
	if l.store == nil {
		v, _ = l.fallback.incrMulti(entries) // fallback never errors
		return v, true
	}
	if l.storeLatched() {
		return l.failPolicyIncrMulti(entries)
	}
	v, err := l.store.incrMulti(entries)
	if err == nil {
		return v, true
	}
	l.recordStoreFailure(err)
	return l.failPolicyIncrMulti(entries)
}

// failPolicyIncrMulti mirrors failPolicyIncrBy for a batch increment.
func (l *limiter) failPolicyIncrMulti(entries []counterIncr) ([]int64, bool) {
	if !l.failOpen {
		return nil, false
	}
	v, _ := l.fallback.incrMulti(entries)
	return v, true
}

// storeIncrAndGetMulti mirrors storeIncrMulti for checkAndCount's fused
// admission round trip (perf review round 3, 2026-08-22): one call against
// the configured store (or the in-process fallback) for BOTH entries' own
// increments and reads' own current values, applying the identical
// fail-open/fail-closed/latched policy storeIncrMulti/storeGetMulti already
// apply to their own separate batches. ok is false only in the fail-closed
// case — checkAndCount must refuse the request for that, rather than
// treating a nil/short slice as real counter readings.
func (l *limiter) storeIncrAndGetMulti(entries []counterIncr, reads []string) (incrVals, readVals []int64, ok bool) {
	if l.store == nil {
		incrVals, readVals, _ = l.fallback.incrAndGetMulti(entries, reads) // fallback never errors
		return incrVals, readVals, true
	}
	if l.storeLatched() {
		return l.failPolicyIncrAndGetMulti(entries, reads)
	}
	incrVals, readVals, err := l.store.incrAndGetMulti(entries, reads)
	if err == nil {
		return incrVals, readVals, true
	}
	l.recordStoreFailure(err)
	return l.failPolicyIncrAndGetMulti(entries, reads)
}

// failPolicyIncrAndGetMulti mirrors failPolicyIncrMulti for the fused
// admission round trip.
func (l *limiter) failPolicyIncrAndGetMulti(entries []counterIncr, reads []string) ([]int64, []int64, bool) {
	if !l.failOpen {
		return nil, nil, false
	}
	incrVals, readVals, _ := l.fallback.incrAndGetMulti(entries, reads)
	return incrVals, readVals, true
}

// storeDownViolation is the violation checkAndCount returns when the
// configured store is unreachable and failOpen is false: the
// request is refused instead of silently enforcing limits against a
// non-shared fallback, so a backend outage cannot let every configured
// limit go unenforced across a fleet of gateway instances.
func storeDownViolation() *limitViolation {
	return &limitViolation{message: "limit store unavailable", storeDown: true}
}

// bucketFor formats t (in UTC) to window's bucket granularity: the string
// windowKey embeds as a key's final component, and the same string GET
// /admin/api/usage/history echoes as each point's "bucket" field
// (historyBucketKeys, below).
func bucketFor(t time.Time, window string) string {
	u := t.UTC()
	switch window {
	case windowHour:
		return u.Format("2006010215")
	case windowMin:
		return u.Format("200601021504")
	case windowDay:
		return u.Format("20060102")
	case windowMonth:
		return u.Format("200601")
	default:
		// window is always one of the four constants above, set by this
		// file's own callers, never by request input — an unknown value
		// here is a programming error. ServeHTTP's recoverPanic turns
		// this into a logged 500 instead of crashing the process.
		panic(fmt.Sprintf("llmgateway: bucketFor: unknown window %q", window))
	}
}

// windowKey builds the counterStore key for one (kind, id, metric, window)
// counter at time t: llmgw:{kind}:{id}:{metric}:{window}:{bucket}. bucket
// is t.UTC() formatted to the window's granularity (bucketFor), so a
// counter's key changes automatically when its window rolls over.
func windowKey(kind, id, metric, window string, t time.Time) string {
	return fmt.Sprintf("llmgw:%s:%s:%s:%s:%s", kind, id, metric, window, bucketFor(t, window))
}

// windowEnd returns the UTC instant at which window's bucket containing t
// ends: the next hour or minute boundary, the next UTC midnight, or the
// first of next month UTC.
func windowEnd(t time.Time, window string) time.Time {
	u := t.UTC()
	switch window {
	case windowHour:
		return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
	case windowMin:
		return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), u.Minute(), 0, 0, time.UTC).Add(time.Minute)
	case windowDay:
		return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	case windowMonth:
		return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
	default:
		panic(fmt.Sprintf("llmgateway: windowEnd: unknown window %q", window))
	}
}

// retryAfterSeconds returns the whole number of seconds from t until
// window's bucket ends, rounded up so a client never retries a moment too
// early.
func retryAfterSeconds(t time.Time, window string) int {
	return int(math.Ceil(windowEnd(t, window).Sub(t.UTC()).Seconds()))
}

// incrCounter increments the (kind, id, metric, window) counter at t by n
// and returns its new value, together with whether the operation
// succeeded under the limiter's fail-open/fail-closed policy (see
// storeIncrBy). ok is false only in the fail-closed case: a configured
// store errored and failOpen is false. incrCounter has no production
// caller since checkAndCount/account moved to the batched incrMulti
// (perf review, 2026-08-21) — it is kept for direct counter seeding in
// tests (5 call sites, all in admin_test.go's
// TestAdminUsage_MathAgainstSeededCounters).
func (l *limiter) incrCounter(kind, id, metric, window string, t time.Time, n int64, ttl time.Duration) (int64, bool) {
	return l.storeIncrBy(windowKey(kind, id, metric, window, t), n, ttl)
}

// getCounter returns the (kind, id, metric, window) counter's value at t,
// together with whether the read succeeded under the limiter's fail-open/
// fail-closed policy (see storeGet).
func (l *limiter) getCounter(kind, id, metric, window string, t time.Time) (int64, bool) {
	return l.storeGet(windowKey(kind, id, metric, window, t))
}

// checkAndCountKeysPerScope is the number of counterIncr entries
// checkAndCount builds per scope (req:min, req:day, req:hour), and the
// stride storeIncrAndGetMulti's flat incrVals result is sliced back into
// per-scope counts by.
const checkAndCountKeysPerScope = 3

// budgetProbe names one token/cost budget checkAndCount's fused admission
// round trip must evaluate — the same values the pre-fusion implementation
// checked via its own evaluateScope/budgetViolation/tokenBudgetViolation
// helpers (removed by perf review round 3, 2026-08-22), now inlined into
// checkAndCount itself so their GET reads can ride the same round trip as
// checkAndCount's own request-counter increments (see incrAndGetMulti,
// counterStore's own doc comment above, for why that fusion is safe).
type budgetProbe struct {
	name     string // e.g. "tokens-per-day" — echoed into the violation message verbatim
	window   string // windowDay or windowMonth; retryAfterSeconds is computed against this
	scopeIdx int    // index into the scopes slice checkAndCount was given
	limit    int64  // already micro-USD for a cost probe (usdToMicros); always > 0
	keyStart int    // index into buildBudgetProbes' own reads slice
	tokens   bool   // true: keyStart/keyStart+1 are tokin/tokout, summed; false: keyStart alone is cost
}

// buildBudgetProbes returns, for every scope in scopes that has a
// TokensPerDay/TokensPerMonth/CostPerDayUSD/CostPerMonthUSD limit set
// (limit<=0 means unlimited and needs no read at all — the same
// "limit<=0 returns immediately, before touching the store" short-circuit
// the pre-fusion budgetViolation/tokenBudgetViolation applied), the probe
// describing that budget and the flat list of counterStore keys
// checkAndCount's fused round trip must read to evaluate it. Probes are
// appended in the SAME order the pre-fusion evaluateScope checked them,
// scope by scope: tokens-per-day, tokens-per-month, cost-per-day,
// cost-per-month within each scope, scopes themselves in their own given
// order — so a caller that walks probes in order after the round trip
// completes reproduces the identical violation precedence.
func buildBudgetProbes(scopes []limitScope, now time.Time) (probes []budgetProbe, reads []string) {
	for i, sc := range scopes {
		if sc.limits == nil {
			continue
		}
		lim := sc.limits
		add := func(name, window string, limit int64, tokens bool) {
			if limit <= 0 {
				return
			}
			probes = append(probes, budgetProbe{scopeIdx: i, name: name, window: window, limit: limit, tokens: tokens, keyStart: len(reads)})
			if tokens {
				reads = append(reads,
					windowKey(sc.kind, sc.id, metricTokIn, window, now),
					windowKey(sc.kind, sc.id, metricTokOut, window, now))
			} else {
				reads = append(reads, windowKey(sc.kind, sc.id, metricCost, window, now))
			}
		}
		add("tokens-per-day", windowDay, lim.TokensPerDay, true)
		add("tokens-per-month", windowMonth, lim.TokensPerMonth, true)
		add("cost-per-day", windowDay, usdToMicros(lim.CostPerDayUSD), false)
		add("cost-per-month", windowMonth, usdToMicros(lim.CostPerMonthUSD), false)
	}
	return probes, reads
}

// checkAndCount increments every scope's req:min, req:day, and req:hour
// counters — unconditionally, before any evaluation, so a request that
// ultimately gets refused by one scope's limit still counts toward every
// other scope's rate tracking — then evaluates each scope's set limits, in
// order, and returns the first violation found, or nil if the request may
// proceed. If the round trip fails closed (a configured store errored and
// failOpen is false), it returns a storeDownViolation immediately — the
// request is refused rather than evaluated against partial or
// fallback-only counters.
//
// Every scope's three increments AND every scope's token/cost budget reads
// ride ONE storeIncrAndGetMulti round trip (perf review round 3,
// 2026-08-22, fusing on top of round 1's own "one round trip for the
// increments" fix): a 2-scope request (user, group — buildLimitScopes,
// routes_unified.go) with both scopes fully limited previously paid that
// one round trip for the increments, PLUS up to 4 more SEPARATE, SERIAL
// round trips per scope for its token/cost budgets (evaluateScope calling
// tokenBudgetViolation twice and budgetViolation twice) — 9 total before
// this fix. incrAndGetMulti's own reply already carries each entry's
// post-increment value AND each budget key's current value, in order, so
// evaluation below needs no further store call at all. This is safe
// because every budget key (tokin/tokout/cost:*) is disjoint from every
// increment's key (req:*), and tokens/cost are only ever written by
// account() AFTER the upstream response completes — never by this call —
// so reading them alongside the increments carries no read-after-write
// hazard; see incrAndGetMulti's own doc comment (counterStore, above) for
// the full argument.
//
// Violation precedence is unchanged from the pre-fusion implementation:
// scopes are evaluated in their own given order (buildLimitScopes puts a
// user before their group; withTotalScope appends the synthetic total
// scope last), and within one scope: requests-per-minute, requests-per-day,
// tokens-per-day, tokens-per-month, cost-per-day, cost-per-month — the
// first violation found anywhere in that walk is returned immediately, and
// evaluation of any later scope never runs. evaluateScope/budgetViolation/
// tokenBudgetViolation (the pre-fusion, separate-round-trip
// implementation) are removed by this change — buildBudgetProbes plus the
// loop below inline their exact logic instead, which is what lets their
// store reads ride this one round trip.
//
// Requests limits compare against the value just incremented in this call.
// Token and cost limits compare against the value already accumulated by
// account (via this call's own reads): a request that would start over
// budget is refused, but because usage is only known after the upstream
// call completes, one request may still push a scope over its budget — an
// accepted trade-off, unchanged from before this round.
func (l *limiter) checkAndCount(scopes []limitScope) *limitViolation {
	now := l.now()

	entries := make([]counterIncr, 0, len(scopes)*checkAndCountKeysPerScope)
	for _, sc := range scopes {
		entries = append(entries,
			newCounterIncr(sc.kind, sc.id, metricReq, windowMin, now, 1, minWindowTTL),
			newCounterIncr(sc.kind, sc.id, metricReq, windowDay, now, 1, dayWindowTTL),
			newCounterIncr(sc.kind, sc.id, metricReq, windowHour, now, 1, hourWindowTTL),
		)
	}
	probes, reads := buildBudgetProbes(scopes, now)

	incrVals, readVals, ok := l.storeIncrAndGetMulti(entries, reads)
	if !ok || len(incrVals) != len(entries) || len(readVals) != len(reads) {
		l.rejections.increment(rejectionScopeStoreDown, rejectionScopeStoreDown)
		return storeDownViolation()
	}

	for i, sc := range scopes {
		if sc.limits == nil {
			continue
		}
		minCount := incrVals[i*checkAndCountKeysPerScope]
		dayCount := incrVals[i*checkAndCountKeysPerScope+1]
		// incrVals[i*checkAndCountKeysPerScope+2] is the req:hour count —
		// stats-only, deliberately never read here.
		if v := requestLimitViolation(sc, "requests-per-minute", sc.limits.RequestsPerMinute, minCount, windowMin, now); v != nil {
			l.rejections.increment(sc.kind, sc.id)
			return v
		}
		if v := requestLimitViolation(sc, "requests-per-day", sc.limits.RequestsPerDay, dayCount, windowDay, now); v != nil {
			l.rejections.increment(sc.kind, sc.id)
			return v
		}
		for _, p := range probes {
			if p.scopeIdx != i {
				continue
			}
			used := readVals[p.keyStart]
			if p.tokens {
				used += readVals[p.keyStart+1]
			}
			if used < p.limit {
				continue
			}
			l.rejections.increment(sc.kind, sc.id)
			return &limitViolation{
				message:    fmt.Sprintf("%s %q exceeded %s budget", sc.kind, sc.id, p.name),
				retryAfter: retryAfterSeconds(now, p.window),
			}
		}
	}
	return nil
}

// requestLimitViolation reports a violation when count (already
// incremented for this request) exceeds limit. limit<=0 means unlimited.
func requestLimitViolation(sc limitScope, name string, limit, count int64, window string, now time.Time) *limitViolation {
	if limit <= 0 || count <= limit {
		return nil
	}
	return &limitViolation{
		message:    fmt.Sprintf("%s %q exceeded %s limit (%d)", sc.kind, sc.id, name, limit),
		retryAfter: retryAfterSeconds(now, window),
	}
}

// account records u's prompt/completion tokens — split into metricTokIn/
// metricTokOut (v0.2 data-layer task) — and costMicros against every
// scope's hour, day, and month counters. Each metric/direction is skipped
// independently when its own value is 0 — no store write for a direction
// this call has nothing to report, so a streaming response cut off before
// any completion tokens arrived still writes tokin alone.
//
// Every scope's writes are collected into ONE storeIncrMulti call (perf
// review, 2026-08-21): a 3-scope request (user, group, total) with all
// three metrics nonzero previously paid up to 9 round trips per scope —
// 27 total — one incrCounter call each. account's return-value discipline
// is unchanged by the batching: it has no error return, and a sample that
// hits a fail-closed batch (see storeIncrMulti) is dropped in full,
// silently, rather than counting it in the fallback — dropping avoids
// double-counting once the store recovers, the same reasoning that
// applied per-key before this call became one batch.
func (l *limiter) account(scopes []limitScope, u usage, costMicros int64) {
	now := l.now()

	entries := make([]counterIncr, 0, len(scopes)*9)
	for _, sc := range scopes {
		if u.prompt != 0 {
			entries = append(entries,
				newCounterIncr(sc.kind, sc.id, metricTokIn, windowHour, now, u.prompt, hourWindowTTL),
				newCounterIncr(sc.kind, sc.id, metricTokIn, windowDay, now, u.prompt, dayWindowTTL),
				newCounterIncr(sc.kind, sc.id, metricTokIn, windowMonth, now, u.prompt, monthWindowTTL),
			)
		}
		if u.completion != 0 {
			entries = append(entries,
				newCounterIncr(sc.kind, sc.id, metricTokOut, windowHour, now, u.completion, hourWindowTTL),
				newCounterIncr(sc.kind, sc.id, metricTokOut, windowDay, now, u.completion, dayWindowTTL),
				newCounterIncr(sc.kind, sc.id, metricTokOut, windowMonth, now, u.completion, monthWindowTTL),
			)
		}
		if costMicros != 0 {
			entries = append(entries,
				newCounterIncr(sc.kind, sc.id, metricCost, windowHour, now, costMicros, hourWindowTTL),
				newCounterIncr(sc.kind, sc.id, metricCost, windowDay, now, costMicros, dayWindowTTL),
				newCounterIncr(sc.kind, sc.id, metricCost, windowMonth, now, costMicros, monthWindowTTL),
			)
		}
	}
	if len(entries) == 0 {
		return // every metric was zero for every scope — nothing to write
	}
	l.storeIncrMulti(entries) // no error return by contract (doc comment above); ok is intentionally discarded
}

// countTargetRequests increments MULTIPLE target scopes' per-target request
// counters (Feature B, v0.21) — each at min, hour, day, AND month, the only
// scope kind this package tracks a month window of REQUESTS for — in ONE
// storeIncrMulti call. kind is targetScopeKind's own output ("mcp" or
// scopeKindAgent — mcp_a2a.go), id is the configured target name; both come
// embedded in windowKey's own key string, so a target scope can never
// collide with a user/group/total scope of the same id even by coincidence
// — kind is part of the key, not just a struct field. A scope's own
// `limits` field is ignored entirely (never read); callers pass plain
// {kind, id} pairs.
//
// This is deliberately NOT folded into checkAndCount: a target scope
// carries no limits by design (this round adds no config surface for
// MCP/agent limits), so there is nothing for checkAndCount to evaluate
// and no violation this call could ever produce — it is pure accounting, exactly
// like account() itself, hence the identical "no error return, ok
// discarded" contract. Month exists here, uniquely, because the admin
// dashboard's targets endpoint (admin.go's adminTargetCountersView)
// surfaces a requestsPerMonth figure no user/group admin view does;
// growing checkAndCount's own req:min/day/hour batch to a month window for
// EVERY scope kind, just to serve this one endpoint's response shape, was
// rejected as scope creep touching user/group accounting nothing else in
// this round asked to change.
//
// Batching (review round 2, 2026-08-21): the federated /mcp endpoint's own
// tools/list fan-out can attempt several backend servers for ONE incoming
// client request — the earlier per-server countTargetRequest call inside
// each fan-out goroutine paid one round trip per server; this collects
// every scope from the whole fan-out into a single call after the fan-out's
// own wg.Wait(), the same "one request in, one round trip out" discipline
// checkAndCount/account already apply per scope-slice.
//
// Callers: handleTargetProxy (mcp_a2a.go, a 1-element slice, once per
// proxied request, after its own user/group/total admission check passes)
// and the federated /mcp endpoint (mcp_federation.go: a 1-element slice for
// tools/call's single resolved target, or one element per server ATTEMPTED
// — not necessarily reached or succeeded — for tools/list's fan-out; see
// mcpFederatedToolsList's own doc comment for why "attempted" is the
// correct word here).
func (l *limiter) countTargetRequests(scopes []limitScope) {
	if len(scopes) == 0 {
		return
	}
	now := l.now()
	entries := make([]counterIncr, 0, len(scopes)*4)
	for _, sc := range scopes {
		entries = append(entries,
			newCounterIncr(sc.kind, sc.id, metricReq, windowMin, now, 1, minWindowTTL),
			newCounterIncr(sc.kind, sc.id, metricReq, windowHour, now, 1, hourWindowTTL),
			newCounterIncr(sc.kind, sc.id, metricReq, windowDay, now, 1, dayWindowTTL),
			newCounterIncr(sc.kind, sc.id, metricReq, windowMonth, now, 1, monthWindowTTL),
		)
	}
	l.storeIncrMulti(entries)
}

// countTargetRequest is countTargetRequests for exactly one target scope —
// handleTargetProxy's own single-target shape (mcp_a2a.go), where a batch
// of one buys nothing but keeps the call site a plain two-argument call
// instead of a one-element slice literal.
func (l *limiter) countTargetRequest(kind, id string) {
	l.countTargetRequests([]limitScope{{kind: kind, id: id}})
}

// targetCounterKeysPerScope is the number of windowKey strings
// targetUsageKeys builds per target scope, and the stride targetUsage's
// flat storeGetMulti result is sliced back into per-target chunks by.
const targetCounterKeysPerScope = 3

// targetUsageKeys returns the targetCounterKeysPerScope windowKey strings
// targetUsage reads for one (kind, id) target scope at time now — req at
// min, day, and month granularity, matching adminTargetCountersView's own
// field order (admin.go) exactly, so targetUsage can map storeGetMulti's
// result slice back by plain index. Hour is deliberately not read here:
// it is stats-only everywhere else in this package (windowHour's own doc
// comment) and no admin view — targets included — ever surfaces it.
func targetUsageKeys(kind, id string, now time.Time) []string {
	return []string{
		windowKey(kind, id, metricReq, windowMin, now),
		windowKey(kind, id, metricReq, windowDay, now),
		windowKey(kind, id, metricReq, windowMonth, now),
	}
}

// targetCounters is one MCP-server's or agent's current-window request
// counters (Feature B, v0.21) — requests only, mirroring handleTargetProxy's
// own passthrough-only accounting (mcp_a2a.go): a target scope never
// accumulates tokens or cost, so there is nothing else to report.
type targetCounters struct {
	requestsPerMinute int64
	requestsPerDay    int64
	requestsPerMonth  int64
}

// targetUsage reads every scope's current req:min/day/month counters in
// ONE storeGetMulti round trip, mirroring currentUsage's own single-batch
// discipline for the same reason: GET /admin/api/targets (admin.go) must
// not pay one round trip per configured MCP server and agent. Order is
// preserved: targetUsage(scopes)[i] corresponds to scopes[i]. Unlike
// scopeUsage, there is no per-entry storeDown flag — a fail-closed read
// (storeGetMulti's own contract) reports every target's counters as zero
// rather than partial/stale values, which is acceptable here specifically
// because a target scope carries no limit: nothing downstream ever treats
// this zero as "confirmed no traffic" the way scopeUsage.storeDown guards
// against for a limited scope.
func (l *limiter) targetUsage(scopes []limitScope) []targetCounters {
	out := make([]targetCounters, len(scopes))
	if len(scopes) == 0 {
		return out
	}

	now := l.now()
	allKeys := make([]string, 0, len(scopes)*targetCounterKeysPerScope)
	for _, sc := range scopes {
		allKeys = append(allKeys, targetUsageKeys(sc.kind, sc.id, now)...)
	}

	vals, ok := l.storeGetMulti(allKeys)
	if !ok || len(vals) != len(allKeys) {
		return out // zero-value counters; see doc comment above
	}

	for i := range scopes {
		v := vals[i*targetCounterKeysPerScope : i*targetCounterKeysPerScope+targetCounterKeysPerScope]
		out[i] = targetCounters{requestsPerMinute: v[0], requestsPerDay: v[1], requestsPerMonth: v[2]}
	}
	return out
}

// kindProvider and kindProviderModel are the limitScope.kind (and
// admin-API scope-kind) strings a provider's and a (provider, upstream
// model) pair's outcome counters use (Feature A, v0.22 — provider/model
// success-rate accounting) — collision-safety via kind embedding, mirroring
// scopeKindAgent's own rationale (mcp_a2a.go): windowKey embeds kind as the
// counter key's own leading segment, so a provider named identically to a
// user, group, or MCP/A2A target can never share a counter with it, and a
// provider-level scope can never collide with one of its own
// (provider, model) scopes either, even when a model happens to be named
// after its provider.
const (
	kindProvider      = "prov"
	kindProviderModel = "provmodel"
)

// metricProvAttempt and metricProvFail are the counter metric names
// recordProviderAttempt writes (Feature A, v0.22): every upstream attempt
// increments metricProvAttempt; only a provider-fault outcome
// (isTransient's own classification, retry.go — reused, not forked, per
// spec ruling) additionally increments metricProvFail.
const (
	metricProvAttempt = "attempt"
	metricProvFail    = "fail"
)

// providerModelScopeID joins provider and model into kindProviderModel's
// id convention: "provider/model" — the same canonical id shape
// pricing.go's unifiedCostMicros and the admin API already use elsewhere
// for a (provider, model) pair, so a reader who already knows that
// convention needs no new one here.
//
// The "/" join is unambiguous even though model itself may legitimately
// contain "/" (a discovered id like "uni/deepseek-v4-flash-0731" —
// routableModelId's own doc comment, webui/src/lib/format.ts, documents
// the identical case for the webui's copy of this convention): provider
// is always validated against configNamePattern (providers.go) before it
// can reach here, and that pattern's character class excludes "/"
// entirely. Two distinct (provider, model) pairs can therefore never
// produce the identical joined string — a provider name can never itself
// contain the separator a model id might.
func providerModelScopeID(provider, model string) string {
	return provider + "/" + model
}

// matchesSentinel reports whether err IS, or wraps (through any
// combination of single-%w and multi-%w Unwrap chains), sentinel —
// implemented with a real errors.Is call, NOT a hand-rolled Unwrap walk.
//
// This function used to hand-roll that walk instead, via two locally
// declared interfaces (single-error Unwrap() error, multi-error
// Unwrap() []error) matched with bare comma-ok type assertions — the
// documented-safe pattern providerHTTPError's own doc comment
// (providers.go) already established for a DIFFERENT direction: checking
// whether an INTERPRETED plugin-defined type (like providerHTTPError
// itself) satisfies the standard error interface, which is exactly what
// makes errors.As panic under Yaegi (it needs to synthesize interpreted
// type info for that check). That hand-rolled version was WRONG for
// THIS direction, and the SHOULD-A yaegi-check harness (v0.22 review
// round, round 2 — tools/yaegi-check/main.go's exerciseAttemptAccounting)
// caught it empirically: under the interpreter, `err.(multiUnwrapper)`
// — an INTERPRETED interface type (multiUnwrapper, declared in this
// file) asserted against a value whose concrete type is COMPILED
// (*fmt.wrapErrors, from a real fmt.Errorf call with more than one %w) —
// silently returned ok=false even though %T correctly reported
// *fmt.wrapErrors and that type genuinely implements Unwrap() []error in
// real, compiled Go. The instrumented follow-up (v0.22 final review)
// proved the rule is BROADER than the multi-%w shape: a comma-ok
// assertion of ANY compiled concrete value against ANY
// interpreter-declared interface type returns ok=false under Yaegi —
// a bare *url.Error asserted against a single-method
// interface{ Unwrap() error } declared in this file failed identically.
// Method count is irrelevant, the failure is silent (never a panic), and
// only the yaegi-check harness can catch it; compiled tests pass the
// broken code. This is the reverse direction of the already-documented
// errors.As trap, not previously known to this codebase.
//
// errors.Is does not have this problem here: sentinel (context.
// DeadlineExceeded, context.Canceled — isDeadlineExceeded's only
// callers) and every err this function is ever called with (the network/
// context error chain client.Do and fmt.Errorf produce) are BOTH always
// compiled-origin values — never an interpreted plugin type. errors.Is's
// own internal Unwrap-chain walk (including its Go 1.20+ multi-error
// support) therefore runs entirely within compiled code on compiled
// types, the same safe shape retry.go's isTransient already relies on
// for these identical two sentinels, proven correct under Yaegi across
// every prior review round. The risk this file's earlier version was
// trying to avoid — errors.As panicking on an INTERPRETED type — simply
// does not arise for a plain sentinel comparison against a compiled
// value. Verified directly: this replacement is what made SHOULD-A's
// timeout-driving harness pass under the real interpreter (round 2's
// hand-rolled fix, though it passed every compiled `go test`, did not).
//
// TRIP-WIRE, load-bearing and unenforced: the safety above depends on
// every error reaching this function being compiled-origin. If anyone
// ever wraps an upstream error in a PLUGIN-DECLARED error type before it
// gets here, errors.Is would have to call Unwrap() on a yaegi-proxied
// value from compiled code — the same reverse-direction dispatch that
// broke the hand-rolled walk — and would silently return false. Keep
// upstream error chains built exclusively from stdlib error types
// (fmt.Errorf, net/url, context), and keep yaegi-check's timeout probe
// in the gate: it is the only test that can see this class of failure.
func matchesSentinel(err, sentinel error) bool {
	return errors.Is(err, sentinel)
}

// isDeadlineExceeded reports whether err IS, or wraps, context.
// DeadlineExceeded — SHOULD-1 ruling (v0.22 review round): retry.go's
// isTransient deliberately excludes context.DeadlineExceeded from what it
// retries (retrying after the gateway's own deadline already lapsed is
// pointless), and recordProviderAttempt's fail determination below reuses
// isTransient unforked for every other classification — but a deadline
// the GATEWAY set and the upstream never answered inside is very much a
// provider-health signal, arguably the clearest one isTransient's
// retry-specific exclusion was never meant to hide from accounting. A
// context.Canceled error (the client walked away) is deliberately NOT
// matched here — that is not the provider's fault, and stays an
// attempt-only outcome, exactly like isTransient already treats it.
func isDeadlineExceeded(err error) bool {
	return matchesSentinel(err, context.DeadlineExceeded)
}

// recordProviderAttempt accounts ONE upstream HTTP attempt against
// provider (Feature A, v0.22), and against the (provider, model) pair too
// when model is non-empty — a caller that cannot cheaply know the upstream
// model before the attempt resolves (native passthrough, routes_
// passthrough.go's handlePassthrough: the model lives in the response
// body, read only after the attempt already happened) passes "" and gets
// provider-level accounting only.
//
// Every attempt increments an "attempts" counter; a provider-fault
// outcome additionally increments a "failures" counter. That outcome is
// exactly isTransient(resp, err)'s own classification (retry.go: a
// transport/connection error, HTTP 429, or a 5xx status) — reused, not
// forked, per spec ruling — WITH ONE NARROW ADDITION (SHOULD-1, v0.22
// review round): isDeadlineExceeded(err) also counts as a failure, even
// though isTransient itself returns false for it (see isDeadlineExceeded's
// own doc comment for why the two functions correctly disagree here). A
// context.Canceled error still counts as an attempt only, from both
// functions alike — the client walked away, not the provider's fault. A
// 4xx-but-not-429 response means the provider answered correctly to a
// request it did not like, which is not a provider-health signal, exactly
// the line isTransient already draws for "worth retrying".
//
// attempt-accounting: this is invoked once per upstream attempt, not once
// per logical request — attemptRecorderFromContext (providers.go) is
// looked up fresh inside retryPolicy.do's own retry loop and proxyUpstream's
// single client.Do call, so a chat completion retried twice before
// succeeding counts as two attempts here (one failure, one success), the
// same "count what was actually attempted" discipline
// countTargetRequests/-Request already apply to target scopes (mcp_a2a.go
// callers), consistent with those counters' own attempt/request semantics.
//
// Both counters are written at minute AND day granularity, for both the
// provider scope and the (provider, model) scope — the write side is
// unaffected by SHOULD-2's read-side change to providerUsage/
// providerCounterKeys below: writing both windows costs nothing extra
// (already one batched storeIncrMulti call either way), and it leaves
// the model-level minute counters available in the store for a future
// caller even though GET /admin/api/overview no longer reads them today.
// No month window: provider health is a now-and-today question (the
// Providers tab's success-rate badge, webui), not a billing one, so there
// is nothing here for a month-long retention window to serve.
//
// The store write itself is fire-and-forget (l.spawn — SHOULD-5, v0.22
// review round): entries is finished being built before spawn is called,
// and never touched again afterward, so capturing it in the closure below
// is race-free even though the write itself now runs on a goroutine this
// function does not wait for.
func (l *limiter) recordProviderAttempt(provider, model string, resp *http.Response, err error) {
	if provider == "" {
		return
	}
	now := l.now()
	fail := isTransient(resp, err) || isDeadlineExceeded(err)

	entries := make([]counterIncr, 0, 8)
	entries = append(entries,
		newCounterIncr(kindProvider, provider, metricProvAttempt, windowMin, now, 1, minWindowTTL),
		newCounterIncr(kindProvider, provider, metricProvAttempt, windowDay, now, 1, dayWindowTTL),
	)
	if fail {
		entries = append(entries,
			newCounterIncr(kindProvider, provider, metricProvFail, windowMin, now, 1, minWindowTTL),
			newCounterIncr(kindProvider, provider, metricProvFail, windowDay, now, 1, dayWindowTTL),
		)
	}
	if model != "" {
		id := providerModelScopeID(provider, model)
		entries = append(entries,
			newCounterIncr(kindProviderModel, id, metricProvAttempt, windowMin, now, 1, minWindowTTL),
			newCounterIncr(kindProviderModel, id, metricProvAttempt, windowDay, now, 1, dayWindowTTL),
		)
		if fail {
			entries = append(entries,
				newCounterIncr(kindProviderModel, id, metricProvFail, windowMin, now, 1, minWindowTTL),
				newCounterIncr(kindProviderModel, id, metricProvFail, windowDay, now, 1, dayWindowTTL),
			)
		}
	}
	l.spawn(func() {
		l.storeIncrMulti(entries) // no error return by contract (doc comment above, mirroring account()); ok is intentionally discarded
	})
}

// providerCounterKeysPerScope is the number of windowKey strings
// providerCounterKeys builds for a PROVIDER-level (kindProvider, or any
// kind other than kindProviderModel) scope — attempt/fail at minute, then
// attempt/fail at day: 4. A kindProviderModel scope reads half as many
// (providerModelCounterKeysPerScope, below) — SHOULD-2 ruling (v0.22
// review round): GET /admin/api/overview's own per-model minute counters
// fed nothing but a badge title, so a dashboard with N models paid N
// wasted minute-window reads on every 5s poll for data nobody displayed.
// providerCounterKeys and providerUsage's own slicing loop each branch on
// sc.kind == kindProviderModel directly rather than through a shared
// "key count for this kind" helper — with exactly two kinds and two
// branches, a third layer of indirection bought nothing a direct
// comparison did not already say more plainly, mirroring
// targetCounterKeysPerScope's fixed-stride convention exactly where the
// stride really IS fixed (every target scope there reads the same three
// keys) and diverging from it only where it now is not.
const (
	providerCounterKeysPerScope      = 4
	providerModelCounterKeysPerScope = 2
)

// providerCounterKeys returns providerCounterKeysPerScope (or, for a
// kindProviderModel scope, providerModelCounterKeysPerScope) windowKey
// strings providerUsage reads for one (kind, id) scope at time now: a
// kindProviderModel scope gets attempt/fail at day only; every other kind
// (kindProvider) gets attempt/fail at minute, then attempt/fail at day —
// matching providerCounters' own field order exactly so providerUsage can
// map storeGetMulti's result slice back by plain index.
func providerCounterKeys(kind, id string, now time.Time) []string {
	if kind == kindProviderModel {
		return []string{
			windowKey(kind, id, metricProvAttempt, windowDay, now),
			windowKey(kind, id, metricProvFail, windowDay, now),
		}
	}
	return []string{
		windowKey(kind, id, metricProvAttempt, windowMin, now),
		windowKey(kind, id, metricProvFail, windowMin, now),
		windowKey(kind, id, metricProvAttempt, windowDay, now),
		windowKey(kind, id, metricProvFail, windowDay, now),
	}
}

// providerCounters is one provider's or (provider, model) pair's current-
// window attempt/failure counters (Feature A, v0.22) — mirrors
// targetCounters' own shape and read-only, admin-view-only role.
// attemptsMinute/failuresMinute stay at their zero value for a
// kindProviderModel scope (SHOULD-2, providerCounterKeys' own doc
// comment): providerUsage never reads a model scope's minute keys at
// all, so this is not a real "zero traffic this minute" reading for a
// model the way it is for a provider — a caller must not treat it as one.
type providerCounters struct {
	attemptsMinute int64
	failuresMinute int64
	attemptsDay    int64
	failuresDay    int64
}

// providerUsage reads every scope's current attempt/failure counters in
// ONE storeGetMulti round trip — mirrors targetUsage's own single-batch
// discipline (limits.go) for the identical reason: GET
// /admin/api/overview must not pay one round trip per configured provider
// and model. scopes is built by the caller (buildAdminOverview, admin.go)
// as kindProvider entries for the provider-level rows followed by
// kindProviderModel entries for the per-model breakdown, all in one
// slice, so both share this single round trip. Order is preserved:
// providerUsage(scopes)[i] corresponds to scopes[i]. A failed read
// (store down + fail-closed) returns every zero-value counters, exactly
// like targetUsage — see its own doc comment for why that is acceptable
// here: nothing downstream treats a provider's zero counters as
// "confirmed no traffic" the way scopeUsage.storeDown guards against for
// a limited scope.
//
// Slicing is by running offset, not a fixed i*stride multiply (SHOULD-2,
// providerCounterKeysPerScope's own doc comment): a kindProvider and a
// kindProviderModel scope contribute a different number of keys to
// allKeys, so the flat result cannot be sliced back by a single constant
// stride the way targetUsage's fixed-shape scopes can.
func (l *limiter) providerUsage(scopes []limitScope) []providerCounters {
	out := make([]providerCounters, len(scopes))
	if len(scopes) == 0 {
		return out
	}

	now := l.now()
	allKeys := make([]string, 0, len(scopes)*providerCounterKeysPerScope)
	offsets := make([]int, len(scopes))
	for i, sc := range scopes {
		offsets[i] = len(allKeys)
		allKeys = append(allKeys, providerCounterKeys(sc.kind, sc.id, now)...)
	}

	vals, ok := l.storeGetMulti(allKeys)
	if !ok || len(vals) != len(allKeys) {
		return out // zero-value counters; see doc comment above
	}

	for i, sc := range scopes {
		start := offsets[i]
		if sc.kind == kindProviderModel {
			out[i] = providerCounters{attemptsDay: vals[start], failuresDay: vals[start+1]}
			continue
		}
		out[i] = providerCounters{
			attemptsMinute: vals[start],
			failuresMinute: vals[start+1],
			attemptsDay:    vals[start+2],
			failuresDay:    vals[start+3],
		}
	}
	return out
}

// usageKeysPerScope is the number of windowKey strings usageWindowKeys
// builds per scope, and the stride currentUsage's flat storeGetMulti
// result is sliced back into per-scope chunks by. Named here, closing a
// v0.2 final review wave follow-up (2026-08-20) that flagged the previous
// literal "6" as a magic-number stride: this task grows the per-scope key
// count from 6 to 8 for the tokens-in/tokens-out split, which is exactly
// the moment a silent off-by-N here would have gone unnoticed.
const usageKeysPerScope = 8

// scopeUsage is one scope's (a user's, a group's, or the synthetic total
// scope's — totalScopeKind) current-window counter values — the admin
// dashboard's usage table (spec §4, v0.2) and GET /admin/api/usage's
// total row (v0.2 data-layer task). tokensInPerDay/tokensOutPerDay and
// their per-month counterparts replace a single combined
// tokensPerDay/tokensPerMonth pair (v0.2 data-layer task, tokens-in/
// tokens-out split): a caller wanting the combined total a
// LimitsConfig.TokensPerDay/TokensPerMonth limit is actually evaluated
// against (checkAndCount's own probe loop, via buildBudgetProbes) sums
// the two itself.
type scopeUsage struct {
	kind               string // "user", "group", or "total", mirroring limitScope.kind
	id                 string
	requestsPerMinute  int64
	requestsPerDay     int64
	tokensInPerDay     int64
	tokensInPerMonth   int64
	tokensOutPerDay    int64
	tokensOutPerMonth  int64
	costPerDayMicros   int64
	costPerMonthMicros int64
	// storeDown reports whether reading any of the usageKeysPerScope
	// counters above failed closed (a configured store errored and
	// failOpen is false) — mirrors limitViolation.storeDown. Every value
	// field is 0 in that case; a caller must not present them as
	// "confirmed zero usage".
	storeDown bool
}

// usageWindowKeys returns the usageKeysPerScope windowKey strings
// currentUsage reads for sc at time now, in a fixed order — req/min,
// req/day, tokin/day, tokin/month, tokout/day, tokout/month, cost/day,
// cost/month — matching scopeUsage's field order exactly, so currentUsage
// can map storeGetMulti's result slice back to named fields by plain
// index.
func usageWindowKeys(sc limitScope, now time.Time) []string {
	return []string{
		windowKey(sc.kind, sc.id, metricReq, windowMin, now),
		windowKey(sc.kind, sc.id, metricReq, windowDay, now),
		windowKey(sc.kind, sc.id, metricTokIn, windowDay, now),
		windowKey(sc.kind, sc.id, metricTokIn, windowMonth, now),
		windowKey(sc.kind, sc.id, metricTokOut, windowDay, now),
		windowKey(sc.kind, sc.id, metricTokOut, windowMonth, now),
		windowKey(sc.kind, sc.id, metricCost, windowDay, now),
		windowKey(sc.kind, sc.id, metricCost, windowMonth, now),
	}
}

// currentUsage reads every scope's usageKeysPerScope current-window
// counters — the same (kind, id, metric, window) combinations
// checkAndCount/account already write — via the limiter's own
// storeGetMulti, so it applies the identical fail-open/fail-closed policy
// every enforcement read already does, and it is read-only: unlike
// checkAndCount, it never increments anything.
//
// ONE storeGetMulti call for the whole scopes slice (v0.2 final review
// wave, 2026-08-20; supersedes the "one call per scope" amendment this
// comment previously described, 2026-08-20 review): every scope's keys
// are flattened into a single round trip against the shared store,
// rather than N separate ones. The reviewer-confirmed reason for this
// second amendment: GET /admin/api/usage's per-scope loop was contending
// with request admission on the same store connection as live traffic,
// and N round trips (even pipelined usageKeysPerScope-at-a-time) scales
// with the number of configured users and groups in a way one round trip
// does not. buildAdminUsage (admin.go) drives this by concatenating its
// users, groups, and the total scope into one scopes slice before calling
// in, then slicing the flat result back into its response sections at the
// same split points.
//
// Tradeoff given up by this second amendment: a transient store error
// now marks every scope in the call storeDown together, where the
// previous per-scope loop could let one scope's failure land mid-batch
// while a later scope's own independent call still succeeded. That
// isolation was never a documented guarantee of this method, only an
// incidental property of how it was written — cutting N round trips to 1
// is the reviewer-confirmed right side of this tradeoff. Order is
// preserved: currentUsage(scopes)[i] corresponds to scopes[i].
func (l *limiter) currentUsage(scopes []limitScope) []scopeUsage {
	out := make([]scopeUsage, len(scopes))
	if len(scopes) == 0 {
		return out
	}

	now := l.now()
	allKeys := make([]string, 0, len(scopes)*usageKeysPerScope)
	for _, sc := range scopes {
		allKeys = append(allKeys, usageWindowKeys(sc, now)...)
	}

	vals, ok := l.storeGetMulti(allKeys)
	// len(vals) != len(allKeys) is reachable only from a non-conforming
	// counterStore implementation (a real respClient.getMulti and
	// memoryStore.getMulti both always return one value per key) —
	// guarded defensively so a future or test-only store's short slice
	// reports every scope storeDown instead of panicking on an
	// out-of-range index below (review sweep, 2026-08-20).
	if !ok || len(vals) != len(allKeys) {
		for i, sc := range scopes {
			out[i] = scopeUsage{kind: sc.kind, id: sc.id, storeDown: true}
		}
		return out
	}

	for i, sc := range scopes {
		v := vals[i*usageKeysPerScope : i*usageKeysPerScope+usageKeysPerScope]
		out[i] = scopeUsage{
			kind: sc.kind, id: sc.id,
			requestsPerMinute: v[0], requestsPerDay: v[1],
			tokensInPerDay: v[2], tokensInPerMonth: v[3],
			tokensOutPerDay: v[4], tokensOutPerMonth: v[5],
			costPerDayMicros: v[6], costPerMonthMicros: v[7],
		}
	}
	return out
}

// historyPoint is one bucket's counter value in a limiter.history result:
// bucket is the same string bucketFor produces for that instant (e.g.
// "2026082114" for an hour bucket), value is the counter's reading — 0
// for a bucket that was never written, matching get/getMulti's existing
// "0 for a missing key" contract.
type historyPoint struct {
	bucket string
	value  int64
}

// historyStepBack returns the instant window's bucket was current i steps
// before now: i=0 is now's own (current, possibly partial) bucket, i=1
// the previous one, and so on. hour and day step by a fixed duration/day
// count from now itself; month anchors on the 1st of now's own month
// first, then steps whole calendar months from there —
// time.Time.AddDate normalizes an overflowing day-of-month (e.g. "Mar 31"
// minus one month becomes "Mar 3", not "Feb 28/29"), so stepping from day
// 1 (which exists in every month) is the only way whole-month arithmetic
// stays exact regardless of what day of the month now is.
func historyStepBack(now time.Time, window string, i int) time.Time {
	u := now.UTC()
	switch window {
	case windowHour:
		return u.Add(-time.Duration(i) * time.Hour)
	case windowDay:
		return u.AddDate(0, 0, -i)
	case windowMonth:
		anchor := time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
		return anchor.AddDate(0, -i, 0)
	default:
		panic(fmt.Sprintf("llmgateway: historyStepBack: unknown window %q", window))
	}
}

// historyBucketKeys returns the span counterStore keys and their matching
// bucket label strings for (kind, id, metric, window), stepping back from
// now one whole window unit at a time — oldest first, the current
// (possibly partial) bucket last, matching GET /admin/api/usage/history's
// "inclusive of current bucket, oldest-first" contract.
func historyBucketKeys(kind, id, metric, window string, now time.Time, span int) (keys, buckets []string) {
	keys = make([]string, span)
	buckets = make([]string, span)
	for i := 0; i < span; i++ {
		t := historyStepBack(now, window, span-1-i)
		keys[i] = windowKey(kind, id, metric, window, t)
		buckets[i] = bucketFor(t, window)
	}
	return keys, buckets
}

// history returns span counter values for (kind, id, metric, window)
// ending at now, oldest-first — GET /admin/api/usage/history's data
// source (v0.2 data-layer task). It computes every bucket key up front
// and reads them in ONE storeGetMulti round trip, the same single-batch
// discipline currentUsage already applies to a scope's current-window
// keys, scaled here to a whole span of one metric/window instead of a
// fixed set of usageKeysPerScope. ok is false only in the fail-closed
// case (storeGetMulti's own contract): a caller must not present the
// returned points as real data then.
func (l *limiter) history(kind, id, metric, window string, now time.Time, span int) ([]historyPoint, bool) {
	keys, buckets := historyBucketKeys(kind, id, metric, window, now, span)
	vals, ok := l.storeGetMulti(keys)
	if !ok || len(vals) != len(keys) {
		return nil, false
	}
	points := make([]historyPoint, span)
	for i := range points {
		points[i] = historyPoint{bucket: buckets[i], value: vals[i]}
	}
	return points, true
}
