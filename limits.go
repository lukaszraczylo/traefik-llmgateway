package traefikllmgateway

import (
	"fmt"
	"math"
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
// LimitsConfig field ever names it, so evaluateScope has nothing to
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
// budget across both — see tokenBudgetViolation.
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
	// review, 2026-08-21) — it is kept for direct counter seeding in
	// tests (5 call sites, all in admin_test.go) and for counterStore
	// interface conformance.
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
	// 2026-08-21: checkAndCount/account previously paid one round trip per
	// counter — up to 9 and 27 respectively for a 3-scope request — this
	// collapses each down to one). An error fails the whole batch, never a
	// partial result, mirrored by the limiter's storeIncrMulti as a single
	// fail-open/fail-closed decision, the same shape storeGetMulti already
	// applies to a batch read.
	incrMulti(entries []counterIncr) ([]int64, error)
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
// 2026-08-21); it is kept for direct counter seeding in tests (5 call
// sites, all in admin_test.go's TestAdminUsage_MathAgainstSeededCounters)
// and for counterStore interface conformance.
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
// totalScopeID}, limits always nil. evaluateScope's nil-limits check
// (below) skips it during evaluation, so checkAndCount still counts it
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
	store    counterStore     // configured backend; nil means always use fallback (see newLimiter)
	fallback *memoryStore     // in-process counter store, always available
	nowFn    func() time.Time // injected for tests; defaults to time.Now
	// logf is a bound method value (g.errorf), injectable for tests; defaults
	// to a no-op. Its own call site (logStoreError) passes exactly one
	// variadic argument — never extend that to two or more without first
	// reading modelRegistry.log's doc comment in registry.go: a struct
	// field of variadic func type crashes Yaegi v0.16.1's CFG builder past
	// one variadic argument, even though the identical call through a
	// method or interface method does not.
	logf             func(format string, args ...any)
	lastLogAt        time.Time // guarded by logMu; last time a store error was logged
	lastStoreFailure time.Time // guarded by logMu; zero means the store-down latch is not open (see storeLatched)
	// lastErrMsg is the message of the most recent store operation
	// failure, guarded by logMu alongside lastStoreFailure. It is never
	// cleared on a later success — "last store error" for the admin
	// dashboard (spec §4, v0.2) means exactly that, a persisting fact,
	// not "is the store currently failing" (storeLatched already answers
	// that question for the enforcement path).
	lastErrMsg string
	logMu      sync.Mutex
	failOpen   bool // store-error policy: true falls back to fallback, false refuses the request
}

// newLimiter returns a limiter. A nil store means every operation uses the
// limiter's own in-process fallback memoryStore — the plugin still
// enforces limits with no distributed backend configured, just without
// sharing counters across Traefik instances. failOpen governs what happens
// when a non-nil store errors: true silently falls back to the in-process
// memoryStore for that operation, false refuses the request (see
// storeIncrBy / storeGet).
func newLimiter(store counterStore, failOpen bool) *limiter {
	return &limiter{
		store:    store,
		fallback: newMemoryStore(),
		nowFn:    time.Now,
		logf:     func(string, ...any) {},
		failOpen: failOpen,
	}
}

// now returns the limiter's current time, via nowFn.
func (l *limiter) now() time.Time {
	return l.nowFn()
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
// case — a caller must refuse the request (checkAndCount) or drop the
// sample (account) for that, rather than treating a nil/short slice as
// real counter readings.
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

// storeDownViolation is the violation checkAndCount/budgetViolation return
// when the configured store is unreachable and failOpen is false: the
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
// stride its flat storeIncrMulti result is sliced back into per-scope
// counts by.
const checkAndCountKeysPerScope = 3

// checkAndCount increments every scope's req:min, req:day, and req:hour
// counters — unconditionally, before any evaluation, so a request that
// ultimately gets refused by one scope's limit still counts toward every
// other scope's rate tracking — then evaluates each scope's set limits in
// order and returns the first violation found, or nil if the request may
// proceed. If the batch fails closed (a configured store errored and
// failOpen is false), it returns a storeDownViolation immediately — the
// request is refused rather than evaluated against partial or
// fallback-only counters.
//
// Every scope's three counters are built into ONE storeIncrMulti call
// (perf review, 2026-08-21): a 3-scope request (user, group, total —
// withTotalScope, routes_unified.go) previously paid up to 9 separate
// round trips here, one per counter. incrMulti's own reply already
// carries each entry's post-increment value in order, so no extra read
// is needed to recover req:min/req:day for evaluation below — only
// req:hour's slot goes unread, since windowHour is stats-only (its own
// doc comment) and never evaluated.
//
// Requests limits compare against the value just incremented in this call.
// Token and cost limits compare against the value already accumulated by
// account (via get): a request that would start over budget is refused,
// but because usage is only known after the upstream call completes, one
// request may still push a scope over its budget — an accepted trade-off.
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

	vals, ok := l.storeIncrMulti(entries)
	if !ok || len(vals) != len(entries) {
		return storeDownViolation()
	}

	for i, sc := range scopes {
		minCount := vals[i*checkAndCountKeysPerScope]
		dayCount := vals[i*checkAndCountKeysPerScope+1]
		// vals[i*checkAndCountKeysPerScope+2] is the req:hour count —
		// stats-only, deliberately never read here.
		if v := l.evaluateScope(sc, minCount, dayCount, now); v != nil {
			return v
		}
	}
	return nil
}

