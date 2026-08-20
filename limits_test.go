package traefikllmgateway

import (
	"fmt"
	"testing"
	"time"
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

// TestMemoryStoreSweepOnAccess grows the store past
// memoryStoreSweepThreshold, backdates every entry's expiry directly
// (rather than racing a short TTL against the insertion loop's own real
// wall-clock time, which is inherently flaky), and asserts the next
// incrBy's opportunistic sweep reclaims every expired entry.
func TestMemoryStoreSweepOnAccess(t *testing.T) {
	m := newMemoryStore()
	for i := 0; i < memoryStoreSweepThreshold+10; i++ {
		key := fmt.Sprintf("k%d", i)
		if _, err := m.incrBy(key, 1, time.Hour); err != nil {
			t.Fatal(err)
		}
	}

	m.mu.Lock()
	past := time.Now().Add(-time.Hour)
	for _, e := range m.data {
		e.expiry = past
	}
	setupCount := len(m.data)
	m.mu.Unlock()
	if setupCount != memoryStoreSweepThreshold+10 {
		t.Fatalf("setup: got %d keys, want %d", setupCount, memoryStoreSweepThreshold+10)
	}

	if _, err := m.incrBy("trigger-sweep", 1, time.Minute); err != nil {
		t.Fatal(err)
	}

	m.mu.Lock()
	n := len(m.data)
	m.mu.Unlock()
	if n != 1 {
		t.Errorf("expected sweep to reclaim every expired entry, %d keys remain (want 1: trigger-sweep)", n)
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

	tok, err := l.getCounter("user", "z", metricTok, windowDay, now)
	if err != nil || tok != 0 {
		t.Errorf("tok counter = %d, %v, want 0, nil", tok, err)
	}
	cost, err := l.getCounter("user", "z", metricCost, windowDay, now)
	if err != nil || cost != 0 {
		t.Errorf("cost counter = %d, %v, want 0, nil", cost, err)
	}
}
