package traefikllmgateway

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLimiterRequestWindow is the brief's Step-1 case: a per-minute
// request limit blocks the third request in a window and resets on the
// next window.
func TestLimiterRequestWindow(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	l := newLimiter(nil, true) // nil store → uses fallback memoryStore
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "a", limits: &LimitsConfig{RequestsPerMinute: 2}}}
	if v := l.checkAndCount(scopes); v != nil {
		t.Fatal("1st should pass")
	}
	if v := l.checkAndCount(scopes); v != nil {
		t.Fatal("2nd should pass")
	}
	if v := l.checkAndCount(scopes); v == nil {
		t.Fatal("3rd should violate")
	}
	now = now.Add(time.Minute) // next window
	if v := l.checkAndCount(scopes); v != nil {
		t.Fatal("new window should pass")
	}
}

// TestLimiterTokenBudget is the brief's Step-1 case: accounting tokens
// past a day budget makes the next check refuse.
func TestLimiterTokenBudget(t *testing.T) {
	l := newLimiter(nil, true)
	scopes := []limitScope{{kind: "group", id: "g", limits: &LimitsConfig{TokensPerDay: 100}}}
	l.account(scopes, usage{prompt: 60, completion: 50}, 0) // 110 > 100
	if v := l.checkAndCount(scopes); v == nil {
		t.Fatal("over token budget must refuse")
	}
}

// TestLimiterCostBudget mirrors TestLimiterTokenBudget for a cost budget:
// accounting micro-USD cost past a day budget makes the next check refuse,
// and the violation carries the day window's Retry-After.
func TestLimiterCostBudget(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	l := newLimiter(nil, true)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{CostPerDayUSD: 1.00}}}

	l.account(scopes, usage{}, 1_500_000) // $1.50 > $1.00 budget
	v := l.checkAndCount(scopes)
	if v == nil {
		t.Fatal("over cost budget must refuse")
	}
	wantRetry := int(time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC).Sub(now).Seconds())
	if v.retryAfter != wantRetry {
		t.Errorf("retryAfter = %d, want %d (seconds to next UTC midnight)", v.retryAfter, wantRetry)
	}
	if v.storeDown {
		t.Error("storeDown must be false in this task")
	}
}

// TestLimiterNilLimitsUnlimited asserts a scope with limits == nil never
// blocks a request, however many times it is checked — request counters
// still climb unboundedly but nothing evaluates them.
func TestLimiterNilLimitsUnlimited(t *testing.T) {
	l := newLimiter(nil, true)
	scopes := []limitScope{{kind: "user", id: "unlimited", limits: nil}}
	for i := 0; i < 1000; i++ {
		if v := l.checkAndCount(scopes); v != nil {
			t.Fatalf("iteration %d: nil limits must never violate, got %+v", i, v)
		}
	}
}

// TestLimiterUserOverridesAlongsideGroup asserts checkAndCount evaluates
// every scope it is given in one call: a tight user limit blocks the
// request even though the group scope in the same call is unlimited, and
// an unlimited user scope still lets a tight group limit block.
func TestLimiterUserOverridesAlongsideGroup(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)

	t.Run("tight user limit blocks despite unlimited group", func(t *testing.T) {
		l := newLimiter(nil, true)
		l.nowFn = func() time.Time { return now }
		scopes := []limitScope{
			{kind: "user", id: "u1", limits: &LimitsConfig{RequestsPerMinute: 1}},
			{kind: "group", id: "g1", limits: nil},
		}
		if v := l.checkAndCount(scopes); v != nil {
			t.Fatal("1st request should pass")
		}
		v := l.checkAndCount(scopes)
		if v == nil {
			t.Fatal("2nd request should violate the user's tight limit")
		}
	})

	t.Run("tight group limit blocks despite unlimited user", func(t *testing.T) {
		l := newLimiter(nil, true)
		l.nowFn = func() time.Time { return now }
		scopes := []limitScope{
			{kind: "user", id: "u2", limits: nil},
			{kind: "group", id: "g2", limits: &LimitsConfig{RequestsPerMinute: 1}},
		}
		if v := l.checkAndCount(scopes); v != nil {
			t.Fatal("1st request should pass")
		}
		v := l.checkAndCount(scopes)
		if v == nil {
			t.Fatal("2nd request should violate the group's tight limit")
		}
	})
}

