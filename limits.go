package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
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
	// metricRej is the fleet-wide rejection counter F4 (v0.3 dashboard
	// task) adds: a day-bucketed count of every checkAndCount rejection,
	// per scope, folded into the SAME storeIncrMulti round trip
	// settleRejection already pays to compensate other scopes' counters
	// (below) — never a second round trip. Read back as
	// scopeUsage.rejectionsPerDay / adminUsageEntryView.RejectionsPerDay.
	metricRej = "rej"
)

// New counter-family metric names (admin-redesign WP-A): landed together,
// early, so WP-B's read-side endpoints (admin.go, stats_read.go) can code
// against the agreed names while WP-A finishes wiring their writers.
// metricProvTimeout is the provider-level "did not answer within budget"
// counter (recordProviderAttempt, below) — distinct from metricProvFail
// (isTransient's broader classification): every timeout is a fail, but not
// every fail is a timeout. metricFover is the failover counter
// (accountWith's accountExtras.failoverFrom): +1 against the FROM
// provider every time a request moves to its next candidate. metricR402
// is the "refused: unpriced model under a cost budget" counter
// (recordUnpriced402). metricChit/metricCsave/metricCmiss are the response
// cache's hit/avoided-cost/miss counters (recordCacheHit, accountWith's
// accountExtras.cacheMiss) — csave is micro-USD, the same unit metricCost
// already uses, not a request count.
const (
	metricProvTimeout = "timeout"
	metricFover       = "fover"
	metricR402        = "r402"
	metricChit        = "chit"
	metricCsave       = "csave"
	metricCmiss       = "cmiss"
)

// kindUserModel and kindTargetCaller are two more limitScope.kind (and
// admin-API scope-kind) strings this task adds, alongside kindProvider/
// kindProviderModel/kindModel above: kindUserModel is the opt-in per-
// (user, model) breakdown (userModelScopeID, below; admin.stats.userModel);
// kindTargetCaller is the per-(target, caller) breakdown countTargetRequestsBy
// writes (targetCallerScopeID, below). Both embed kind as their counter
// key's own leading segment, the same collision-safety every other kind
// constant in this file already relies on.
const (
	kindUserModel    = "umodel"
	kindTargetCaller = "tcaller"
)

// userModelScopeID builds kindUserModel's id for (user, canonical): the
// user name's own byte length (decimal ASCII), ":", then the user name
// immediately followed by canonical with NO separator between them. The
// length prefix is what makes this unambiguous to split back apart later
// (WP-B's usage/totals?kind=usermodel reader): a user name may itself
// contain any character a configured username allows, including one that
// could otherwise collide with a plain fixed separator, but canonical
// always starts exactly len(user) bytes after the first ":".
func userModelScopeID(user, canonical string) string {
	return strconv.Itoa(len(user)) + ":" + user + canonical
}

// targetCallerScopeID builds kindTargetCaller's id for (targetKind,
// target, caller): "{mcp|agent}/{target}/{caller}" — targetKind is
// targetScopeKind's own output ("mcp" or "agent", mcp_a2a.go), so this
// reuses the identical vocabulary GET /admin/api/targets already exposes
// rather than minting a third one.
func targetCallerScopeID(targetKind, target, caller string) string {
	return targetKind + "/" + target + "/" + caller
}

// latencyDurationMetric and latencyTTFBMetric name the per-bucket latency
// counters accountWith writes (opt-in, admin.stats.latency): idx is
// latencyBucketIndex's own return value (metrics.go) — 0..len(
// latencyBucketBounds)-1 for a real bucket, len(latencyBucketBounds) itself
// for the overflow bucket — so "ld00".."ld13"/"lt00".."lt13" for the 13
// configured bounds plus one overflow bucket each, matching plan §1.2's
// "ld00..ld13"/"lt00..lt13" vocabulary exactly. %02d, not a bare int, so
// every bucket's metric name sorts and reads consistently regardless of
// how many bounds latencyBucketBounds ever grows to hold (up to 100).
func latencyDurationMetric(idx int) string {
	return fmt.Sprintf("ld%02d", idx)
}

func latencyTTFBMetric(idx int) string {
	return fmt.Sprintf("lt%02d", idx)
}

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

// enforceTTLMinWindow/HourWindow/DayWindow/MonthWindow are enforceTTLFor's
// four return values, factored out as named constants so enforceTTLFor and
// enforceTTLsFor (computing all four windows' floors at once instead of one
// enforceTTLFor call per counterIncr) share a single source of truth
// instead of two switches that could silently drift apart.
const (
	enforceTTLMinWindow   = minWindowTTL
	enforceTTLHourWindow  = 2 * time.Hour
	enforceTTLDayWindow   = 25 * time.Hour
	enforceTTLMonthWindow = 32 * 24 * time.Hour
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
// expired.
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
	// absolute marks this entry as a plain SET, not an INCRBY: delta is
	// the exact value to write (e.g. a unix-second timestamp), not an
	// amount to add. Added for the last-seen counter family (Q6,
	// DECISIONS): a last-seen value is "when did we last see this scope",
	// which an increment can never express. redisStore.incrMulti/
	// incrAndGetMulti build a single "SET key delta EX secs" command for
	// an absolute entry instead of INCRBY(+EXPIRE) — see their own doc
	// comments — and memoryStore.applySet mirrors that: unconditionally
	// overwrite, never add. false (the zero value) is every existing
	// entry's own behavior, unchanged.
	absolute bool
}