// evaluateScope checks one scope's already-set limits against its
// just-incremented request counts and its accumulated token/cost counts.
// A nil limits field means nothing to evaluate for this scope. In
// principle a nil-limits scope can still reach here with its req counters
// already incremented by checkAndCount, since that increment runs before
// any evaluation; in practice the unified route's buildLimitScopes
// (routes_unified.go, ruling e) never builds one — it omits a user or
// group scope from the slice entirely whenever that scope's own limits
// are nil — so this check is a defensive no-op today, not a path any
// current caller exercises.
func (l *limiter) evaluateScope(sc limitScope, minCount, dayCount int64, now time.Time) *limitViolation {
	if sc.limits == nil {
		return nil
	}
	lim := sc.limits

	if v := requestLimitViolation(sc, "requests-per-minute", lim.RequestsPerMinute, minCount, windowMin, now); v != nil {
		return v
	}
	if v := requestLimitViolation(sc, "requests-per-day", lim.RequestsPerDay, dayCount, windowDay, now); v != nil {
		return v
	}
	if v := l.tokenBudgetViolation(sc, "tokens-per-day", lim.TokensPerDay, windowDay, now); v != nil {
		return v
	}
	if v := l.tokenBudgetViolation(sc, "tokens-per-month", lim.TokensPerMonth, windowMonth, now); v != nil {
		return v
	}
	if v := l.budgetViolation(sc, metricCost, "cost-per-day", usdToMicros(lim.CostPerDayUSD), windowDay, now); v != nil {
		return v
	}
	if v := l.budgetViolation(sc, metricCost, "cost-per-month", usdToMicros(lim.CostPerMonthUSD), windowMonth, now); v != nil {
		return v
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

// budgetViolation reports a violation when the scope's accumulated
// counter for metric/window is already at or above limit. limit<=0 means
// unlimited. A failed-closed read (configured store errored, failOpen
// false) returns storeDownViolation rather than silently passing the
// check — a store outage must never look the same as staying under
// budget.
func (l *limiter) budgetViolation(sc limitScope, metric, name string, limit int64, window string, now time.Time) *limitViolation {
	if limit <= 0 {
		return nil
	}
	used, ok := l.getCounter(sc.kind, sc.id, metric, window, now)
	if !ok {
		return storeDownViolation()
	}
	if used < limit {
		return nil
	}
	return &limitViolation{
		message:    fmt.Sprintf("%s %q exceeded %s budget", sc.kind, sc.id, name),
		retryAfter: retryAfterSeconds(now, window),
	}
}

// tokenBudgetViolation mirrors budgetViolation for a token limit, which —
// unlike a request or cost limit — enforces a TOTAL budget over two split
// counters (metricTokIn, metricTokOut; v0.2 data-layer task): it reads
// both of the scope's accumulated tok-in/tok-out counters for window in
// ONE storeGetMulti round trip and compares their sum against limit.
// limit<=0 means unlimited. A fail-closed read returns
// storeDownViolation, matching budgetViolation's own contract; name is
// still e.g. "tokens-per-day", so the violation message still says
// "tokens" regardless of which direction pushed the total over.
func (l *limiter) tokenBudgetViolation(sc limitScope, name string, limit int64, window string, now time.Time) *limitViolation {
	if limit <= 0 {
		return nil
	}
	keys := []string{
		windowKey(sc.kind, sc.id, metricTokIn, window, now),
		windowKey(sc.kind, sc.id, metricTokOut, window, now),
	}
	vals, ok := l.storeGetMulti(keys)
	if !ok || len(vals) != 2 {
		return storeDownViolation()
	}
	used := vals[0] + vals[1]
	if used < limit {
		return nil
	}
	return &limitViolation{
		message:    fmt.Sprintf("%s %q exceeded %s budget", sc.kind, sc.id, name),
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
// against (tokenBudgetViolation) sums the two itself.
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