// TestLimiterRetryAfter checks the Retry-After computation directly for
// each window kind: seconds to the next minute boundary, next UTC
// midnight, and the first of next month UTC.
func TestLimiterRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		now    time.Time
		window string
		want   int
	}{
		{"minute boundary", time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC), windowMin, 30},
		{"day boundary", time.Date(2026, 8, 20, 23, 59, 0, 0, time.UTC), windowDay, 60},
		{"month boundary, 31-day month", time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC), windowMonth, 1},
		{"month boundary, mid-month", time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC), windowMonth, int(14 * 24 * time.Hour / time.Second)}, // Feb 2026 (not a leap year) runs Feb1-Mar1 = 28 days; Feb15 is 14 days before Mar1
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retryAfterSeconds(c.now, c.window); got != c.want {
				t.Errorf("retryAfterSeconds(%v, %q) = %d, want %d", c.now, c.window, got, c.want)
			}
		})
	}
}

// TestLimiterRequestLimitRetryAfter asserts a request-limit violation
// (not a budget violation) also carries the correct Retry-After for its
// window.
func TestLimiterRequestLimitRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	l := newLimiter(nil, true)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "a", limits: &LimitsConfig{RequestsPerMinute: 1}}}

	if v := l.checkAndCount(scopes); v != nil {
		t.Fatal("1st should pass")
	}
	v := l.checkAndCount(scopes)
	if v == nil {
		t.Fatal("2nd should violate")
	}
	if v.retryAfter != 30 {
		t.Errorf("retryAfter = %d, want 30", v.retryAfter)
	}
}

// TestWindowKeyFormat pins the exact key layout every store operation
// depends on: llmgw:{kind}:{id}:{metric}:{window}:{bucket}, with bucket
// formatted from t.UTC() at each window's granularity.
func TestWindowKeyFormat(t *testing.T) {
	tm := time.Date(2026, 8, 20, 10, 4, 59, 0, time.FixedZone("UTC+1", 3600)) // 09:04:59 UTC
	cases := []struct {
		window string
		want   string
	}{
		{windowMin, "llmgw:user:a:req:min:202608200904"},
		{windowDay, "llmgw:user:a:req:day:20260820"},
		{windowMonth, "llmgw:user:a:req:month:202608"},
	}
	for _, c := range cases {
		if got := windowKey("user", "a", "req", c.window, tm); got != c.want {
			t.Errorf("windowKey(..., %q, ...) = %q, want %q", c.window, got, c.want)
		}
	}
}

// TestMemoryStoreIncrAndGet exercises the fallback store directly: fresh
// keys start at n, repeated incrBy accumulates, and get on an unknown key
// returns 0 with no error.
func TestMemoryStoreIncrAndGet(t *testing.T) {
	m := newMemoryStore()

	v, err := m.incrBy("k", 5, time.Minute)
	if err != nil || v != 5 {
		t.Fatalf("incrBy = %d, %v, want 5, nil", v, err)
	}
	v, err = m.incrBy("k", 3, time.Minute)
	if err != nil || v != 8 {
		t.Fatalf("incrBy = %d, %v, want 8, nil", v, err)
	}

	got, err := m.get("k")
	if err != nil || got != 8 {
		t.Fatalf("get = %d, %v, want 8, nil", got, err)
	}

	got, err = m.get("missing")
	if err != nil || got != 0 {
		t.Fatalf("get missing = %d, %v, want 0, nil", got, err)
	}
}

// TestMemoryStoreExpiry asserts a counter reset to 0 (as a fresh key) once
// its TTL has elapsed, rather than continuing to accumulate forever.
func TestMemoryStoreExpiry(t *testing.T) {
	m := newMemoryStore()
	if _, err := m.incrBy("k", 5, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)

	got, err := m.get("k")
	if err != nil || got != 0 {
		t.Fatalf("get after expiry = %d, %v, want 0, nil", got, err)
	}

	v, err := m.incrBy("k", 2, time.Minute)
	if err != nil || v != 2 {
		t.Fatalf("incrBy after expiry = %d, %v, want 2, nil (fresh counter, not 7)", v, err)
	}
}