// counterStore is the storage backend the limiter uses for atomic windowed
// counters. memoryStore (below) is the in-process fallback; a distributed
// (e.g. Redis-backed) implementation is wired in a later task.
//
// counterStore had single-key incrBy/get methods until the 2026-09 deadcode
// audit: checkAndCount/account moved onto the batched incrMulti/getMulti/
// incrAndGetMulti below in the 2026-08-21/22 perf reviews, which left
// incrBy/get with no production caller of any kind — every remaining call
// site was a test exercising a store implementation directly. They were
// removed from the interface and from memoryStore/redisStore; the tests
// that needed a single-key call now build a one-entry incrMulti batch or a
// one-key getMulti read instead (see limits_helpers_test.go and
// redis_store_test.go's incrOne/getOne).
type counterStore interface {
	// getMulti reads every key in keys in one round trip where the
	// backend supports it (redisStore pipelines via respClient.getBatch;
	// memoryStore's in-process map needs no such optimization but
	// implements the same contract for interface conformance), returning
	// one value per key in the same order — 0 for a key that does not
	// exist or has expired, matching get's contract. An error fails the
	// whole batch (mirrored by the limiter's storeGetMulti as a single
	// fail-open/fail-closed decision), never a partial result.
	getMulti(keys []string) ([]int64, error)
	// incrMulti applies counterIncr's per-entry contract to every entry in
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

// sweepEvery is the minimum real time between memoryStore's opportunistic
// expired-entry sweeps, regardless of how many keys memoryStore holds. A
// size-gated sweep (only above N keys) was measured to scan the whole map
// on every increment once live keys exceeded that threshold, since a sweep
// that frees nothing (all keys still live) never brings the count back
// down — a 167x increment cliff. Gating on elapsed time instead bounds sweep
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
// opportunistically from applyIncr, at most once every sweepEvery.
type memoryStore struct {
	data      map[string]*memoryEntry
	nowFn     func() time.Time // injected for tests; defaults to time.Now
	lastSweep time.Time        // guarded by mu; zero value sweeps on the first applyIncr
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

// applyIncr increments key by n — creating it fresh, expiring at
// now+ttl, if absent or already expired — and returns its new value. ttl
// must already be clamped by the caller: incrMulti clamps per entry with
// that entry's own enforceTTL floor (see clampTTL); the test-only incrBy
// helper (limits_helpers_test.go, restoring the single-key API removed
// from production in the 2026-09 deadcode audit — checkAndCount/account
// moved onto the batched incrMulti below in the 2026-08-21 perf review,
// leaving incrBy with no production caller) clamps with no enforcement
// floor, since a bare key carries no (kind, id, metric, window) tuple to
// derive one from. Sharing this one map-mutation-plus-opportunistic-sweep
// implementation between both callers means the
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

// applySet sets key's value to v and its expiry to now+ttl, unconditionally
// — creating the entry if absent, overwriting it if present — memoryStore's
// counterpart to Redis's SET key v EX secs (an absolute counterIncr entry's
// own command, redisStore.incrMulti). Unlike applyIncr, this never adds to
// an existing value and never treats "already expired" specially: every
// call simply replaces whatever was there, exactly like a real SET does.
func (m *memoryStore) applySet(key string, v int64, ttl time.Duration) int64 {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()

	if now.Sub(m.lastSweep) >= sweepEvery {
		m.sweepLocked(now)
		m.lastSweep = now
	}

	m.data[key] = &memoryEntry{expiry: now.Add(ttl), value: v}
	return v
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
		if e.absolute {
			out[i] = m.applySet(e.key, e.delta, clampTTL(e.ttl, e.enforceTTL))
			continue
		}
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
	// kind is one of the eventKindRateLimit/eventKindBudget/
	// eventKindStoreDown vocabulary (events.go, F3 v0.3 dashboard task) —
	// set once, at construction (storeDownViolation, requestLimitViolation,
	// checkAndCount's own budget-probe branch), so recordLimitEvent can
	// classify GET /admin/api/events' Kind field straight from it instead
	// of re-deriving the same distinction from v.message/v.storeDown a
	// second time.
	kind string
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

// lastSeenGateThrottle is the minimum real time between two last-seen SET
// writes THIS replica issues for the identical (kind, id) scope (Q6,
// DECISIONS: "last-seen via absolute SET entries in the admission
// pipeline, throttled 60s/replica/scope"). An absolute SET is idempotent
// and safe to send on every admitted request, but doing so would add a
// synchronous store round-trip cost proportional to request volume for a
// value nothing needs updated more often than this.
const lastSeenGateThrottle = 60 * time.Second

// lastSeenGateMapCapBase is lastSeenGate.capacity's starting value, and
// the floor it never shrinks below — the same generation-rotation shape
// rejectionCounter/authFailureTracker already use, and for the identical
// reason (rejectionCounterMapCap's own doc comment): a hot-reloaded
// user/group set can churn names over a long process lifetime, and
// nothing here should grow this map without bound for a renamed or
// deleted scope. A scope displaced by rotation simply writes its next
// last-seen SET a little early — never a correctness bug, only an extra
// write, the same trade-off rejectionCounter's own doc comment already
// accepts.
const lastSeenGateMapCapBase = 4096

// lastSeenGateMapCapMax is the hard ceiling lastSeenGate.capacity's own
// adaptive growth (below) never exceeds — bounding this replica's total
// last-seen throttle memory at, worst case, two generations of
// lastSeenGateMapCapMax entries (current + previous) regardless of how
// many distinct scopes this process ever admits. N5 (verify-redesign-
// final.md): a fixed 4,096 cap throttled correctly up to ~2x itself
// (~8,192 live scopes, current+previous both full), but a live scope
// count ABOVE that rotated current before lastSeenGateThrottle could
// ever elapse for most of it, discarding still-fresh writes on every
// rotation and degrading to one write per admission — the throttle it
// exists to provide. Past this ceiling the gate degrades gracefully back
// to the same fixed-size rotate/discard behaviour a below-ceiling gate
// always had, rather than growing without bound.
const lastSeenGateMapCapMax = 65536

// lastSeenGate throttles checkAndCount's own last-seen SET writes
// (lastSeenEntries, below) per (kind, id) scope, per replica: due reports
// true (and records now) only when this replica has never written one for
// that scope, or its last write was more than lastSeenGateThrottle ago.
// Guarded by mu since checkAndCount runs on every request's own goroutine
// concurrently.
//
// capacity is the CURRENT rotation threshold (starts at
// lastSeenGateMapCapBase, only ever grows, up to lastSeenGateMapCapMax —
// see due's own doc comment for when and why it grows) rather than the
// fixed lastSeenGateMapCap constant an earlier version used.
type lastSeenGate struct {
	current    map[string]int64 // kind+"\x00"+id -> unix seconds of this replica's last write
	previous   map[string]int64
	lastRotate time.Time // zero until the first rotation; read/written under mu, same as the two maps
	capacity   int
	mu         sync.Mutex
}

// due reports whether this replica should write a fresh last-seen SET for
// (kind, id) at now, recording now as that scope's new last-write time
// when it does.
//
// Adaptive capacity (N5 fix, verify-redesign-final.md): when current
// fills past g.capacity, due does not always rotate. It rotates (moving
// current to previous, starting a fresh current) only on that scope's
// FIRST overflow, or once at least lastSeenGateThrottle has passed since
// the previous rotation — the same signal a healthy, below-capacity gate
// already relies on (a population that fits comfortably takes at least
// that long to refill current). When current instead refills WITHIN one
// throttle window of the last rotation, that is direct evidence the live
// scope population exceeds 2x the current capacity: rotating now would
// immediately discard `previous`'s still-fresh writes and start evicting
// the very entries that just proved the map too small. Growing capacity
// instead (doubled, capped at lastSeenGateMapCapMax) lets current keep
// accumulating past the old threshold without losing anything already
// recorded, so a population that stabilizes below the ceiling ends up
// fully covered by current+previous, and the throttle holds for it the
// same way it always did below ~2x lastSeenGateMapCapBase.
func (g *lastSeenGate) due(kind, id string, now time.Time) bool {
	key := kind + "\x00" + id
	nowUnix := now.Unix()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.current == nil {
		g.current = make(map[string]int64)
		g.capacity = lastSeenGateMapCapBase
	}
	if last, ok := g.current[key]; ok {
		if nowUnix-last < int64(lastSeenGateThrottle/time.Second) {
			return false
		}
	} else if last, ok := g.previous[key]; ok {
		// P8 fix (admin dashboard redesign verify round): a scope
		// written before the last rotation must still throttle against
		// ITS OWN last write instead of being treated as brand new just
		// because current rotated out from under it — previous existed
		// for exactly this lookup, but nothing ever consulted it before
		// this fix, so every scope's throttle silently reset on its
		// first request after any rotation (i.e. constantly, once the
		// map holds more than g.capacity active scopes). Copy the found
		// timestamp forward into current so the NEXT lookup for this key
		// hits the fast path above; previous is left otherwise untouched
		// (it keeps answering for every other not-yet-migrated key until
		// the next rotation replaces it).
		if nowUnix-last < int64(lastSeenGateThrottle/time.Second) {
			g.current[key] = last
			return false
		}
	} else if len(g.current) >= g.capacity {
		if g.capacity < lastSeenGateMapCapMax && !g.lastRotate.IsZero() && now.Sub(g.lastRotate) < lastSeenGateThrottle {
			g.capacity *= 2
			if g.capacity > lastSeenGateMapCapMax {
				g.capacity = lastSeenGateMapCapMax
			}
		} else {
			g.previous = g.current
			g.current = make(map[string]int64, g.capacity)
			g.lastRotate = now
		}
	}
	g.current[key] = nowUnix
	return true
}

// lastSeenKey builds the counterStore key an absolute last-seen SET entry
// writes to for (kind, id): "llmgw:seen:{kind}:{id}" — a fixed key with no
// window-bucket component (unlike windowKey/windowKeyForBucket): a
// last-seen timestamp has no natural rollover, so there is exactly one key
// per scope, never one per bucket. Exposed for WP-B's own GET
// /admin/api/usage reader (usageWindowKeys, below, already appends this
// alongside every other per-scope key) to build the identical key.
func lastSeenKey(kind, id string) string {
	return "llmgw:seen:" + kind + ":" + id
}

// limiter enforces per-minute/day/month request, token, and cost limits
// using fixed windows keyed by windowKey.
type limiter struct {
	// fieldalignment (govet): pointer-shaped fields first, grouped ahead
	// of the two time.Time fields and every scalar below — same convention
	// providerState/adminProviderView already document (registry.go/
	// admin.go).
	lastStoreFailure time.Time    // guarded by logMu; zero means the store-down latch is not open (see storeLatched)
	lastLogAt        time.Time    // guarded by logMu; last time a store error was logged
	store            counterStore // configured backend; nil means always use fallback (see newLimiter)
	// lastSeen throttles checkAndCount's own last-seen SET writes (Q6,
	// DECISIONS) — always constructed by newLimiter (never nil), so every
	// limiter built either directly or via newConfiguredLimiter can call
	// lastSeenEntries safely regardless of whether admin stats end up
	// enabled.
	lastSeen *lastSeenGate
	fallback *memoryStore     // in-process counter store, always available
	nowFn    func() time.Time // injected for tests; defaults to time.Now
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
	// as a bare map increment costs one uncontended lock per rejection,
	// never a second network round trip, and never re-derives a number
	// the store already tracks.
	//
	// CORRECTION (adversarial verification, 2026-08-23): an earlier
	// version of this comment called the increment "the rare, not-hot-
	// path outcome of checkAndCount" — true for an ordinary single-scope
	// limit breach, but false for the storeDownViolation branch
	// (checkAndCount's own body, below): with a configured store down
	// and failOpen false, EVERY request takes that branch, so the
	// increment runs on every request for as long as the outage lasts,
	// not rarely. The honest claim is narrower: it is still cheaper than
	// the network round trip that already failed to produce this
	// violation in the first place, never an ADDITIONAL one.
	rejections rejectionCounter
	// lastErrMsg is the message of the most recent store operation
	// failure, guarded by logMu alongside lastStoreFailure. It is never
	// cleared on a later success — "last store error" for the admin
	// dashboard (spec §4, v0.2) means exactly that, a persisting fact,
	// not "is the store currently failing" (storeLatched already answers
	// that question for the enforcement path).
	lastErrMsg string
	logMu      sync.Mutex
	failOpen   bool // store-error policy: true falls back to fallback, false refuses the request
	// statsAdmin, statsUserModel, statsLatency gate the admin-redesign
	// counter families this task adds (DECISIONS: "Always-on-with-admin"
	// vs the two opt-in sub-flags) — set once, by newConfiguredLimiter,
	// from Config.Admin/Config.Admin.Stats; a limiter built directly via
	// newLimiter (most tests) defaults every one of these to false, so
	// existing tests observe byte-identical behavior to before this task
	// unless they opt in explicitly.
	//
	//   - statsAdmin: adminEnabled(cfg) — gates every "always-on-with-
	//     admin" family (last-seen, hourly provider attempt/fail/timeout,
	//     failover, rej/hour, r402, target x caller, cache counters).
	//   - statsUserModel: statsAdmin && Config.Admin.Stats.UserModel
	//     (default off) — gates the opt-in per-(user, model) breakdown.
	//   - statsLatency: statsAdmin && Config.Admin.Stats.Latency (default
	//     off) — gates the opt-in per-bucket latency counters. Already
	//     ANDed with statsAdmin at construction, so a call site need only
	//     check this one field, never both.
	statsAdmin     bool
	statsUserModel bool
	statsLatency   bool
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

// rejectionCounterMapCap bounds how many distinct (kind, id) scopes
// rejectionCounter.current tracks before rotating (review fix,
// adversarial verification 2026-08-23): replaceFileUsers (auth.go) can
// hot-reload the configured user/group set at any time, and nothing
// here ever removed a scope's entry when the user or group behind it
// was renamed or deleted — over a long-running process's lifetime, a
// deployment that renames or churns users keeps exporting a series FOR
// EVERY NAME EVER SEEN, not just currently configured ones, growing
// this map (and the resulting Prometheus series count) without bound.
// 2000 comfortably exceeds the 1,000-user scale this codebase already
// treats as a real deployment size elsewhere (adminUsageChunkScopes'
// own doc comment, admin.go) with headroom left for renamed-user churn
// and the synthetic store_down entry, while still bounding the worst
// case.
const rejectionCounterMapCap = 2000

// rejectionCounter accumulates rate/budget-limit-violation counts per
// scope for the lifetime of the process — limiter.rejections' own
// backing store; see its doc comment for why this exists as a bare
// in-process map rather than a counterStore key.
//
// current/previous generation-rotation (review fix, adversarial
// verification 2026-08-23) mirrors authFailureTracker's own shape
// (auth.go) for the identical reason: bound total memory without ever
// scanning. Rotation is a two-pointer swap (O(1)), triggered on
// inserting a brand-new scope once current is already at
// rejectionCounterMapCap; total memory is bounded to at most
// 2×rejectionCounterMapCap entries. Unlike authFailureTracker, there is
// no TTL — a Prometheus counter must only ever grow for as long as its
// series is being reported, so a scope's count is never read back out
// of previous once superseded (see snapshot, below): a scope displaced
// by rotation, if it is ever rejected again, starts a fresh count at 1
// rather than resuming its old total. That is a real, visible counter
// reset for that one series — accepted, the same trade-off
// authFailureTracker's own doc comment already accepts for a different
// bounded resource, and one Prometheus's own counter-reset handling in
// rate()/increase() already tolerates (the identical mechanism that
// already tolerates a process restart).
type rejectionCounter struct {
	current  map[rejectionScope]int64
	previous map[rejectionScope]int64
	mu       sync.Mutex
}

// increment records one rejection for (kind, id), initializing current
// on first use — rejectionCounter's zero value (as embedded, unexported,
// in limiter) is ready to use without a constructor. Rotates current ->
// previous before inserting a brand-new (kind, id) once current is
// already at rejectionCounterMapCap, mirroring authFailureTracker.
// increment's identical shape (auth.go).
func (c *rejectionCounter) increment(kind, id string) {
	key := rejectionScope{kind: kind, id: id}
	c.mu.Lock()
	if c.current == nil {
		c.current = make(map[rejectionScope]int64)
	}
	if _, ok := c.current[key]; !ok && len(c.current) >= rejectionCounterMapCap {
		c.previous = c.current
		c.current = make(map[rejectionScope]int64, rejectionCounterMapCap)
	}
	c.current[key]++
	c.mu.Unlock()
}

// rejectionSnapshot is one (kind, id) scope's rate-limit-rejection count
// — rejectionCounter.snapshot's own output, metrics.go's read of it.
type rejectionSnapshot struct {
	kind  string
	id    string
	count int64
}

// snapshot returns every scope in current with at least one recorded
// rejection, in no particular order (metrics.go sorts its own copy
// before rendering). previous is deliberately NOT merged in: a scope
// there was displaced by rotation, and re-reporting its stale, no-
// longer-growing count would misrepresent it as still current.
func (c *rejectionCounter) snapshot() []rejectionSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]rejectionSnapshot, 0, len(c.current))
	for scope, n := range c.current {
		out = append(out, rejectionSnapshot{kind: scope.kind, id: scope.id, count: n})
	}
	return out
}

// pruneUserScopes deletes every "user"-kind entry from both current and
// previous whose id is not in keep — limiter.pruneRejections' own
// backing implementation; see that method's doc comment for when and
// why this runs. Walking and deleting from a live map mid-range is safe
// in Go (deleting the current key during a range is explicitly
// permitted); previous is included so a recently-rotated, still-stale
// entry for a deleted user does not linger there either.
func (c *rejectionCounter) pruneUserScopes(keep map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for scope := range c.current {
		if scope.kind == "user" && !keep[scope.id] {
			delete(c.current, scope)
		}
	}
	for scope := range c.previous {
		if scope.kind == "user" && !keep[scope.id] {
			delete(c.previous, scope)
		}
	}
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

// pruneRejections removes every "user"-kind rejection scope whose id is
// not in keep (review fix, adversarial verification 2026-08-23): a user
// removed from a hot-reloaded users file (auth.go's replaceFileUsers)
// must not keep exporting llmgateway_rate_limit_rejections_total
// forever. Called only from ServeHTTP, and only when authStore.
// maybeReload just reported a REAL reload — see that method's own doc
// comment (users_file.go) for why this must never run on every request.
// Scope kinds other than "user" (group, the synthetic total and
// store_down entries) are left untouched: groups and the total scope
// are never hot-reloaded (Config.Groups is fixed at construction), and
// store_down names no real scope at all.
func (l *limiter) pruneRejections(keep map[string]bool) {
	l.rejections.pruneUserScopes(keep)
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
// storeIncrMulti / storeGetMulti).
//
// Known limitation, documented rather than fixed (verify-core review,
// round 4, finding 8 — accepted as a cost too high for the benefit): with
// failOpen=true, EVERY Traefik instance running this plugin falls back to
// its OWN private, unshared memoryStore for the whole duration of a
// configured store outage (storeLatched's 5s latch, re-opened on each new
// failure). Two instances behind the same load balancer therefore each
// enforce limits against a DIFFERENT count for the same scope during that
// window — user/group/total totals visibly drift apart across backends —
// and neither fallback's counts are reconciled back into the shared store
// once it recovers; each instance's fallback simply stops being read
// again. This under-enforces (each instance only sees its own share of
// traffic against the full limit) rather than over-enforces, and is
// bounded to the outage window itself, so it is accepted as a fail-open
// trade-off rather than fixed — a cross-instance reconciliation pass
// would need either a persistent pending-delta queue replayed into the
// store on recovery, or a broadcast mechanism between instances, either
// of which is a materially larger change than this review's scope.
func newLimiter(store counterStore, failOpen bool) *limiter {
	l := &limiter{
		store:       store,
		fallback:    newMemoryStore(),
		nowFn:       time.Now,
		logf:        func(string, ...any) {},
		failOpen:    failOpen,
		spawnTokens: make(chan struct{}, providerAttemptSpawnCap),
		lastSeen:    &lastSeenGate{},
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
// recorded store failure. When true, storeIncrMulti/storeGetMulti/
// storeIncrAndGetMulti must skip the network call entirely and go straight
// to the fail-open/fail-closed
// policy — the same outcome a fresh call would reach anyway, just without
// paying respCallTimeout again to find out. Guarded by logMu, the same
// mutex lastLogAt already uses for this kind of small timestamp state.
func (l *limiter) storeLatched() bool {
	l.logMu.Lock()
	defer l.logMu.Unlock()
	return !l.lastStoreFailure.IsZero() && l.now().Sub(l.lastStoreFailure) < storeDownLatchFor
}

// configuredStoreDown reports whether l has a configured store that is
// currently within its failure latch (storeLatched) — meaning any read
// made right now is served from the in-process fallback, never the
// real, shared store, REGARDLESS of what storeGetMulti's own ok return
// says. l.store == nil (no store configured at all — a fallback-only
// deployment) always reports false: there, the fallback IS the source
// of truth by design, nothing has degraded.
//
// This exists because storeGetMulti's ok=true does not distinguish "the
// store answered for real" from "the store is down, failOpen fell back
// to the empty in-process fallback, which answered with zeros" — and
// failOpen defaults to true (newConfiguredLimiter, llmgateway.go). A
// caller that only checks ok, as currentUsage/providerUsage's callers
// used to, reports a fail-OPEN store outage as confirmed real usage:
// exactly the fabricated-counter-reset bug metrics.go's storeDown skip
// was built to prevent, except on the DEFAULT config, where it was
// never actually reachable (review fix, adversarial verification
// 2026-08-23) — see currentUsage/providerUsage's own doc comments for
// where this is now checked.
func (l *limiter) configuredStoreDown() bool {
	return l.store != nil && l.storeLatched()
}

// recordStoreFailure logs err (rate-limited, see logStoreError) and opens
// the store-down latch: every storeIncrMulti/storeGetMulti/
// storeIncrAndGetMulti call for the next storeDownLatchFor skips the
// network call and applies the fail-open/fail-closed policy directly.
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

// storeGetMulti applies the limiter's fail-open/fail-closed/latched
// policy to a batch read: one round trip against the configured store (or
// the in-process fallback) for the whole slice of keys. ok is false only
// in the fail-closed case — a caller must not read the returned slice as
// real values when ok is false.
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

// failPolicyGetMulti applies the limiter's fail-open/fail-closed policy
// for a batch read the caller has decided not to attempt against the
// configured store (it just failed, or the store-down latch is open):
// failOpen=true reads through to the fallback instead; failOpen=false
// reports the operation as failed.
func (l *limiter) failPolicyGetMulti(keys []string) ([]int64, bool) {
	if !l.failOpen {
		return nil, false
	}
	v, _ := l.fallback.getMulti(keys)
	return v, true
}

// storeIncrMulti mirrors storeGetMulti for a batch of increments: one round
// trip against the configured store (or the in-process fallback) for the
// whole slice, applying the identical fail-open/fail-closed/latched
// policy storeGetMulti applies per batch. ok is false only in the fail-closed
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

// failPolicyIncrMulti mirrors failPolicyGetMulti for a batch increment.
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

// countAsync spawns entries as one fire-and-forget storeIncrMulti batch,
// off the request's hot path (l.spawn) — the shared primitive behind every
// async-only counter family this task adds: a cache HIT's chit/csave
// (recordCacheHit) and a refused-unpriced-model's r402 (recordUnpriced402),
// mirroring recordProviderAttempt's own long-established "build entries,
// then spawn the write" shape, factored out here so it is written once. A
// nil/empty entries slice spawns nothing at all.
func (l *limiter) countAsync(entries []counterIncr) {
	if len(entries) == 0 {
		return
	}
	l.spawn(func() {
		l.storeIncrMulti(entries)
	})
}

// storeDownViolation is the violation checkAndCount returns when the
// configured store is unreachable and failOpen is false: the
// request is refused instead of silently enforcing limits against a
// non-shared fallback, so a backend outage cannot let every configured
// limit go unenforced across a fleet of gateway instances.
func storeDownViolation() *limitViolation {
	return &limitViolation{message: "limit store unavailable", storeDown: true, kind: eventKindStoreDown}
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

// windowBuckets holds all four windows' bucketFor strings for one instant,
// as bucketsFor (below) computes them: checkAndCount/account/
// countTargetRequests/recordProviderAttempt each need several of these
// together (never just one) and call bucketsFor(now) exactly once per
// call, computing every bucket it needs once — instead of once per
// counterIncr, as windowKey/bucketFor did directly before this type
// existed.
type windowBuckets struct {
	min, hour, day, month string
}

// bucketsFor computes t's four window buckets via bucketFor, once each,
// for a caller that needs several of them together.
func bucketsFor(t time.Time) windowBuckets {
	return windowBuckets{
		min:   bucketFor(t, windowMin),
		hour:  bucketFor(t, windowHour),
		day:   bucketFor(t, windowDay),
		month: bucketFor(t, windowMonth),
	}
}

// enforceTTLSet holds all four windows' enforceTTLFor floors together, the
// TTL-side counterpart to windowBuckets — built fresh by enforceTTLsFor on
// every call (unlike windowBuckets, these four values are the same
// compile-time constants regardless of t, so there is nothing to cache).
type enforceTTLSet struct {
	min, hour, day, month time.Duration
}

// enforceTTLsFor returns enforceTTLFor's four window floors at once, so
// checkAndCount/account/countTargetRequests/recordProviderAttempt can
// build every counterIncr in the call from one shared value instead of
// calling enforceTTLFor per entry.
func enforceTTLsFor() enforceTTLSet {
	return enforceTTLSet{min: enforceTTLMinWindow, hour: enforceTTLHourWindow, day: enforceTTLDayWindow, month: enforceTTLMonthWindow}
}

// windowKeyForBucket builds the counterStore key for one (kind, id,
// metric, window) counter given an already-computed bucket string:
// llmgw:{kind}:{id}:{metric}:{window}:{bucket}. windowKey (below) is the
// bucketFor(t, window)-deriving convenience wrapper every existing caller
// still uses; checkAndCount/account/countTargetRequests/
// recordProviderAttempt call this directly with a bucket from their own
// bucketsFor(now) call instead, skipping a redundant bucketFor call per
// counterIncr when several entries in the same call already share the
// same window's bucket.
func windowKeyForBucket(kind, id, metric, window, bucket string) string {
	return strings.Join([]string{"llmgw", kind, id, metric, window, bucket}, ":")
}

// windowKey builds the counterStore key for one (kind, id, metric, window)
// counter at time t: llmgw:{kind}:{id}:{metric}:{window}:{bucket}. bucket
// is t.UTC() formatted to the window's granularity (bucketFor), so a
// counter's key changes automatically when its window rolls over.
func windowKey(kind, id, metric, window string, t time.Time) string {
	return windowKeyForBucket(kind, id, metric, window, bucketFor(t, window))
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
// counters — unconditionally, before any evaluation — then evaluates each
// scope's set limits, in order, and returns the first violation found, or
// nil if the request may proceed. If the round trip fails closed (a
// configured store errored and failOpen is false), it returns a
// storeDownViolation immediately — the request is refused rather than
// evaluated against partial or fallback-only counters.
//
// On a violation, settleRejection (finding F1, 2026-09 review; extended by
// F4, v0.3 dashboard task, to also record the fleet-wide rejection —
// see its own doc comment) compensates the increments made above for
// every scope OTHER than the one that actually violated: the violating
// scope's own counter still reflects a real admission attempt against
// ITS OWN limit — unchanged, intentional — but WITHOUT the rollback,
// every OTHER scope (a user's group, the synthetic total scope) would
// also count that same rejected request toward ITS rate tracking, even
// though it never had anything to do with the rejection. Left unrolled-
// back, a single user hammering past their own requests-per-minute limit
// drives their group's identical counter up on every one of those 429s
// too, and can lock out every OTHER member of the group once the group's
// own limit is reached from traffic that was never actually admitted.
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
	wb := bucketsFor(now)
	et := enforceTTLsFor()

	entries := make([]counterIncr, 0, len(scopes)*checkAndCountKeysPerScope)
	for _, sc := range scopes {
		entries = append(entries,
			counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricReq, windowMin, wb.min), delta: 1, ttl: minWindowTTL, enforceTTL: et.min},
			counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricReq, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
			counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricReq, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
		)
	}
	// Last-seen (Q6, DECISIONS) no longer rides this batch (P9 fix, admin
	// dashboard redesign verify round): it used to be appended here,
	// unconditionally, which meant a REJECTED request still moved that
	// scope's last-seen timestamp forward before checkAndCount even knew
	// whether it was about to refuse the request. recordLastSeen, below,
	// is now called by every admission call site's own "violation == nil"
	// branch instead — see its own doc comment.
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
			l.settleRejection(entries, i, scopes, wb, et)
			return v
		}
		if v := requestLimitViolation(sc, "requests-per-day", sc.limits.RequestsPerDay, dayCount, windowDay, now); v != nil {
			l.rejections.increment(sc.kind, sc.id)
			l.settleRejection(entries, i, scopes, wb, et)
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
			l.settleRejection(entries, i, scopes, wb, et)
			return &limitViolation{
				message:    fmt.Sprintf("%s %q exceeded %s budget", sc.kind, sc.id, p.name),
				retryAfter: retryAfterSeconds(now, p.window),
				kind:       eventKindBudget,
			}
		}
	}
	return nil
}

// recordLastSeen writes the absolute last-seen SET entries lastSeenEntries
// (below) builds for scopes, fire-and-forget (countAsync/l.spawn) — P9 fix
// (admin dashboard redesign verify round): every admitRequestForRoute-
// shaped call site calls this ONLY from its own "violation == nil" branch,
// after checkAndCount has already reported admission, so a request
// checkAndCount refuses never touches last-seen at all. Moving it off
// checkAndCount's own synchronous batch adds no new round trip to the
// admitted path either — countAsync already spawns off the request's own
// goroutine, the same shape recordCacheHit/recordUnpriced402 use.
func (l *limiter) recordLastSeen(scopes []limitScope, now time.Time) {
	l.countAsync(l.lastSeenEntries(scopes, now))
}

// lastSeenEntries returns the absolute last-seen SET entries recordLastSeen
// (above) spawns — one per "user" or "group" scope in scopes (never the
// synthetic total scope: a fleet-wide "when was anyone last seen" has no
// meaning), skipped entirely when admin stats are off (l.statsAdmin) or
// this replica already wrote one for that scope within lastSeenGateThrottle
// (l.lastSeen.due). TTL is monthWindowTTL
// (400d, matching plan §1.2's last-seen retention) with no enforceTTL
// floor (0): unlike a windowed counter, a last-seen value has no
// correctness-driven minimum lifetime — memoryStoreMaxTTL's own ceiling is
// free to shorten the in-process fallback's copy, since it is refreshed on
// every admitted request that scope makes anyway.
func (l *limiter) lastSeenEntries(scopes []limitScope, now time.Time) []counterIncr {
	if !l.statsAdmin {
		return nil
	}
	var out []counterIncr
	for _, sc := range scopes {
		if sc.kind != "user" && sc.kind != "group" {
			continue
		}
		if !l.lastSeen.due(sc.kind, sc.id, now) {
			continue
		}
		out = append(out, counterIncr{key: lastSeenKey(sc.kind, sc.id), delta: now.Unix(), ttl: monthWindowTTL, absolute: true})
	}
	return out
}

// settleRejection compensates the req:min/req:day/req:hour increments
// checkAndCount already made for every scope OTHER than violatedIdx —
// exactly what rollbackOtherScopeCounts (its pre-F4 name) did — AND, in
// the SAME batched store call, records the fleet-wide rejection: +1 to
// the violated scope's own rej:day counter, and +1 to the synthetic total
// scope's rej:day counter when one is present in scopes (F4, v0.3
// dashboard task — GET /admin/api/usage's rejectionsPerDay field, Q6:
// "attribute rejections to total/all too"). Folding this into the
// compensating batch, rather than a separate storeIncrMulti call, means a
// rejection still pays exactly the one further round trip it already did
// before F4 existed — see checkAndCount's own doc comment for the round-
// trip accounting this preserves.
func (l *limiter) settleRejection(entries []counterIncr, violatedIdx int, scopes []limitScope, wb windowBuckets, et enforceTTLSet) {
	comp := make([]counterIncr, 0, len(scopes)*checkAndCountKeysPerScope+4)
	totalPresent := false
	for i, sc := range scopes {
		if i == violatedIdx {
			continue
		}
		for k := 0; k < checkAndCountKeysPerScope; k++ {
			e := entries[i*checkAndCountKeysPerScope+k]
			comp = append(comp, counterIncr{key: e.key, delta: -e.delta, ttl: e.ttl, enforceTTL: e.enforceTTL})
		}
		if sc.kind == totalScopeKind && sc.id == totalScopeID {
			totalPresent = true
		}
	}
	violated := scopes[violatedIdx]
	comp = append(comp, rejectionCounterIncr(violated.kind, violated.id, windowDay, wb.day, et.day))
	if totalPresent {
		comp = append(comp, rejectionCounterIncr(totalScopeKind, totalScopeID, windowDay, wb.day, et.day))
	}
	// rej/hour (always-on-with-admin, DECISIONS): rides this SAME
	// compensation batch, never a second round trip, alongside the
	// existing always-on rej/day entries above.
	if l.statsAdmin {
		comp = append(comp, rejectionCounterIncr(violated.kind, violated.id, windowHour, wb.hour, et.hour))
		if totalPresent {
			comp = append(comp, rejectionCounterIncr(totalScopeKind, totalScopeID, windowHour, wb.hour, et.hour))
		}
	}
	if _, ok := l.storeIncrMulti(comp); !ok {
		l.logf("limits: rollback/rejection-accounting batch failed after a rejection; other scopes' req:min/day/hour counters may be over-counted by 1, and rejectionsPerDay may undercount, for the current window")
	}
}

// rejectionCounterIncr builds the single rej counterIncr entry
// settleRejection adds per scope it attributes a rejection to, for window
// (windowDay, always-on; windowHour, admin-gated) — delta 1, ttl the
// window's own matching constant (dayWindowTTL/hourWindowTTL), enforceTTL
// the caller's own et.day/et.hour.
func rejectionCounterIncr(kind, id, window, bucket string, enforceTTL time.Duration) counterIncr {
	ttl := dayWindowTTL
	if window == windowHour {
		ttl = hourWindowTTL
	}
	return counterIncr{key: windowKeyForBucket(kind, id, metricRej, window, bucket), delta: 1, ttl: ttl, enforceTTL: enforceTTL}
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
		kind:       eventKindRateLimit,
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
// 27 total — one single-key store write each. account's return-value discipline
// is unchanged by the batching: it has no error return, and a sample that
// hits a fail-closed batch (see storeIncrMulti) is dropped in full,
// silently, rather than counting it in the fallback — dropping avoids
// double-counting once the store recovers, the same reasoning that
// applied per-key before this call became one batch.
// accountExtras carries the per-call, non-scope facts accountWith's own
// extra counter families need — never a limitScope, since none of these
// are things a request is admitted or budgeted against, only recorded
// alongside its usage. account (limits_helpers_test.go, admin-redesign
// WP-G cleanup: every production call site now calls accountWith
// directly, so account itself is test-only) is accountWith with a
// zero-value accountExtras, so every pre-existing caller's behavior is
// byte-for-byte unchanged.
type accountExtras struct {
	// provider is the serving candidate's own provider name — accountWith
	// writes its opt-in latency buckets (statsLatency) under this, and its
	// failover-from entry (below) is keyed by the PREVIOUS candidate's
	// provider name, not this one.
	provider string
	// failoverFrom is the previous candidate's provider name, set only
	// when this account call is for a candidate reached after an earlier
	// one failed (i > 0 in runMeteredCall's own loop) — accountWith
	// increments prov/{failoverFrom}:fover when non-empty and admin stats
	// are on.
	failoverFrom string
	// lat/hasLat carry one completed upstream watchdogBody's latency
	// sample (metrics.go), when withLatencyRecorder was wired for this
	// call and it actually fired — hasLat false means no sample arrived
	// (metrics disabled and stats.latency off, or the watchdog never
	// closed).
	lat    latencySample
	hasLat bool
	// cacheMiss reports whether THIS candidate's own cacheable response-
	// cache lookup (routes_unified.go) was attempted and missed — always
	// false for a request the cache was never consulted for. A HIT never
	// reaches accountWith at all (routes_unified.go returns before ever
	// calling it); recordCacheHit, below, is that path's own async
	// counterpart.
	cacheMiss bool
	// noModelHistograms suppresses accountWith's own model-scope latency
	// AND user x model (umodel) writes even when scopes DOES carry a
	// kindModel scope (P13 fix, admin dashboard redesign verify round:
	// plan §2(c) says passthrough gets "provider histograms only", but
	// routes_passthrough.go always passed the served model's scope, so
	// latency (28 metrics x hour/day) and umodel entries were ALSO
	// written keyed by an upstream-echoed model id — bounded only by
	// length/control-char, never by a configured catalog, and never read
	// back anywhere: usage/models and usermodel totals both rank off the
	// CONFIGURED catalog, not whatever a passthrough response happened to
	// name). The base req/tokin/tokout/cost model-scope counters (the
	// per-scope loop above, driven purely by which scopes the caller
	// passed in) are unaffected — only the two opt-in, per-model-keyed
	// families this flag names.
	noModelHistograms bool
}

// accountWith is account generalized with accountExtras (admin-redesign
// WP-A): every scope's own req/tokin/tokout/cost entries below are
// byte-for-byte what account always built; x's own fields each add AT
// MOST a few more counterIncr entries to the SAME batch, never a second
// storeIncrMulti round trip — see accountExtras' own field docs and this
// function's per-block comments below for exactly what each one writes
// and when.
func (l *limiter) accountWith(scopes []limitScope, u usage, costMicros int64, x accountExtras) {
	now := l.now()
	wb := bucketsFor(now)
	et := enforceTTLsFor()

	// Security audit run-1: every figure reaching here is read verbatim
	// out of an UPSTREAM response body (provider_openai.go's forwardJSON,
	// the translate_*.go stream states, extractPassthroughUsage) into an
	// int64, and is written below as an INCRBY DELTA. A negative value
	// therefore DECREMENTS this principal's counters — and because
	// checkAndCount admits while `used < limit`, a counter driven below
	// zero stops every token and cost budget binding for the rest of that
	// window. costMicros can also arrive negative from an int64 overflow
	// in costMicros' own unguarded multiply (pricing.go) on an absurd
	// reported token count.
	//
	// These counters are increment-only by contract, so a negative delta
	// is never legitimate: drop it and say so, loudly enough that a
	// misbehaving or hostile upstream is visible rather than silently
	// rewriting a budget. Clamped per-component, so one bad field does not
	// discard the other two, which may be perfectly valid.
	if u.prompt < 0 || u.completion < 0 || costMicros < 0 {
		l.logf("%s", fmt.Sprintf("limits: dropping negative usage reported by an upstream (prompt=%d completion=%d costMicros=%d); these counters are increment-only", u.prompt, u.completion, costMicros))
		if u.prompt < 0 {
			u.prompt = 0
		}
		if u.completion < 0 {
			u.completion = 0
		}
		if costMicros < 0 {
			costMicros = 0
		}
	}

	entries := make([]counterIncr, 0, len(scopes)*12+16)
	userScopeCount := 0
	var userScopeID string
	hasModelScope := false
	var modelCanonical string
	for _, sc := range scopes {
		if sc.kind == "user" {
			userScopeCount++
			userScopeID = sc.id
		}
		if sc.kind == kindModel {
			hasModelScope = true
			modelCanonical = sc.id
		}
		// A kindModel scope is the one scope kind checkAndCount never
		// sees: it is resolved post-response, because only then is the
		// SERVING model known (failover can move a request to another
		// provider after admission). So its request counter is written
		// here rather than at admission, in this same batch — no extra
		// round trip, and no double count, since admission never wrote
		// one for this kind. Unconditional, unlike the three metrics
		// below: a served request counts even when the upstream reports
		// no usage at all.
		if sc.kind == kindModel {
			entries = append(entries,
				counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricReq, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
				counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricReq, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
				counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricReq, windowMonth, wb.month), delta: 1, ttl: monthWindowTTL, enforceTTL: et.month},
			)
		}
		if u.prompt != 0 {
			entries = append(entries,
				counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricTokIn, windowHour, wb.hour), delta: u.prompt, ttl: hourWindowTTL, enforceTTL: et.hour},
				counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricTokIn, windowDay, wb.day), delta: u.prompt, ttl: dayWindowTTL, enforceTTL: et.day},
				counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricTokIn, windowMonth, wb.month), delta: u.prompt, ttl: monthWindowTTL, enforceTTL: et.month},
			)
		}
		if u.completion != 0 {
			entries = append(entries,
				counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricTokOut, windowHour, wb.hour), delta: u.completion, ttl: hourWindowTTL, enforceTTL: et.hour},
				counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricTokOut, windowDay, wb.day), delta: u.completion, ttl: dayWindowTTL, enforceTTL: et.day},
				counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricTokOut, windowMonth, wb.month), delta: u.completion, ttl: monthWindowTTL, enforceTTL: et.month},
			)
		}
		if costMicros != 0 {
			entries = append(entries,
				counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricCost, windowHour, wb.hour), delta: costMicros, ttl: hourWindowTTL, enforceTTL: et.hour},
				counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricCost, windowDay, wb.day), delta: costMicros, ttl: dayWindowTTL, enforceTTL: et.day},
				counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricCost, windowMonth, wb.month), delta: costMicros, ttl: monthWindowTTL, enforceTTL: et.month},
			)
		}
	}

	// (a) user x model (opt-in, admin.stats.userModel): only when exactly
	// ONE user scope is present (a multi-group principal's several group
	// scopes never trigger this — there is still only one user) and a
	// served model is known. <=8 more entries: req always (a served
	// request counts even with zero usage, mirroring kindModel's own
	// unconditional req write above), tokin/tokout/cost only when nonzero
	// — day and month only, no hour (plan §1.2's own umodel row). Media's
	// account(withModelScope(nil, ...), usage{}, 0) call has no user scope
	// at all (scopes is nil there) and so never reaches this block.
	if l.statsUserModel && userScopeCount == 1 && hasModelScope && !x.noModelHistograms {
		id := userModelScopeID(userScopeID, modelCanonical)
		entries = append(entries,
			counterIncr{key: windowKeyForBucket(kindUserModel, id, metricReq, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
			counterIncr{key: windowKeyForBucket(kindUserModel, id, metricReq, windowMonth, wb.month), delta: 1, ttl: monthWindowTTL, enforceTTL: et.month},
		)
		if u.prompt != 0 {
			entries = append(entries,
				counterIncr{key: windowKeyForBucket(kindUserModel, id, metricTokIn, windowDay, wb.day), delta: u.prompt, ttl: dayWindowTTL, enforceTTL: et.day},
				counterIncr{key: windowKeyForBucket(kindUserModel, id, metricTokIn, windowMonth, wb.month), delta: u.prompt, ttl: monthWindowTTL, enforceTTL: et.month},
			)
		}
		if u.completion != 0 {
			entries = append(entries,
				counterIncr{key: windowKeyForBucket(kindUserModel, id, metricTokOut, windowDay, wb.day), delta: u.completion, ttl: dayWindowTTL, enforceTTL: et.day},
				counterIncr{key: windowKeyForBucket(kindUserModel, id, metricTokOut, windowMonth, wb.month), delta: u.completion, ttl: monthWindowTTL, enforceTTL: et.month},
			)
		}
		if costMicros != 0 {
			entries = append(entries,
				counterIncr{key: windowKeyForBucket(kindUserModel, id, metricCost, windowDay, wb.day), delta: costMicros, ttl: dayWindowTTL, enforceTTL: et.day},
				counterIncr{key: windowKeyForBucket(kindUserModel, id, metricCost, windowMonth, wb.month), delta: costMicros, ttl: monthWindowTTL, enforceTTL: et.month},
			)
		}
	}

	// (c) failover (always-on-with-admin): +1 to the PREVIOUS candidate's
	// own prov/{from}:fover hour+day counters, every time runMeteredCall's
	// own loop moves to a next candidate after an earlier one failed.
	if x.failoverFrom != "" && l.statsAdmin {
		entries = append(entries,
			counterIncr{key: windowKeyForBucket(kindProvider, x.failoverFrom, metricFover, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
			counterIncr{key: windowKeyForBucket(kindProvider, x.failoverFrom, metricFover, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
		)
	}

	// (c) latency (opt-in, admin.stats.latency): the serving provider's
	// own bucket, plus the served model's own bucket when one is known —
	// each hour+day, duration always, TTFB only when the sample carries
	// one (a body that never delivered a successful byte has nothing
	// meaningful to report there).
	if x.hasLat && l.statsLatency {
		if x.provider != "" {
			entries = append(entries, latencyCounterEntries(kindProvider, x.provider, x.lat, wb, et)...)
		}
		if hasModelScope && !x.noModelHistograms {
			entries = append(entries, latencyCounterEntries(kindModel, modelCanonical, x.lat, wb, et)...)
		}
	}

	// (d) cache miss (always-on-with-admin, cache already required to be
	// enabled for cacheMiss to ever be true at all — routes_unified.go
	// only sets it inside its own cacheable branch): total cmiss hour+day,
	// plus the served model's own cmiss at day only (matching every other
	// model-scope cache metric's day-only granularity — recordCacheHit's
	// identical rule for chit/csave).
	if x.cacheMiss && l.statsAdmin {
		entries = append(entries,
			counterIncr{key: windowKeyForBucket(totalScopeKind, totalScopeID, metricCmiss, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
			counterIncr{key: windowKeyForBucket(totalScopeKind, totalScopeID, metricCmiss, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
		)
		if hasModelScope {
			entries = append(entries, counterIncr{key: windowKeyForBucket(kindModel, modelCanonical, metricCmiss, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day})
		}
	}

	if len(entries) == 0 {
		return // every metric was zero for every scope, and no extra was set — nothing to write
	}
	l.storeIncrMulti(entries) // no error return by contract (doc comment above); ok is intentionally discarded
}

// latencyCounterEntries returns the hour+day duration-bucket entries for
// (kind, id) — plus the matching TTFB-bucket entries when lat.hasTTFB —
// accountWith's own opt-in latency block (above) appends for the serving
// provider and, separately, for the served model. latencyBucketIndex
// (metrics.go) already folds "past every configured bound" into its own
// returned index (len(latencyBucketBounds)), so no separate overflow
// branch is needed here.
func latencyCounterEntries(kind, id string, lat latencySample, wb windowBuckets, et enforceTTLSet) []counterIncr {
	durIdx, _ := latencyBucketIndex(lat.duration)
	durMetric := latencyDurationMetric(durIdx)
	out := []counterIncr{
		{key: windowKeyForBucket(kind, id, durMetric, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
		{key: windowKeyForBucket(kind, id, durMetric, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
	}
	if lat.hasTTFB {
		ttfbIdx, _ := latencyBucketIndex(lat.ttfb)
		ttfbMetric := latencyTTFBMetric(ttfbIdx)
		out = append(out,
			counterIncr{key: windowKeyForBucket(kind, id, ttfbMetric, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
			counterIncr{key: windowKeyForBucket(kind, id, ttfbMetric, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
		)
	}
	return out
}

// recordCacheHit accounts one response-cache HIT (Q7, DECISIONS:
// "cache-hit body parse only when cache stats on: accept") — total
// chit/csave at hour and day, plus the served model's own chit/csave at
// day only (matching accountWith's own cmiss day-only rule for a model
// scope). Fire-and-forget (countAsync/l.spawn): a HIT has already written
// its full response to the client before routes_unified.go's own call
// site reaches this, so nothing here can ever delay it. csave entries are
// skipped when savedMicros is 0 (an unpriced or free model), mirroring
// account's own "skip a zero direction" rule.
func (l *limiter) recordCacheHit(canonical string, savedMicros int64) {
	if !l.statsAdmin {
		return
	}
	now := l.now()
	wb := bucketsFor(now)
	et := enforceTTLsFor()
	entries := []counterIncr{
		{key: windowKeyForBucket(totalScopeKind, totalScopeID, metricChit, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
		{key: windowKeyForBucket(totalScopeKind, totalScopeID, metricChit, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
	}
	if savedMicros != 0 {
		entries = append(entries,
			counterIncr{key: windowKeyForBucket(totalScopeKind, totalScopeID, metricCsave, windowHour, wb.hour), delta: savedMicros, ttl: hourWindowTTL, enforceTTL: et.hour},
			counterIncr{key: windowKeyForBucket(totalScopeKind, totalScopeID, metricCsave, windowDay, wb.day), delta: savedMicros, ttl: dayWindowTTL, enforceTTL: et.day},
		)
	}
	if canonical != "" {
		entries = append(entries, counterIncr{key: windowKeyForBucket(kindModel, canonical, metricChit, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day})
		if savedMicros != 0 {
			entries = append(entries, counterIncr{key: windowKeyForBucket(kindModel, canonical, metricCsave, windowDay, wb.day), delta: savedMicros, ttl: dayWindowTTL, enforceTTL: et.day})
		}
	}
	l.countAsync(entries)
}

// recordUnpriced402 accounts one "refused: unpriced model under a cost
// budget" event (Feature F-1, security audit run-1; events.go's
// recordUnpricedRefusalEvent calls this alongside its own gatewayEvent
// record, the one chokepoint both routes_unified.go's and routes_
// passthrough.go's identical 402 guard already share) — total r402
// hour+day, plus the refused model's own r402 hour+day when canonical is
// known. Fire-and-forget (countAsync/l.spawn): the request has already
// been refused with a clean 402 envelope by the time this runs.
func (l *limiter) recordUnpriced402(canonical string) {
	if !l.statsAdmin {
		return
	}
	now := l.now()
	wb := bucketsFor(now)
	et := enforceTTLsFor()
	entries := []counterIncr{
		{key: windowKeyForBucket(totalScopeKind, totalScopeID, metricR402, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
		{key: windowKeyForBucket(totalScopeKind, totalScopeID, metricR402, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
	}
	if canonical != "" {
		entries = append(entries,
			counterIncr{key: windowKeyForBucket(kindModel, canonical, metricR402, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
			counterIncr{key: windowKeyForBucket(kindModel, canonical, metricR402, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
		)
	}
	l.countAsync(entries)
}

// countTargetRequestsBy increments MULTIPLE target scopes' per-target
// request counters (Feature B, v0.21) — each at min, hour, day, AND
// month, the only scope kind this package tracks a month window of
// REQUESTS for — in ONE storeIncrMulti call. kind is targetScopeKind's
// own output ("mcp" or scopeKindAgent — mcp_a2a.go), id is the
// configured target name; both come embedded in windowKey's own key
// string, so a target scope can never collide with a user/group/total
// scope of the same id even by coincidence — kind is part of the key,
// not just a struct field. A scope's own `limits` field is ignored
// entirely (never read); callers pass plain {kind, id} pairs.
//
// This is deliberately NOT folded into checkAndCount: a target scope
// carries no limits by design (this round adds no config surface for
// MCP/agent limits), so there is nothing for checkAndCount to evaluate
// and no violation this call could ever produce — it is pure accounting, exactly
// like accountWith() itself, hence the identical "no error return, ok
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
// checkAndCount/accountWith already apply per scope-slice.
//
// caller identity (target x caller, DECISIONS' "always-on-with-admin"
// family): when caller is non-empty and admin stats are on, ALSO appends
// kindTargetCaller req entries at day+month for every scope, in the SAME
// batch below already builds — never a second round trip. caller == ""
// (countTargetRequests/countTargetRequest, limits_helpers_test.go's
// test-only wrappers — every production call site now names a caller)
// skips this addition entirely.
//
// Callers: handleTargetProxy (mcp_a2a.go, a 1-element slice, once per
// proxied request, after its own user/group/total admission check passes)
// and the federated /mcp endpoint (mcp_federation.go: a 1-element slice for
// tools/call's single resolved target, or one element per server ATTEMPTED
// — not necessarily reached or succeeded — for tools/list's fan-out; see
// mcpFederatedToolsList's own doc comment for why "attempted" is the
// correct word here).
func (l *limiter) countTargetRequestsBy(caller string, scopes []limitScope) {
	if len(scopes) == 0 {
		return
	}
	now := l.now()
	wb := bucketsFor(now)
	et := enforceTTLsFor()
	entries := make([]counterIncr, 0, len(scopes)*6)
	for _, sc := range scopes {
		entries = append(entries,
			counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricReq, windowMin, wb.min), delta: 1, ttl: minWindowTTL, enforceTTL: et.min},
			counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricReq, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
			counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricReq, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
			counterIncr{key: windowKeyForBucket(sc.kind, sc.id, metricReq, windowMonth, wb.month), delta: 1, ttl: monthWindowTTL, enforceTTL: et.month},
		)
		if caller != "" && l.statsAdmin {
			id := targetCallerScopeID(sc.kind, sc.id, caller)
			entries = append(entries,
				counterIncr{key: windowKeyForBucket(kindTargetCaller, id, metricReq, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
				counterIncr{key: windowKeyForBucket(kindTargetCaller, id, metricReq, windowMonth, wb.month), delta: 1, ttl: monthWindowTTL, enforceTTL: et.month},
			)
		}
	}
	l.storeIncrMulti(entries)
}

// countTargetRequestBy is countTargetRequest (limits_helpers_test.go's
// test-only wrapper) with a caller identity attached —
// countTargetRequestsBy's own single-target shape.
func (l *limiter) countTargetRequestBy(caller, kind, id string) {
	l.countTargetRequestsBy(caller, []limitScope{{kind: kind, id: id}})
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
//
// configuredStoreDown is checked in addition to storeGetMulti's own ok
// (finding F5, 2026-09 review, applying currentUsage/providerUsage's own
// fix here too): with the default failOpen=true, storeGetMulti's ok stays
// true during a store outage — it silently reads the in-process fallback
// instead, which never saw this target's real traffic — so ok alone
// cannot tell a real reading apart from a fail-open one served from an
// empty fallback. The output is identical either way (the same
// zero-value targetCounters this doc comment already describes for the
// fail-closed case), so this only changes what happens during a fail-open
// outage, matching the sibling reads' own behavior instead of silently
// serving fabricated zeros as though they were confirmed current usage.
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
	if !ok || len(vals) != len(allKeys) || l.configuredStoreDown() {
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

// kindModel is the limitScope.kind (and admin-API scope-kind) string a
// SERVED model's usage counters use — requests, tokens and cost, the same
// four metrics a user or group scope accumulates, so the Charts view can
// render a model with the very same history endpoint and chart component.
//
// Distinct from kindProviderModel above, and deliberately so: that kind
// counts upstream ATTEMPTS and FAILURES (an availability signal, day
// window only), whereas this one counts delivered usage. A request that
// fails over from one provider to another increments attempt counters on
// both and usage counters only on the one that actually served it.
//
// Its id is the canonical "provider/model" (providerModelScopeID's own
// convention), never the bare model id: two providers can serve the same
// bare id at different prices, and the operator question this feature
// answers ("which model is costing me, and on whose backend") needs the
// serving provider kept in the key. windowKey embeds kind, so a model
// scope can never collide with a user, group, provider or target scope.
const kindModel = "model"

// maxModelScopeIDLen bounds the "provider/model" id withModelScope will
// accept as a counter-scope id. Generous next to any real model id —
// the longest in the built-in table is well under 40 bytes — and far
// below the 4MiB an upstream response body could otherwise put there
// (maxAccountingTeeBytes, routes_passthrough.go). Security audit run-1.
const maxModelScopeIDLen = 256

// withModelScope returns scopes plus a kindModel scope for canonical, the
// "provider/model" id that actually SERVED the request. It copies rather
// than appending in place: the caller's slice is the same one
// admitRequestForRoute already passed to checkAndCount, and growing it through a
// shared backing array would be a data race waiting to happen.
//
// canonical is returned unchanged (no model scope added) when it names no
// model — empty, or a bare "provider/" with nothing after the separator.
// That is the passthrough path's real case: extractPassthroughUsage can
// only report a model id when the upstream's own response body carries
// one, and attributing usage to a "provider/" bucket would invent a model
// that does not exist rather than admit the gap.
func withModelScope(scopes []limitScope, canonical string) []limitScope {
	if canonical == "" || canonical[len(canonical)-1] == '/' {
		return scopes
	}
	// Security audit run-1: on the passthrough route the model half of
	// canonical is read VERBATIM out of the upstream's own response body
	// (extractPassthroughUsage, routes_passthrough.go) with no length or
	// character-set check, and every distinct id it yields mints its own
	// set of counter keys — up to twelve, four of them retained for
	// monthWindowTTL (400 days). An upstream (or, where a group sets no
	// Models restriction, a caller whose requested id an OpenAI-compatible
	// relay echoes back) could therefore grow the counter keyspace without
	// bound. Bounding the id here is the one chokepoint every model-scope
	// write passes through.
	//
	// An id that fails these checks drops the MODEL scope only: the
	// request still accounts to user, group and total exactly as before,
	// which is the same "attribute what we can, invent nothing" rule the
	// empty/trailing-slash case above already follows.
	if len(canonical) > maxModelScopeIDLen {
		return scopes
	}
	for i := 0; i < len(canonical); i++ {
		if canonical[i] < 0x20 || canonical[i] == 0x7f {
			return scopes
		}
	}
	out := make([]limitScope, len(scopes), len(scopes)+1)
	copy(out, scopes)
	return append(out, limitScope{kind: kindModel, id: canonical})
}

// scopesHaveCostBudget reports whether ANY scope in scopes configures a
// money budget — costPerDayUSD or costPerMonthUSD. A scope with nil
// limits (the synthetic total scope, and any user/group that configured
// none) cannot carry one.
//
// Security audit run-1, finding F-1 (high): this is the predicate that
// decides whether serving a model with NO known price would silently
// defeat a control the operator actually configured. It deliberately
// ignores token and request limits — those accrue correctly for an
// unpriced model and keep working; only the cost budget is the one that
// can never fire, because its counter is never written. Matching
// buildBudgetProbes' own `limit <= 0` rule, a zero or negative budget
// means unlimited and so does not count as configured.
func scopesHaveCostBudget(scopes []limitScope) bool {
	for _, sc := range scopes {
		if sc.limits == nil {
			continue
		}
		if usdToMicros(sc.limits.CostPerDayUSD) > 0 || usdToMicros(sc.limits.CostPerMonthUSD) > 0 {
			return true
		}
	}
	return false
}

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
// The PROVIDER scope's counters are written at minute AND day
// granularity; the (provider, model) scope is written at DAY granularity
// only (finding, dead-code sweep, 2026-09 review: this file used to also
// write provmodel:*:attempt|fail:min every attempt, but providerCounterKeys
// below has never read a kindProviderModel scope's minute keys at all —
// SHOULD-2, v0.22 review round, its own doc comment — so those writes were
// 2-4 pure-cost INCRBY+EXPIRE commands per attempt with no reader anywhere
// in this codebase, admin.go, metrics.go, or webui/src; grepped again
// before removing them here). No month window for either scope: provider
// health is a now-and-today question (the Providers tab's success-rate
// badge, webui), not a billing one, so there is nothing here for a
// month-long retention window to serve.
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
	wb := bucketsFor(now)
	et := enforceTTLsFor()
	fail := isTransient(resp, err) || isDeadlineExceeded(err)
	// timeout (always-on-with-admin, below): a strict SUBSET of fail — the
	// gateway's own deadline lapsed, or the watchdog's stalled-progress
	// sentinel fired — reusing matchesSentinel/isDeadlineExceeded exactly
	// like recordUpstreamEvent's own identical classification (events.go),
	// never a third, independently-drifting check.
	timeout := matchesSentinel(err, errProviderTimeout) || isDeadlineExceeded(err)

	entries := make([]counterIncr, 0, 14)
	entries = append(entries,
		counterIncr{key: windowKeyForBucket(kindProvider, provider, metricProvAttempt, windowMin, wb.min), delta: 1, ttl: minWindowTTL, enforceTTL: et.min},
		counterIncr{key: windowKeyForBucket(kindProvider, provider, metricProvAttempt, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
	)
	if fail {
		entries = append(entries,
			counterIncr{key: windowKeyForBucket(kindProvider, provider, metricProvFail, windowMin, wb.min), delta: 1, ttl: minWindowTTL, enforceTTL: et.min},
			counterIncr{key: windowKeyForBucket(kindProvider, provider, metricProvFail, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
		)
	}
	if model != "" {
		// Day granularity only — see this function's own doc comment
		// (dead-code sweep, 2026-09 review) for why a minute-window pair
		// was removed here: providerCounterKeys never reads one for a
		// kindProviderModel scope.
		id := providerModelScopeID(provider, model)
		entries = append(entries,
			counterIncr{key: windowKeyForBucket(kindProviderModel, id, metricProvAttempt, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
		)
		if fail {
			entries = append(entries,
				counterIncr{key: windowKeyForBucket(kindProviderModel, id, metricProvFail, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
			)
		}
	}

	// Always-on-with-admin (DECISIONS): hourly attempt/fail for prov and
	// provmodel, plus prov's own timeout hour+day — zero cost when admin
	// is disabled, riding this SAME async batch otherwise, never a second
	// round trip.
	if l.statsAdmin {
		entries = append(entries,
			counterIncr{key: windowKeyForBucket(kindProvider, provider, metricProvAttempt, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
		)
		if fail {
			entries = append(entries,
				counterIncr{key: windowKeyForBucket(kindProvider, provider, metricProvFail, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
			)
		}
		if model != "" {
			id := providerModelScopeID(provider, model)
			entries = append(entries,
				counterIncr{key: windowKeyForBucket(kindProviderModel, id, metricProvAttempt, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
			)
			if fail {
				entries = append(entries,
					counterIncr{key: windowKeyForBucket(kindProviderModel, id, metricProvFail, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
				)
			}
		}
		if timeout {
			entries = append(entries,
				counterIncr{key: windowKeyForBucket(kindProvider, provider, metricProvTimeout, windowHour, wb.hour), delta: 1, ttl: hourWindowTTL, enforceTTL: et.hour},
				counterIncr{key: windowKeyForBucket(kindProvider, provider, metricProvTimeout, windowDay, wb.day), delta: 1, ttl: dayWindowTTL, enforceTTL: et.day},
			)
		}
	}
	l.countAsync(entries)
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
	// storeDown reports whether reading this scope's counters failed
	// closed (a configured store errored and failOpen is false) —
	// mirrors scopeUsage.storeDown exactly (review fix: the doc comment
	// below used to claim "nothing downstream treats a provider's zero
	// counters as confirmed no-traffic", which metrics.go's renderer
	// made false the moment it existed — a Redis blip made every
	// provider counter read as 0, then jump back to its real value on
	// the next successful scrape, which Prometheus reads as a counter
	// RESET followed by a fresh increase equal to the whole prior total:
	// one transient store error paints a phantom multi-thousand-request
	// spike. Every field is 0 when this is true; a caller must not
	// present them as "confirmed zero usage", the identical rule
	// scopeUsage's own doc comment states).
	storeDown bool
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
// (store down + fail-closed) returns every counter at its zero value
// WITH storeDown set (review fix: adversarial verification, 2026-08-23
// — the previous version of this comment claimed nothing downstream
// treats a provider's zero counters as confirmed no-traffic; metrics.go
// is exactly such a downstream, and without this flag a transient Redis
// blip fabricated a hard 0 that Prometheus reads as a counter reset,
// followed by a fresh "increase" equal to the whole prior total on the
// next successful scrape). A caller reading providerCounters must skip
// a storeDown entry the same way currentUsage's own callers already skip
// a storeDown scopeUsage — see that type's doc comment.
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
	// configuredStoreDown catches the fail-open case ok alone misses
	// (review fix, adversarial verification 2026-08-23) — see
	// currentUsage's identical check and its own doc comment for why.
	if !ok || len(vals) != len(allKeys) || l.configuredStoreDown() {
		for i := range out {
			out[i] = providerCounters{storeDown: true}
		}
		return out
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
// the moment a silent off-by-N here would have gone unnoticed. F4 (v0.3
// dashboard task) grows it again, 8 to 9, for the fleet-wide
// rejectionsPerDay counter (metricRej) usageWindowKeys now appends. Grows
// once more, 9 to 10, for the last-seen counter (Q6, DECISIONS) — see
// usageWindowKeys' own doc comment for the appended field's position.
const usageKeysPerScope = 10

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
	// rejectionsPerDay is the fleet-wide count of checkAndCount rejections
	// attributed to this scope today (metricRej, F4 v0.3 dashboard task) —
	// GET /admin/api/usage's rejectionsPerDay field. Unlike every other
	// field here, this counts REFUSED requests, not admitted ones.
	rejectionsPerDay int64
	// lastSeen is the unix-second value THIS scope's last-seen SET last
	// wrote (Q6, DECISIONS) — 0 when never seen, or when admin stats were
	// off for the whole life of the deployment so far, matching GET
	// /admin/api/usage's own "lastSeen 0 means never/unknown" contract.
	lastSeen int64
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
// cost/month, rej/day — matching scopeUsage's field order exactly, so
// currentUsage can map storeGetMulti's result slice back to named fields
// by plain index. rej/day (F4, v0.3 dashboard task) is appended after
// every original index; lastSeenKey (Q6, DECISIONS) is appended last of
// all — every existing index above it stays byte-for-byte unchanged. A
// last-seen key never rolls over (lastSeenKey has no window-bucket
// component), so it needs no `now` argument the way every other key here
// does; passed through only so this stays a single flat literal.
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
		windowKey(sc.kind, sc.id, metricRej, windowDay, now),
		lastSeenKey(sc.kind, sc.id),
	}
}

// currentUsage reads every scope's usageKeysPerScope current-window
// counters — the same (kind, id, metric, window) combinations
// checkAndCount/account already write — via the limiter's own
// storeGetMulti, so it applies the identical fail-open/fail-closed policy
// every enforcement read already does, and it is read-only: unlike
// checkAndCount, it never increments anything.
//
// storeDown is also set when configuredStoreDown reports the store
// currently latched (review fix, adversarial verification 2026-08-23):
// with failOpen true (the default), storeGetMulti's own ok stays true
// on a store outage — it silently reads the in-process fallback instead
// — so ok alone is not enough to tell a real reading apart from a
// fail-open one served from an empty fallback. Checked in addition to,
// never instead of, storeGetMulti's own ok/length checks below.
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
	if !ok || len(vals) != len(allKeys) || l.configuredStoreDown() {
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
			rejectionsPerDay: v[8], lastSeen: v[9],
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

// modelSpanTotals/modelTotals/modelSpanTotalsMulti (limits_helpers_test.go,
// admin-redesign WP-G cleanup) read span counter buckets per id — via
// historyBucketKeys/historyBucketKeysAt, the identical per-id bucket walk
// history (below) uses for one scope — flattening every id's span keys
// into ONE storeGetMulti round trip. GET /admin/api/usage/models' actual
// production read path is admin.go's chunkedModelSpanTotals/
// chunkedModelSpanTotalsMulti, which read directly via
// historyBucketKeysAt+storeGetMulti in bounded-size chunks instead of
// delegating here (admin.go's own doc comment on chunkedModelSpanTotals
// explains why: a single limiter.modelSpanTotalsMulti call has no chunk
// boundary, so a wide model catalog could build one storeGetMulti larger
// than any real Redis pipeline should be). These three stay as test-only
// helpers because limits_test.go's own coverage of historyBucketKeys/
// historyBucketKeysAt's span and offset semantics is written against
// them.

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

// historyBucketKeysAt is historyBucketKeys generalized with an offset
// (WP-B's own comparison-mode reads: GET /admin/api/usage/models and
// /usage/series' ?offset= parameter): the returned span ends offset window
// units before now instead of at now itself, letting a caller read an
// EARLIER span of the identical width for a period-over-period
// comparison, without duplicating historyStepBack's own calendar-aware
// month arithmetic a second time. offset=0 is byte-for-byte
// historyBucketKeys' own pre-existing behavior — composing historyStepBack
// twice (once for offset, once per bucket within the span) is
// associative with calling it once for their sum, since historyStepBack's
// own month case always anchors on the 1st of whatever month it is
// applied to, and its result here is always itself a 1st-of-month date.
func historyBucketKeysAt(kind, id, metric, window string, now time.Time, span, offset int) (keys, buckets []string) {
	base := now
	if offset > 0 {
		base = historyStepBack(now, window, offset)
	}
	keys = make([]string, span)
	buckets = make([]string, span)
	for i := 0; i < span; i++ {
		t := historyStepBack(base, window, span-1-i)
		keys[i] = windowKey(kind, id, metric, window, t)
		buckets[i] = bucketFor(t, window)
	}
	return keys, buckets
}

// historyBucketKeys returns the span counterStore keys and their matching
// bucket label strings for (kind, id, metric, window), stepping back from
// now one whole window unit at a time — oldest first, the current
// (possibly partial) bucket last, matching GET /admin/api/usage/history's
// "inclusive of current bucket, oldest-first" contract. A thin offset=0
// wrapper around historyBucketKeysAt, above — every existing caller of
// this name keeps calling it, and its own behavior is unchanged.
func historyBucketKeys(kind, id, metric, window string, now time.Time, span int) (keys, buckets []string) {
	return historyBucketKeysAt(kind, id, metric, window, now, span, 0)
}

// chunkedGetMulti reads keys via one or more storeGetMulti round trips,
// each capped at chunkSize keys, and returns the values concatenated back
// in the SAME order as keys (P6 fix, admin dashboard redesign verify
// round: GET /admin/api/usage/series and spanTotalsMulti's own predecessors
// each used to build ONE unbounded storeGetMulti pipeline regardless of
// how many keys the request's own scope/span/offset/metric combination
// worked out to — holding the shared Redis connection mutex, respClient
// (resp.go), for however long that single oversized pipeline took, while
// live traffic's own checkAndCount/account calls queued behind it).
// Chunking a flat key list this way is safe for a caller that does not
// need chunk boundaries aligned to any larger unit (e.g. one id's own
// span): values come back positionally identical to an unchunked read,
// this just bounds how many keys any ONE round trip carries. Any chunk
// that fails (storeDown or a mismatched read) fails the whole call — a
// series silently missing an arbitrary subset of its own buckets is worse
// than a 503, the same "no partial answer" rule every admin stats reader
// in this package already follows.
func (l *limiter) chunkedGetMulti(keys []string, chunkSize int) ([]int64, bool) {
	if chunkSize < 1 {
		chunkSize = len(keys)
	}
	out := make([]int64, 0, len(keys))
	for len(keys) > 0 {
		n := chunkSize
		if n > len(keys) {
			n = len(keys)
		}
		vals, ok := l.storeGetMulti(keys[:n])
		if !ok || len(vals) != n || l.configuredStoreDown() {
			return nil, false
		}
		out = append(out, vals...)
		keys = keys[n:]
	}
	return out, true
}

// spanTotalsMulti is the ONE production "per id, per metric, sum over a
// span of window buckets" reader every admin stats endpoint that needs a
// span TOTAL (as opposed to GET /admin/api/usage/series' own per-bucket
// values, which read via chunkedGetMulti directly instead) now shares
// (P12 fix, admin dashboard redesign verify round: this exact shape used
// to exist three times over — stats_read.go's own readSpanTotals, admin.go's
// chunkedModelSpanTotals/chunkedModelSpanTotalsMulti hardcoded to
// kind=kindModel, and this file's own modelSpanTotalsMulti, which drifted
// to a test-only helper (limits_helpers_test.go) once WP-B's rewrite
// stopped calling it from production, so limits_test.go's own coverage of
// the offset/chunking semantics no longer exercised what actually ran in
// production). ids may name any kind's scopes; kind is fixed per call the
// same way historyBucketKeysAt itself takes one kind per call.
//
// Reads are chunked at usageModelsChunkKeys (admin.go) keys per
// storeGetMulti round trip, sized so each chunk's own key count
// (len(chunk)*len(metrics)*span) stays at or under that bound — mirroring
// chunkedModelSpanTotalsMulti's own pre-existing chunking math, generalized
// past kindModel. now is resolved once by the caller and threaded through
// every chunk, so every id's span ends at the identical instant regardless
// of how many chunks ids splits into. Any chunk that fails fails the whole
// call, for the identical "no partial ranking" reason chunkedGetMulti's own
// doc comment gives.
//
// An empty ids or metrics slice (or span < 1) returns immediately with
// each metric's own nil slice and ok=true — no store read at all, matching
// every span-total reader's pre-existing "empty input never touches the
// store" contract (TestModelSpanTotals_EmptyIDs_NoStoreRead pins this
// against an always-erroring store).
func (l *limiter) spanTotalsMulti(kind string, ids, metrics []string, window string, now time.Time, span, offset int) ([][]int64, bool) {
	out := make([][]int64, len(metrics))
	if len(ids) == 0 || len(metrics) == 0 || span < 1 {
		return out, true
	}
	perChunk := usageModelsChunkKeys / (span * len(metrics))
	if perChunk < 1 {
		perChunk = 1
	}
	for m := range out {
		out[m] = make([]int64, 0, len(ids))
	}
	remaining := ids
	for len(remaining) > 0 {
		n := perChunk
		if n > len(remaining) {
			n = len(remaining)
		}
		chunk := remaining[:n]
		allKeys := make([]string, 0, len(chunk)*len(metrics)*span)
		for _, id := range chunk {
			for _, metric := range metrics {
				keys, _ := historyBucketKeysAt(kind, id, metric, window, now, span, offset)
				allKeys = append(allKeys, keys...)
			}
		}
		vals, ok := l.chunkedGetMulti(allKeys, usageModelsChunkKeys)
		if !ok || len(vals) != len(allKeys) {
			return nil, false
		}
		stride := len(metrics) * span
		for i := range chunk {
			base := i * stride
			for m := range metrics {
				mBase := base + m*span
				var sum int64
				for _, v := range vals[mBase : mBase+span] {
					sum += v
				}
				out[m] = append(out[m], sum)
			}
		}
		remaining = remaining[n:]
	}
	return out, true
}

// history returns span counter values for (kind, id, metric, window)
// ending at now, oldest-first — GET /admin/api/usage/history's data
// source (v0.2 data-layer task). It computes every bucket key up front
// and reads them in ONE storeGetMulti round trip, the same single-batch
// discipline currentUsage already applies to a scope's current-window
// keys, scaled here to a whole span of one metric/window instead of a
// fixed set of usageKeysPerScope. ok is false only in the fail-closed
// case (storeGetMulti's own contract) OR when configuredStoreDown reports
// a fail-open outage in progress (finding F5, 2026-09 review, applying
// currentUsage/providerUsage's own fix here too — with the default
// failOpen=true, storeGetMulti's ok alone stays true while silently
// reading the in-process fallback, which a Redis blip empties out to
// near-zero, painting a false drop on the usage-history charts instead of
// the 503 this doc comment already promises for a store failure): a
// caller must not present the returned points as real data then.
func (l *limiter) history(kind, id, metric, window string, now time.Time, span int) ([]historyPoint, bool) {
	keys, buckets := historyBucketKeys(kind, id, metric, window, now, span)
	vals, ok := l.storeGetMulti(keys)
	if !ok || len(vals) != len(keys) || l.configuredStoreDown() {
		return nil, false
	}
	points := make([]historyPoint, span)
	for i := range points {
		points[i] = historyPoint{bucket: buckets[i], value: vals[i]}
	}
	return points, true
}
