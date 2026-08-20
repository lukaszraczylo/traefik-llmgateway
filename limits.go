package traefikllmgateway

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// Window identifiers used throughout windowKey, counter metrics, and
// retry-after calculation.
const (
	windowMin   = "min"
	windowDay   = "day"
	windowMonth = "month"
)

// Counter metric names embedded in windowKey.
const (
	metricReq  = "req"
	metricTok  = "tok"
	metricCost = "cost"
)

// TTLs applied to counter keys. They exceed their window's natural length
// so a key stays readable for the whole window it belongs to; the bucket
// embedded in the key (see windowKey) already makes a rolled-over window
// use a different key, so these TTLs only bound how long a stale key
// lingers in memoryStore before an opportunistic sweep reclaims it.
const (
	minWindowTTL   = 2 * time.Minute
	dayWindowTTL   = 25 * time.Hour
	monthWindowTTL = 32 * 24 * time.Hour
)

// usdToMicroFactor scales a USD amount to micro-USD (1e6 micro-USD per
// USD), matching the integer accounting unit used by account and the
// counterStore.
const usdToMicroFactor = 1_000_000

// counterStore is the storage backend the limiter uses for atomic windowed
// counters. memoryStore (below) is the in-process fallback; a distributed
// (e.g. Redis-backed) implementation is wired in a later task.
type counterStore interface {
	// incrBy adds n to key's counter, creating it with an expiry of ttl
	// from now if it does not exist or has expired, and returns the
	// counter's new value.
	incrBy(key string, n int64, ttl time.Duration) (int64, error)
	// get returns key's current counter value, or 0 if it does not exist
	// or has expired.
	get(key string) (int64, error)
}

// memoryEntry is one counter's value and expiry in memoryStore.
type memoryEntry struct {
	expiry time.Time
	value  int64
}

// memoryStoreSweepThreshold is the key count above which incrBy
// opportunistically deletes expired entries before writing. Kept well
// above typical live-scope counts so the sweep is rare on a healthy
// gateway and only fires when stale keys are actually accumulating.
const memoryStoreSweepThreshold = 1024

// memoryStore is an in-process counterStore: a mutex-guarded map with
// per-key expiry. It has no background goroutine — a Yaegi middleware
// instance is rebuilt on every config reload, and any ticker or goroutine
// started here would leak on rebuild instead of being collected with the
// rest of the old instance. Expired entries are instead swept
// opportunistically from incrBy once the map grows past
// memoryStoreSweepThreshold keys.
type memoryStore struct {
	data map[string]*memoryEntry
	mu   sync.Mutex
}

// newMemoryStore returns an empty memoryStore.
func newMemoryStore() *memoryStore {
	return &memoryStore{data: make(map[string]*memoryEntry)}
}

// incrBy implements counterStore.
func (m *memoryStore) incrBy(key string, n int64, ttl time.Duration) (int64, error) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.data) > memoryStoreSweepThreshold {
		m.sweepLocked(now)
	}

	e, ok := m.data[key]
	if !ok || !now.Before(e.expiry) {
		e = &memoryEntry{expiry: now.Add(ttl)}
		m.data[key] = e
	}
	e.value += n
	return e.value, nil
}

// get implements counterStore.
func (m *memoryStore) get(key string) (int64, error) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.data[key]
	if !ok || !now.Before(e.expiry) {
		return 0, nil
	}
	return e.value, nil
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
	// estimated is set by a later task's streaming/estimation path; this
	// task only produces the field for that interface, never reads it.
	estimated bool //nolint:unused // interface field for a later task, see comment above
}

// total returns the request's combined prompt and completion tokens.
func (u usage) total() int64 {
	return u.prompt + u.completion
}

// limitScope is one entity (a user or their group) whose limits apply to a
// request. checkAndCount and account evaluate every scope in the slice
// they are given, so a request is counted and checked against both its
// user's and its group's limits in one call.
type limitScope struct {
	limits *LimitsConfig
	kind   string // "user" or "group"
	id     string
}

// limitViolation describes the first limit a request breached.
type limitViolation struct {
	message string
	// retryAfter is the number of seconds until the violated window ends
	// — until the client can plausibly succeed again.
	retryAfter int
	// storeDown reports whether the violation was manufactured because the
	// configured counterStore was unreachable, rather than an actual limit
	// breach. Always false in this task; a later task wires failOpen /
	// fail-closed handling for a real remote store.
	storeDown bool
}

// limiter enforces per-minute/day/month request, token, and cost limits
// using fixed windows keyed by windowKey.
type limiter struct {
	store    counterStore     // nil means always use fallback (see newLimiter)
	fallback *memoryStore     // in-process counter store, always available
	nowFn    func() time.Time // injected for tests; defaults to time.Now
	failOpen bool             // wired for a later task's store-down handling
}

// newLimiter returns a limiter. A nil store means every operation uses the
// limiter's own in-process fallback memoryStore — the plugin still
// enforces limits with no distributed backend configured, just without
// sharing counters across Traefik instances.
func newLimiter(store counterStore, failOpen bool) *limiter {
	return &limiter{
		store:    store,
		fallback: newMemoryStore(),
		nowFn:    time.Now,
		failOpen: failOpen,
	}
}

// now returns the limiter's current time, via nowFn.
func (l *limiter) now() time.Time {
	return l.nowFn()
}

// activeStore returns the store operations should use: the configured
// store when set, otherwise the in-process fallback.
func (l *limiter) activeStore() counterStore {
	if l.store != nil {
		return l.store
	}
	return l.fallback
}