// TestMemoryStoreSweepTimeGated asserts the sweep gate is purely
// time-based (sweepEvery), independent of how many keys memoryStore
// holds: an incrBy call before the gate elapses leaves an already-expired
// key physically in place (no scan happened), and the first incrBy at or
// past the gate reclaims it. Time is injected via m.nowFn so the test
// needs no real sleep and no size threshold to trigger the behavior.
func TestMemoryStoreSweepTimeGated(t *testing.T) {
	m := newMemoryStore()
	fake := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	m.nowFn = func() time.Time { return fake }

	// lastSweep starts at the zero time, so this first incrBy always
	// sweeps (a no-op on an empty store) and sets lastSweep to fake.
	if _, err := m.incrBy("k1", 1, time.Nanosecond); err != nil {
		t.Fatal(err)
	}

	// k1 is already expired (ttl was 1ns), but stay well inside the
	// sweepEvery gate — the next incrBy must not scan, so k1 survives as
	// a physical (if stale) map entry.
	fake = fake.Add(time.Millisecond)
	if _, err := m.incrBy("k2", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if !memoryStoreHasKey(m, "k1") {
		t.Fatal("k1 must still be present: the sweep gate has not elapsed yet")
	}

	// Cross the sweepEvery boundary from the last sweep: this incrBy must
	// scan and reclaim k1.
	fake = fake.Add(sweepEvery)
	if _, err := m.incrBy("k3", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if memoryStoreHasKey(m, "k1") {
		t.Error("k1 should have been swept once sweepEvery elapsed")
	}
}

// memoryStoreHasKey reports whether key is physically present in m.data,
// bypassing get's expiry check — used to observe sweep timing directly.
func memoryStoreHasKey(m *memoryStore, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[key]
	return ok
}

// TestLimiterCostBudgetRoundsUSDConversion pins usdToMicros' rounding
// against a value (8.2) where float64 representation sits just under the
// true value: a truncating int64(8.2*1e6) would undershoot to 8_199_999,
// making a spend of exactly $8.199999 look already over an intended
// $8.20 budget. With rounding, 8_199_999 must stay under budget and only
// the exact $8.20 spend violates.
func TestLimiterCostBudgetRoundsUSDConversion(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	l := newLimiter(nil, true)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{CostPerDayUSD: 8.2}}}

	l.account(scopes, usage{}, 8_199_999)
	if v := l.checkAndCount(scopes); v != nil {
		t.Fatalf("spending $8.199999 against an $8.20 budget must not violate, got %+v", v)
	}

	l.account(scopes, usage{}, 1) // now exactly at $8.20
	if v := l.checkAndCount(scopes); v == nil {
		t.Fatal("spending exactly $8.20 against an $8.20 budget must violate")
	}
}

// TestAccountSkipsZeroMetrics asserts account writes no tokens counter
// when usage totals 0, and no cost counter when costMicros is 0 — each
// metric's absence is independently observable via get.
func TestAccountSkipsZeroMetrics(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	l := newLimiter(nil, true)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "z", limits: nil}}

	l.account(scopes, usage{}, 0)

	tok, ok := l.getCounter("user", "z", metricTok, windowDay, now)
	if !ok || tok != 0 {
		t.Errorf("tok counter = %d, ok=%v, want 0, true", tok, ok)
	}
	cost, ok := l.getCounter("user", "z", metricCost, windowDay, now)
	if !ok || cost != 0 {
		t.Errorf("cost counter = %d, ok=%v, want 0, true", cost, ok)
	}
}

// TestLimiterRequestPerDayLimit is carried-item (b): a per-day request
// limit blocks once the day's count exceeds it, independent of the
// (unset, unlimited) per-minute limit.
func TestLimiterRequestPerDayLimit(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	l := newLimiter(nil, true)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "a", limits: &LimitsConfig{RequestsPerDay: 2}}}

	if v := l.checkAndCount(scopes); v != nil {
		t.Fatal("1st should pass")
	}
	if v := l.checkAndCount(scopes); v != nil {
		t.Fatal("2nd should pass")
	}
	v := l.checkAndCount(scopes)
	if v == nil {
		t.Fatal("3rd should violate the day limit")
	}
	if v.storeDown {
		t.Error("storeDown must be false for a real limit breach")
	}
}

