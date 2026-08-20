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

// sweepEvery is the minimum real time between incrBy's opportunistic
// expired-entry sweeps, regardless of how many keys memoryStore holds. A
// size-gated sweep (only above N keys) was measured to scan the whole map
// on every incrBy once live keys exceeded that threshold, since a sweep
// that frees nothing (all keys still live) never brings the count back
// down — a 167x incrBy cliff. Gating on elapsed time instead bounds sweep
// frequency independent of key count: worst case is one full-map scan
// every 30s.
const sweepEvery = 30 * time.Second

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

// incrBy implements counterStore.
func (m *memoryStore) incrBy(key string, n int64, ttl time.Duration) (int64, error) {
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
	return e.value, nil
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
	store            counterStore                     // configured backend; nil means always use fallback (see newLimiter)
	fallback         *memoryStore                     // in-process counter store, always available
	nowFn            func() time.Time                 // injected for tests; defaults to time.Now
	logf             func(format string, args ...any) // injectable store-error log; defaults to a no-op
	lastLogAt        time.Time                        // guarded by logMu; last time a store error was logged
	lastStoreFailure time.Time                        // guarded by logMu; zero means the store-down latch is not open (see storeLatched)
	logMu            sync.Mutex
	failOpen         bool // store-error policy: true falls back to fallback, false refuses the request
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
	l.logMu.Unlock()
}

// storeIncrBy increments key by n with the given ttl, applying the
// limiter's fail-open/fail-closed policy when a configured store errors
// or when the store-down latch (storeLatched) is already open from a
// recent failure. ok is false only in the fail-closed case — a caller
// must refuse the request for that, rather than treating a zero value as
// a real counter reading.
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

// storeDownViolation is the violation checkAndCount/budgetViolation return
// when the configured store is unreachable and failOpen is false: the
// request is refused instead of silently enforcing limits against a
// non-shared fallback, so a backend outage cannot let every configured
// limit go unenforced across a fleet of gateway instances.
func storeDownViolation() *limitViolation {
	return &limitViolation{message: "limit store unavailable", storeDown: true}
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
// and returns its new value, together with whether the operation
// succeeded under the limiter's fail-open/fail-closed policy (see
// storeIncrBy). ok is false only in the fail-closed case: a configured
// store errored and failOpen is false.
func (l *limiter) incrCounter(kind, id, metric, window string, t time.Time, n int64, ttl time.Duration) (int64, bool) {
	return l.storeIncrBy(windowKey(kind, id, metric, window, t), n, ttl)
}

// getCounter returns the (kind, id, metric, window) counter's value at t,
// together with whether the read succeeded under the limiter's fail-open/
// fail-closed policy (see storeGet).
func (l *limiter) getCounter(kind, id, metric, window string, t time.Time) (int64, bool) {
	return l.storeGet(windowKey(kind, id, metric, window, t))
}

// checkAndCount increments every scope's req:min and req:day counters —
// unconditionally, before any evaluation, so a request that ultimately
// gets refused by one scope's limit still counts toward every other
// scope's rate tracking — then evaluates each scope's set limits in order
// and returns the first violation found, or nil if the request may
// proceed. If any store operation fails closed (a configured store errored
// and failOpen is false), it returns a storeDownViolation immediately —
// the request is refused rather than evaluated against partial or
// fallback-only counters.
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
		v, ok := l.incrCounter(sc.kind, sc.id, metricReq, windowMin, now, 1, minWindowTTL)
		if !ok {
			return storeDownViolation()
		}
		minCounts[i] = v

		v, ok = l.incrCounter(sc.kind, sc.id, metricReq, windowDay, now, 1, dayWindowTTL)
		if !ok {
			return storeDownViolation()
		}
		dayCounts[i] = v
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

// account records u's total tokens and costMicros against every scope's
// day and month counters. A zero-valued metric is skipped entirely — no
// store write for a metric this call has nothing to report. A sample that
// hits a fail-closed store error (see storeIncrBy) is dropped silently:
// account has no error return to signal it, and dropping — rather than
// counting it in the fallback — avoids double-counting once the store
// recovers.
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