// windowKey builds the counterStore key for one (kind, id, metric, window)
// counter at time t: llmgw:{kind}:{id}:{metric}:{window}:{bucket}. bucket
// is t.UTC() formatted to the window's granularity, so a counter's key
// changes automatically when its window rolls over.
func windowKey(kind, id, metric, window string, t time.Time) string {
	u := t.UTC()
	var bucket string
	switch window {
	case windowMin:
		bucket = u.Format("200601021504")
	case windowDay:
		bucket = u.Format("20060102")
	case windowMonth:
		bucket = u.Format("200601")
	default:
		// window is always one of the three constants above, set by this
		// file's own callers, never by request input — an unknown value
		// here is a programming error. ServeHTTP's recoverPanic turns
		// this into a logged 500 instead of crashing the process.
		panic(fmt.Sprintf("llmgateway: windowKey: unknown window %q", window))
	}
	return fmt.Sprintf("llmgw:%s:%s:%s:%s:%s", kind, id, metric, window, bucket)
}

// windowEnd returns the UTC instant at which window's bucket containing t
// ends: the next minute boundary, the next UTC midnight, or the first of
// next month UTC.
func windowEnd(t time.Time, window string) time.Time {
	u := t.UTC()
	switch window {
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
// and returns its new value. A store error is treated as "no increment
// observed" — the fallback memoryStore never errors, and a real store's
// fail-open/fail-closed policy is wired in a later task.
func (l *limiter) incrCounter(kind, id, metric, window string, t time.Time, n int64, ttl time.Duration) int64 {
	v, err := l.activeStore().incrBy(windowKey(kind, id, metric, window, t), n, ttl)
	if err != nil {
		return 0
	}
	return v
}

// getCounter returns the (kind, id, metric, window) counter's value at t.
func (l *limiter) getCounter(kind, id, metric, window string, t time.Time) (int64, error) {
	return l.activeStore().get(windowKey(kind, id, metric, window, t))
}

// checkAndCount increments every scope's req:min and req:day counters —
// unconditionally, before any evaluation, so a request that ultimately
// gets refused by one scope's limit still counts toward every other
// scope's rate tracking — then evaluates each scope's set limits in order
// and returns the first violation found, or nil if the request may
// proceed.
//
// Requests limits compare against the value just incremented in this call.
// Token and cost limits compare against the value already accumulated by
// account (via get): a request that would start over budget is refused,
// but because usage is only known after the upstream call completes, one
// request may still push a scope over its budget — an accepted trade-off.
func (l *limiter) checkAndCount(scopes []limitScope) *limitViolation {
	now := l.now()

	minCounts := make([]int64, len(scopes))
	dayCounts := make([]int64, len(scopes))
	for i, sc := range scopes {
		minCounts[i] = l.incrCounter(sc.kind, sc.id, metricReq, windowMin, now, 1, minWindowTTL)
		dayCounts[i] = l.incrCounter(sc.kind, sc.id, metricReq, windowDay, now, 1, dayWindowTTL)
	}

	for i, sc := range scopes {
		if v := l.evaluateScope(sc, minCounts[i], dayCounts[i], now); v != nil {
			return v
		}
	}
	return nil
}

// evaluateScope checks one scope's already-set limits against its
// just-incremented request counts and its accumulated token/cost counts.
// A nil limits field means nothing to evaluate — the scope's req counters
// still incremented above, feeding other scopes' aggregates and future
// observability, but this scope itself never blocks a request.
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
	if v := l.budgetViolation(sc, metricTok, "tokens-per-day", lim.TokensPerDay, windowDay, now); v != nil {
		return v
	}
	if v := l.budgetViolation(sc, metricTok, "tokens-per-month", lim.TokensPerMonth, windowMonth, now); v != nil {
		return v
	}
	if v := l.budgetViolation(sc, metricCost, "cost-per-day", int64(lim.CostPerDayUSD*usdToMicroFactor), windowDay, now); v != nil {
		return v
	}
	if v := l.budgetViolation(sc, metricCost, "cost-per-month", int64(lim.CostPerMonthUSD*usdToMicroFactor), windowMonth, now); v != nil {
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
// unlimited. A store read error fails open — no violation reported —
// consistent with this task's storeDown always being false; a later task
// wires strict fail-closed handling for a real remote store.
func (l *limiter) budgetViolation(sc limitScope, metric, name string, limit int64, window string, now time.Time) *limitViolation {
	if limit <= 0 {
		return nil
	}
	used, err := l.getCounter(sc.kind, sc.id, metric, window, now)
	if err != nil || used < limit {
		return nil
	}
	return &limitViolation{
		message:    fmt.Sprintf("%s %q exceeded %s budget", sc.kind, sc.id, name),
		retryAfter: retryAfterSeconds(now, window),
	}
}

// account records u's total tokens and costMicros against every scope's
// day and month counters. A zero-valued metric is skipped entirely — no
// store write for a metric this call has nothing to report.
func (l *limiter) account(scopes []limitScope, u usage, costMicros int64) {
	now := l.now()
	total := u.total()

	for _, sc := range scopes {
		if total != 0 {
			l.incrCounter(sc.kind, sc.id, metricTok, windowDay, now, total, dayWindowTTL)
			l.incrCounter(sc.kind, sc.id, metricTok, windowMonth, now, total, monthWindowTTL)
		}
		if costMicros != 0 {
			l.incrCounter(sc.kind, sc.id, metricCost, windowDay, now, costMicros, dayWindowTTL)
			l.incrCounter(sc.kind, sc.id, metricCost, windowMonth, now, costMicros, monthWindowTTL)
		}
	}
}