// TestLimiterTokensPerMonthBudget mirrors TestLimiterTokenBudget for the
// month window: accounting tokens past a month budget makes the next
// check refuse.
func TestLimiterTokensPerMonthBudget(t *testing.T) {
	l := newLimiter(nil, true)
	scopes := []limitScope{{kind: "group", id: "g", limits: &LimitsConfig{TokensPerMonth: 100}}}
	l.account(scopes, usage{prompt: 60, completion: 50}, 0) // 110 > 100
	if v := l.checkAndCount(scopes); v == nil {
		t.Fatal("over month token budget must refuse")
	}
}

// TestLimiterCostPerMonthBudget mirrors TestLimiterCostBudget for the
// month window, and checks the violation carries the month window's
// Retry-After.
func TestLimiterCostPerMonthBudget(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	l := newLimiter(nil, true)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{CostPerMonthUSD: 1.00}}}

	l.account(scopes, usage{}, 1_500_000) // $1.50 > $1.00 budget
	v := l.checkAndCount(scopes)
	if v == nil {
		t.Fatal("over month cost budget must refuse")
	}
	wantRetry := int(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).Sub(now).Seconds())
	if v.retryAfter != wantRetry {
		t.Errorf("retryAfter = %d, want %d (seconds to next UTC month start)", v.retryAfter, wantRetry)
	}
	if v.storeDown {
		t.Error("storeDown must be false for a real limit breach")
	}
}

// erroringStore is a counterStore stub that always fails, used to test the
// limiter's fail-open/fail-closed handling on a store error without a
// real network dependency (see redis_store_test.go for the equivalent
// end-to-end test against an unreachable redisStore).
type erroringStore struct {
	err error
}

func (s *erroringStore) incrBy(string, int64, time.Duration) (int64, error) { return 0, s.err }
func (s *erroringStore) get(string) (int64, error)                          { return 0, s.err }
func (s *erroringStore) getMulti([]string) ([]int64, error)                 { return nil, s.err }

// TestLimiter_FailOpen_StoreErrorUsesFallback is carried-item (c): a store
// error with failOpen=true must not block the request — the operation
// transparently falls back to the limiter's in-process memoryStore, and
// the fallback counter actually advances (proving the write really
// happened somewhere, not just that no violation was reported).
func TestLimiter_FailOpen_StoreErrorUsesFallback(t *testing.T) {
	store := &erroringStore{err: errors.New("boom")}
	l := newLimiter(store, true)
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerMinute: 100}}}

	if v := l.checkAndCount(scopes); v != nil {
		t.Fatalf("want no violation with failOpen=true on a store error, got %+v", v)
	}
	got, err := l.fallback.get(windowKey("user", "u", metricReq, windowDay, l.now()))
	if err != nil {
		t.Fatalf("fallback.get: %v", err)
	}
	if got != 1 {
		t.Errorf("fallback req:day counter = %d, want 1", got)
	}
}

// TestLimiter_FailClosed_StoreErrorReturnsStoreDownViolation is
// carried-item (c): failOpen=false refuses the request outright on a
// store error, rather than silently using the fallback.
func TestLimiter_FailClosed_StoreErrorReturnsStoreDownViolation(t *testing.T) {
	store := &erroringStore{err: errors.New("boom")}
	l := newLimiter(store, false)
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerMinute: 100}}}

	v := l.checkAndCount(scopes)
	if v == nil {
		t.Fatal("want a storeDown violation on a store error with failOpen=false")
	}
	if !v.storeDown {
		t.Error("v.storeDown = false, want true")
	}
	if v.message != "limit store unavailable" {
		t.Errorf("v.message = %q, want %q", v.message, "limit store unavailable")
	}
}

// succeedIncrFailGetStore is a counterStore stub whose incrBy always
// succeeds and whose get always errors. It isolates budgetViolation's own
// storeDown branch (limits.go) from checkAndCount's earlier incrCounter
// one: an erroringStore that fails both methods makes checkAndCount's
// initial req:min/req:day increments fail closed and return before
// evaluateScope — and therefore budgetViolation — ever runs, so a test
// built on it cannot actually prove budgetViolation's own branch works.
type succeedIncrFailGetStore struct {
	getErr error
}

func (s *succeedIncrFailGetStore) incrBy(string, int64, time.Duration) (int64, error) {
	return 1, nil
}
func (s *succeedIncrFailGetStore) get(string) (int64, error) { return 0, s.getErr }
func (s *succeedIncrFailGetStore) getMulti([]string) ([]int64, error) {
	return nil, s.getErr
}

// TestLimiter_FailClosed_BudgetReadReturnsStoreDownViolation covers the
// budgetViolation fail-closed path specifically (as opposed to the
// request-counter incrCounter path TestLimiter_FailClosed_
// StoreErrorReturnsStoreDownViolation covers above): a store error on a
// token/cost budget read also refuses the request when failOpen is false.
// The store's incrBy succeeds so checkAndCount's req:min/req:day
// increments pass and evaluateScope actually reaches budgetViolation; a
// TokensPerDay limit makes evaluateScope call it.
func TestLimiter_FailClosed_BudgetReadReturnsStoreDownViolation(t *testing.T) {
	store := &succeedIncrFailGetStore{getErr: errors.New("boom")}
	l := newLimiter(store, false)
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{TokensPerDay: 100}}}

	v := l.checkAndCount(scopes)
	if v == nil {
		t.Fatal("want a storeDown violation from the budget-read path with failOpen=false")
	}
	if !v.storeDown {
		t.Error("v.storeDown = false, want true")
	}
}

// TestLimiter_FailClosed_AccountDropsSampleOnStoreError asserts account
// drops a sample entirely — writes it nowhere — when failOpen=false and
// the store errors, rather than falling back to counting it locally (which
// would double-count once the store recovers). account has no error
// return to signal the drop, so this is observed via the fallback staying
// at 0.
func TestLimiter_FailClosed_AccountDropsSampleOnStoreError(t *testing.T) {
	store := &erroringStore{err: errors.New("boom")}
	l := newLimiter(store, false)
	scopes := []limitScope{{kind: "user", id: "u", limits: nil}}

	l.account(scopes, usage{prompt: 10, completion: 10}, 500)

	got, err := l.fallback.get(windowKey("user", "u", metricTok, windowDay, l.now()))
	if err != nil {
		t.Fatalf("fallback.get: %v", err)
	}
	if got != 0 {
		t.Errorf("fallback tok:day counter = %d, want 0 (sample must be dropped, not counted locally)", got)
	}
}

// TestLimiter_LogsStoreErrorOncePerRateLimit asserts a store error is
// logged via the limiter's injectable log func, rate-limited to once per
// storeErrorLogEvery — a store outage under load must not flood the log
// with one line per request, however many store operations fail within
// the window.
func TestLimiter_LogsStoreErrorOncePerRateLimit(t *testing.T) {
	store := &erroringStore{err: errors.New("boom")}
	l := newLimiter(store, true)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	l.nowFn = func() time.Time { return now }

	var logged []string
	l.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	scopes := []limitScope{{kind: "user", id: "u", limits: nil}}

	l.checkAndCount(scopes) // two store ops (min, day) within one call
	if len(logged) != 1 {
		t.Fatalf("logged = %d lines after the first checkAndCount, want 1 (rate-limited)", len(logged))
	}

	l.checkAndCount(scopes) // still inside the rate-limit window
	if len(logged) != 1 {
		t.Fatalf("logged = %d lines after a second call inside the window, want 1", len(logged))
	}

	now = now.Add(storeErrorLogEvery)
	l.checkAndCount(scopes)
	if len(logged) != 2 {
		t.Fatalf("logged = %d lines after the rate-limit window elapsed, want 2", len(logged))
	}
}

// countingErrorStore is a counterStore stub that always errors and counts
// how many times each method is actually invoked. It proves the
// store-down latch (limits.go) skips the network call entirely during
// its window, rather than merely tolerating the error each time.
type countingErrorStore struct {
	err       error
	incrCalls int
	getCalls  int
}

func (s *countingErrorStore) incrBy(string, int64, time.Duration) (int64, error) {
	s.incrCalls++
	return 0, s.err
}

func (s *countingErrorStore) get(string) (int64, error) {
	s.getCalls++
	return 0, s.err
}

func (s *countingErrorStore) getMulti([]string) ([]int64, error) {
	s.getCalls++
	return nil, s.err
}

// TestLimiter_StoreDownLatch_SkipsStoreCallsWithinWindow is review-round-3
// item 2: after a store operation fails, every operation for the next
// storeDownLatchFor must short-circuit straight to the fail-open policy
// without calling the store at all — a request that touches several
// counters (min/day/month, tokens/cost, user/group) must not pay a fresh
// bounded wait against a store that just proved unreachable for each one.
// failOpen=true is used so checkAndCount completes both its req:min and
// req:day increments per call instead of short-circuiting after the
// first — this is what makes the "zero additional store calls" assertion
// meaningful, rather than trivially true because checkAndCount stops
// after one op regardless of the latch.
func TestLimiter_StoreDownLatch_SkipsStoreCallsWithinWindow(t *testing.T) {
	store := &countingErrorStore{err: errors.New("boom")}
	l := newLimiter(store, true)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "u", limits: nil}}

	if v := l.checkAndCount(scopes); v != nil {
		t.Fatalf("want no violation with failOpen=true, got %+v", v)
	}
	firstCalls := store.incrCalls
	if firstCalls == 0 {
		t.Fatal("want the first call to actually reach the store at least once")
	}

	if v := l.checkAndCount(scopes); v != nil {
		t.Fatalf("want no violation with failOpen=true, got %+v", v)
	}
	if store.incrCalls != firstCalls {
		t.Errorf("incrBy calls = %d after a second checkAndCount within the latch window, want unchanged at %d (the store must not be called)", store.incrCalls, firstCalls)
	}

	now = now.Add(storeDownLatchFor) // latch expires
	if v := l.checkAndCount(scopes); v != nil {
		t.Fatalf("want no violation with failOpen=true, got %+v", v)
	}
	if store.incrCalls <= firstCalls {
		t.Errorf("incrBy calls = %d after the latch window elapsed, want more than %d (the store must be probed again)", store.incrCalls, firstCalls)
	}
}

// TestLimitsConfigValidate is carried-item (a): validate rejects
// NaN/±Inf/negative cost limits and negative int64 count limits, accepts
// zero/positive values, and treats a nil receiver (no limits configured
// at all) as valid.
func TestLimitsConfigValidate(t *testing.T) {
	cases := []struct {
		lc      *LimitsConfig
		name    string
		wantErr bool
	}{
		{name: "nil is valid", lc: nil, wantErr: false},
		{name: "zero value is valid", lc: &LimitsConfig{}, wantErr: false},
		{name: "positive values are valid", lc: &LimitsConfig{
			RequestsPerMinute: 1, RequestsPerDay: 1, TokensPerDay: 1, TokensPerMonth: 1,
			CostPerDayUSD: 1.5, CostPerMonthUSD: 1.5,
		}, wantErr: false},
		{name: "negative RequestsPerMinute", lc: &LimitsConfig{RequestsPerMinute: -1}, wantErr: true},
		{name: "negative RequestsPerDay", lc: &LimitsConfig{RequestsPerDay: -1}, wantErr: true},
		{name: "negative TokensPerDay", lc: &LimitsConfig{TokensPerDay: -1}, wantErr: true},
		{name: "negative TokensPerMonth", lc: &LimitsConfig{TokensPerMonth: -1}, wantErr: true},
		{name: "negative CostPerDayUSD", lc: &LimitsConfig{CostPerDayUSD: -0.01}, wantErr: true},
		{name: "negative CostPerMonthUSD", lc: &LimitsConfig{CostPerMonthUSD: -0.01}, wantErr: true},
		{name: "NaN CostPerDayUSD", lc: &LimitsConfig{CostPerDayUSD: math.NaN()}, wantErr: true},
		{name: "+Inf CostPerMonthUSD", lc: &LimitsConfig{CostPerMonthUSD: math.Inf(1)}, wantErr: true},
		{name: "-Inf CostPerDayUSD", lc: &LimitsConfig{CostPerDayUSD: math.Inf(-1)}, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.lc.validate()
			if (err != nil) != c.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

// TestLimiter_FailPolicyGet covers failPolicyGet's own fail-open/fail-closed
// branching in isolation (storeGet's error/latched paths already drive it
// indirectly elsewhere in this file): failOpen=true reads through to the
// in-process fallback, failOpen=false refuses outright.
func TestLimiter_FailPolicyGet(t *testing.T) {
	cases := []struct {
		name     string
		failOpen bool
		wantOK   bool
	}{
		{name: "failOpen true reads the fallback", failOpen: true, wantOK: true},
		{name: "failOpen false refuses", failOpen: false, wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := newLimiter(nil, c.failOpen)
			v, ok := l.failPolicyGet("k")
			require.Equal(t, c.wantOK, ok)
			if c.wantOK {
				assert.Equal(t, int64(0), v)
			}
		})
	}
}
