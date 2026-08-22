package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
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
// formatted from t.UTC() at each window's granularity — including hour
// (v0.2 data-layer task).
func TestWindowKeyFormat(t *testing.T) {
	tm := time.Date(2026, 8, 20, 10, 4, 59, 0, time.FixedZone("UTC+1", 3600)) // 09:04:59 UTC
	cases := []struct {
		window string
		want   string
	}{
		{windowHour, "llmgw:user:a:req:hour:2026082009"},
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

// TestWindowKeyFormat_HourUTCBoundary pins the hour bucket rolling over
// exactly at the UTC hour boundary, and proves a non-UTC input is
// converted to UTC first (an input at 00:04:59 in a UTC+1 zone is 23:04:59
// UTC the PREVIOUS day — a bug converting the hour field alone without
// re-deriving the date would produce "...23" glued onto the wrong day).
func TestWindowKeyFormat_HourUTCBoundary(t *testing.T) {
	justBefore := time.Date(2026, 8, 20, 8, 59, 59, 0, time.UTC)
	justAfter := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	if got := windowKey("user", "a", "req", windowHour, justBefore); got != "llmgw:user:a:req:hour:2026082008" {
		t.Errorf("just before the hour boundary: got %q", got)
	}
	if got := windowKey("user", "a", "req", windowHour, justAfter); got != "llmgw:user:a:req:hour:2026082009" {
		t.Errorf("just after the hour boundary: got %q", got)
	}

	// 00:04:59 in UTC+1 is 23:04:59 UTC the previous day.
	crossMidnight := time.Date(2026, 8, 21, 0, 4, 59, 0, time.FixedZone("UTC+1", 3600))
	if got := windowKey("user", "a", "req", windowHour, crossMidnight); got != "llmgw:user:a:req:hour:2026082023" {
		t.Errorf("cross-midnight non-UTC input: got %q, want the previous UTC day's hour 23 bucket", got)
	}
}

// TestWindowEnd_HourBoundary asserts windowEnd's new hour case (v0.2
// data-layer task) returns the next UTC hour boundary, and that a value
// exactly on the boundary reports zero seconds remaining in that boundary
// (retryAfterSeconds rounds a zero duration up to 0, distinct from
// rounding a fractional second up to 1 — see TestLimiterRetryAfter for
// the day/month/min cases this mirrors).
func TestWindowEnd_HourBoundary(t *testing.T) {
	t.Run("mid-hour", func(t *testing.T) {
		got := windowEnd(time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC), windowHour)
		want := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("windowEnd = %v, want %v", got, want)
		}
	})
	t.Run("exactly on the boundary", func(t *testing.T) {
		got := windowEnd(time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC), windowHour)
		want := time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC)
		if !got.Equal(want) {
			t.Errorf("windowEnd = %v, want %v", got, want)
		}
	})
}

// TestHistoryStepBack_HourAndDay asserts the fixed-duration step-back
// cases (hour, day) land i units before now exactly, including across a
// UTC day boundary.
func TestHistoryStepBack_HourAndDay(t *testing.T) {
	now := time.Date(2026, 8, 20, 1, 30, 0, 0, time.UTC)

	if got := historyStepBack(now, windowHour, 0); !got.Equal(now) {
		t.Errorf("i=0 must be now itself, got %v", got)
	}
	wantHour := time.Date(2026, 8, 19, 23, 30, 0, 0, time.UTC) // 2 hours back, crossing midnight
	if got := historyStepBack(now, windowHour, 2); !got.Equal(wantHour) {
		t.Errorf("i=2 hours back = %v, want %v", got, wantHour)
	}

	wantDay := time.Date(2026, 8, 17, 1, 30, 0, 0, time.UTC)
	if got := historyStepBack(now, windowDay, 3); !got.Equal(wantDay) {
		t.Errorf("i=3 days back = %v, want %v", got, wantDay)
	}
}

// TestHistoryStepBack_MonthAnchorsOnDayOne is the month-arithmetic trap
// TestLimiterRetryAfter's own month case does not exercise: stepping back
// from a high day-of-month (the 31st) with a naive
// now.AddDate(0, -i, 0) overflows a shorter target month (e.g. "Mar 31"
// minus one month normalizes to "Mar 3", not "Feb 28") and would land in
// the WRONG calendar month's bucket. historyStepBack avoids this by
// anchoring on day 1 first — this test drives it from Mar 31 and Jan 31
// specifically because those are the inputs that would expose a
// regression back to the naive form.
func TestHistoryStepBack_MonthAnchorsOnDayOne(t *testing.T) {
	cases := []struct {
		now  time.Time
		name string
		want string
		i    int
	}{
		{name: "Mar 31 minus 1 month = Feb", now: time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC), i: 1, want: "202602"},
		{name: "Jan 31 minus 1 month = Dec (year rollover)", now: time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC), i: 1, want: "202512"},
		{name: "May 31 minus 3 months = Feb", now: time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC), i: 3, want: "202602"},
		{name: "i=0 is now's own month", now: time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC), i: 0, want: "202603"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bucketFor(historyStepBack(c.now, windowMonth, c.i), windowMonth)
			if got != c.want {
				t.Errorf("bucketFor(historyStepBack(%v, month, %d)) = %q, want %q", c.now, c.i, got, c.want)
			}
		})
	}
}

// TestHistoryBucketKeys_OldestFirstInclusiveOfCurrent asserts
// historyBucketKeys returns span buckets oldest-first, with the LAST
// entry being now's own (current, possibly partial) bucket — the
// "inclusive of current bucket" contract GET /admin/api/usage/history
// promises.
func TestHistoryBucketKeys_OldestFirstInclusiveOfCurrent(t *testing.T) {
	now := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	keys, buckets := historyBucketKeys("total", "all", "req", windowHour, now, 4)

	wantBuckets := []string{"2026082011", "2026082012", "2026082013", "2026082014"}
	if len(buckets) != len(wantBuckets) {
		t.Fatalf("len(buckets) = %d, want %d", len(buckets), len(wantBuckets))
	}
	for i, want := range wantBuckets {
		if buckets[i] != want {
			t.Errorf("buckets[%d] = %q, want %q", i, buckets[i], want)
		}
		wantKey := "llmgw:total:all:req:hour:" + want
		if keys[i] != wantKey {
			t.Errorf("keys[%d] = %q, want %q", i, keys[i], wantKey)
		}
	}
}

// TestLimiterHistory_OneBatchCallOldestFirst drives limiter.history
// against a counting stub store: it must issue exactly ONE getMulti call
// for the whole span, and the returned points must be oldest-first with
// the seeded values in the right buckets.
func TestLimiterHistory_OneBatchCallOldestFirst(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	countStore := &historyCountingStore{values: map[string]int64{
		windowKey("total", "all", metricReq, windowHour, now.Add(-2*time.Hour)): 5,
		windowKey("total", "all", metricReq, windowHour, now):                   9,
	}}
	l := newLimiter(countStore, true)
	l.nowFn = func() time.Time { return now }

	points, ok := l.history("total", "all", "req", windowHour, now, 3)
	if !ok {
		t.Fatal("want ok=true")
	}
	if countStore.getMultiCalls != 1 {
		t.Errorf("getMultiCalls = %d, want 1", countStore.getMultiCalls)
	}
	if len(points) != 3 {
		t.Fatalf("len(points) = %d, want 3", len(points))
	}
	if points[0].value != 5 || points[0].bucket != bucketFor(now.Add(-2*time.Hour), windowHour) {
		t.Errorf("points[0] (oldest) = %+v, want value 5 at the -2h bucket", points[0])
	}
	if points[1].value != 0 {
		t.Errorf("points[1] (unseeded -1h bucket) = %+v, want value 0", points[1])
	}
	if points[2].value != 9 || points[2].bucket != bucketFor(now, windowHour) {
		t.Errorf("points[2] (newest, current bucket) = %+v, want value 9", points[2])
	}
}

// historyCountingStore is a counterStore stub whose getMulti serves fixed
// values and counts its own calls — mirrors admin_test.go's
// countingMultiStore, defined separately here since limits_test.go must
// not depend on admin_test.go's test-only types.
type historyCountingStore struct {
	values        map[string]int64
	getMultiCalls int
}

func (s *historyCountingStore) incrBy(string, int64, time.Duration) (int64, error) { return 0, nil }
func (s *historyCountingStore) get(string) (int64, error)                          { return 0, nil }
func (s *historyCountingStore) getMulti(keys []string) ([]int64, error) {
	s.getMultiCalls++
	out := make([]int64, len(keys))
	for i, k := range keys {
		out[i] = s.values[k]
	}
	return out, nil
}
func (s *historyCountingStore) incrMulti(entries []counterIncr) ([]int64, error) {
	return make([]int64, len(entries)), nil
}
func (s *historyCountingStore) incrAndGetMulti(entries []counterIncr, reads []string) ([]int64, []int64, error) {
	readVals, err := s.getMulti(reads)
	return make([]int64, len(entries)), readVals, err
}

// TestLimiterHistory_StoreDown asserts limiter.history reports ok=false
// when the configured store errors and failOpen is false — the caller
// (GET /admin/api/usage/history) must answer 503, never a silently-zero
// series.
func TestLimiterHistory_StoreDown(t *testing.T) {
	l := newLimiter(alwaysErrStore{}, false)
	points, ok := l.history("total", "all", "req", windowDay, time.Now(), 5)
	if ok {
		t.Fatal("want ok=false when the configured store errors and failOpen is false")
	}
	if points != nil {
		t.Errorf("points = %+v, want nil on a storeDown read", points)
	}
}

// TestWithTotalScope_AppendsUnlimitedTotalScope asserts withTotalScope
// appends exactly one {kind: totalScopeKind, id: totalScopeID, limits:
// nil} scope after whatever buildLimitScopes already returned.
func TestWithTotalScope_AppendsUnlimitedTotalScope(t *testing.T) {
	base := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerMinute: 1}}}
	got := withTotalScope(base)

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].kind != "user" || got[0].id != "u" {
		t.Errorf("got[0] = %+v, want the original user scope untouched", got[0])
	}
	total := got[1]
	if total.kind != totalScopeKind || total.id != totalScopeID || total.limits != nil {
		t.Errorf("got[1] = %+v, want kind=%q id=%q limits=nil", total, totalScopeKind, totalScopeID)
	}
}

// TestWithTotalScope_CountedButNeverEvaluated drives checkAndCount many
// times against a scopes slice built via withTotalScope: the total
// scope's own limits are always nil, so no volume of traffic can ever
// make it violate — checkAndCount's own nil-limits check skips it — while its
// req counter still climbs by exactly one per call, proving
// checkAndCount's counting loop does not skip a nil-limits scope the way
// evaluation does.
func TestWithTotalScope_CountedButNeverEvaluated(t *testing.T) {
	l := newLimiter(nil, true)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	l.nowFn = func() time.Time { return now }

	scopes := withTotalScope([]limitScope{{kind: "user", id: "u", limits: nil}})
	const n = 250
	for i := 0; i < n; i++ {
		if v := l.checkAndCount(scopes); v != nil {
			t.Fatalf("iteration %d: total scope must never violate (limits always nil), got %+v", i, v)
		}
	}

	total, ok := l.getCounter(totalScopeKind, totalScopeID, metricReq, windowDay, now)
	if !ok || total != n {
		t.Errorf("total req:day = %d, ok=%v, want %d (counted every call despite never evaluating)", total, ok, n)
	}
	totalHour, ok := l.getCounter(totalScopeKind, totalScopeID, metricReq, windowHour, now)
	if !ok || totalHour != n {
		t.Errorf("total req:hour = %d, ok=%v, want %d", totalHour, ok, n)
	}
}

// TestWithTotalScope_AccountWritesHourBuckets asserts account (v0.2
// data-layer task) writes the total scope's hour-window tokin/tokout/cost
// counters, not just day/month — checkAndCount's own hour write is
// covered by TestWithTotalScope_CountedButNeverEvaluated above.
func TestWithTotalScope_AccountWritesHourBuckets(t *testing.T) {
	l := newLimiter(nil, true)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	l.nowFn = func() time.Time { return now }

	scopes := withTotalScope([]limitScope{{kind: "user", id: "u", limits: nil}})
	l.account(scopes, usage{prompt: 4, completion: 6}, 700)

	tokIn, ok := l.getCounter(totalScopeKind, totalScopeID, metricTokIn, windowHour, now)
	if !ok || tokIn != 4 {
		t.Errorf("total tokin:hour = %d, ok=%v, want 4", tokIn, ok)
	}
	tokOut, ok := l.getCounter(totalScopeKind, totalScopeID, metricTokOut, windowHour, now)
	if !ok || tokOut != 6 {
		t.Errorf("total tokout:hour = %d, ok=%v, want 6", tokOut, ok)
	}
	cost, ok := l.getCounter(totalScopeKind, totalScopeID, metricCost, windowHour, now)
	if !ok || cost != 700 {
		t.Errorf("total cost:hour = %d, ok=%v, want 700", cost, ok)
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

// TestMemoryStore_TTLClampedToMax is the perf-review (2026-08-21) case:
// a TTL beyond memoryStoreMaxTTL (48h) — e.g. dayWindowTTL at 35 days or
// monthWindowTTL at 400 days — must be clamped down to 48h before it is
// ever applied, so the fallback's own live-key count stays bounded
// independent of which window a caller writes.
func TestMemoryStore_TTLClampedToMax(t *testing.T) {
	m := newMemoryStore()
	fake := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	m.nowFn = func() time.Time { return fake }

	cases := []struct {
		name string
		ttl  time.Duration
	}{
		{"dayWindowTTL (35d)", dayWindowTTL},
		{"monthWindowTTL (400d)", monthWindowTTL},
		{"an arbitrary huge ttl", 10 * 365 * 24 * time.Hour},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := fmt.Sprintf("k%d", i)
			if _, err := m.incrBy(key, 1, c.ttl); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			got := m.data[key].expiry
			m.mu.Unlock()
			want := fake.Add(memoryStoreMaxTTL)
			if !got.Equal(want) {
				t.Errorf("expiry = %v, want %v (clamped to memoryStoreMaxTTL, not the requested %v)", got, want, c.ttl)
			}
		})
	}
}

// TestMemoryStore_TTLBelowMaxIsUnaffected asserts the clamp is a ceiling,
// not a rewrite: a TTL already at or under memoryStoreMaxTTL passes
// through exactly as requested — hourWindowTTL (48h) sits precisely at
// the clamp boundary and must round-trip unchanged, not get pushed under
// it by an off-by-one comparison.
func TestMemoryStore_TTLBelowMaxIsUnaffected(t *testing.T) {
	m := newMemoryStore()
	fake := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	m.nowFn = func() time.Time { return fake }

	cases := []struct {
		name string
		ttl  time.Duration
	}{
		{"minWindowTTL (2m)", minWindowTTL},
		{"hourWindowTTL (48h, exactly at the clamp)", hourWindowTTL},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := fmt.Sprintf("k%d", i)
			if _, err := m.incrBy(key, 1, c.ttl); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			got := m.data[key].expiry
			m.mu.Unlock()
			want := fake.Add(c.ttl)
			if !got.Equal(want) {
				t.Errorf("expiry = %v, want %v (ttl under the clamp must pass through unchanged)", got, want)
			}
		})
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

// TestAccountSkipsZeroMetrics asserts account writes no tokin/tokout
// counter when the matching usage direction is 0, and no cost counter
// when costMicros is 0 — each metric's absence is independently
// observable via get.
func TestAccountSkipsZeroMetrics(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	l := newLimiter(nil, true)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "z", limits: nil}}

	l.account(scopes, usage{}, 0)

	tokIn, ok := l.getCounter("user", "z", metricTokIn, windowDay, now)
	if !ok || tokIn != 0 {
		t.Errorf("tokin counter = %d, ok=%v, want 0, true", tokIn, ok)
	}
	tokOut, ok := l.getCounter("user", "z", metricTokOut, windowDay, now)
	if !ok || tokOut != 0 {
		t.Errorf("tokout counter = %d, ok=%v, want 0, true", tokOut, ok)
	}
	cost, ok := l.getCounter("user", "z", metricCost, windowDay, now)
	if !ok || cost != 0 {
		t.Errorf("cost counter = %d, ok=%v, want 0, true", cost, ok)
	}
}

// TestAccountSplitsTokensIndependently asserts account writes prompt
// tokens to metricTokIn and completion tokens to metricTokOut
// independently — including the asymmetric case (prompt only, no
// completion yet) the estimation fallback and a dropped stream both rely
// on: a nonzero prompt with a zero completion must still write tokin
// alone, never skip it because completion is 0.
func TestAccountSplitsTokensIndependently(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	l := newLimiter(nil, true)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "split", limits: nil}}

	l.account(scopes, usage{prompt: 25}, 0) // completion left at zero

	tokIn, ok := l.getCounter("user", "split", metricTokIn, windowDay, now)
	if !ok || tokIn != 25 {
		t.Errorf("tokin counter = %d, ok=%v, want 25, true", tokIn, ok)
	}
	tokOut, ok := l.getCounter("user", "split", metricTokOut, windowDay, now)
	if !ok || tokOut != 0 {
		t.Errorf("tokout counter = %d, ok=%v, want 0, true (no completion tokens reported)", tokOut, ok)
	}

	l.account(scopes, usage{completion: 9}, 0) // prompt left at zero this time
	tokIn, ok = l.getCounter("user", "split", metricTokIn, windowDay, now)
	if !ok || tokIn != 25 {
		t.Errorf("tokin counter = %d, ok=%v, want still 25 (this call reported no prompt tokens)", tokIn, ok)
	}
	tokOut, ok = l.getCounter("user", "split", metricTokOut, windowDay, now)
	if !ok || tokOut != 9 {
		t.Errorf("tokout counter = %d, ok=%v, want 9", tokOut, ok)
	}
}

// TestTokenBudgetSumsInAndOut asserts a TokensPerDay/TokensPerMonth limit
// enforces a TOTAL budget across metricTokIn and metricTokOut combined —
// the operator directive's "limits stay total-token (limiter sums both at
// read)": neither direction alone crosses the budget, but their sum does.
func TestTokenBudgetSumsInAndOut(t *testing.T) {
	l := newLimiter(nil, true)
	scopes := []limitScope{{kind: "group", id: "g", limits: &LimitsConfig{TokensPerDay: 100}}}

	l.account(scopes, usage{prompt: 60, completion: 45}, 0) // 105 > 100, split 60/45
	if v := l.checkAndCount(scopes); v == nil {
		t.Fatal("60 tokin + 45 tokout = 105 > 100 must refuse, even though neither direction alone exceeds 100")
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
func (s *erroringStore) incrMulti([]counterIncr) ([]int64, error)           { return nil, s.err }
func (s *erroringStore) incrAndGetMulti([]counterIncr, []string) ([]int64, []int64, error) {
	return nil, nil, s.err
}

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

// succeedIncrFailGetStore is a counterStore stub whose incrBy/incrMulti
// always succeed and whose get/getMulti/incrAndGetMulti always error. Its
// incrMulti/get/getMulti stay independently overridable (used by any
// OTHER caller — account, currentUsage, and so on — that still calls them
// separately) even though checkAndCount itself, since the round-3 fusion
// (2026-08-22), calls incrAndGetMulti exclusively: a store that cannot
// serve a read reliably fails that ONE combined round trip as a whole,
// there being no longer a separate "increment succeeded, read failed"
// split to isolate at the store level for checkAndCount specifically.
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
func (s *succeedIncrFailGetStore) incrMulti(entries []counterIncr) ([]int64, error) {
	out := make([]int64, len(entries))
	for i := range out {
		out[i] = 1
	}
	return out, nil
}
func (s *succeedIncrFailGetStore) incrAndGetMulti([]counterIncr, []string) ([]int64, []int64, error) {
	return nil, nil, s.getErr
}

// TestLimiter_FailClosed_BudgetReadReturnsStoreDownViolation covers the
// budget-read fail-closed path: a store that cannot be trusted to serve a
// token/cost budget read refuses the request when failOpen is false, via
// checkAndCount's own storeDownViolation short-circuit (limits.go) — the
// same contract the pre-fusion budgetViolation helper enforced before
// checkAndCount's admission round trip was fused (perf review round 3,
// 2026-08-22). A TokensPerDay limit is what makes checkAndCount build a
// budget probe/read for this scope at all.
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

	gotIn, err := l.fallback.get(windowKey("user", "u", metricTokIn, windowDay, l.now()))
	if err != nil {
		t.Fatalf("fallback.get: %v", err)
	}
	if gotIn != 0 {
		t.Errorf("fallback tokin:day counter = %d, want 0 (sample must be dropped, not counted locally)", gotIn)
	}
	gotOut, err := l.fallback.get(windowKey("user", "u", metricTokOut, windowDay, l.now()))
	if err != nil {
		t.Fatalf("fallback.get: %v", err)
	}
	if gotOut != 0 {
		t.Errorf("fallback tokout:day counter = %d, want 0 (sample must be dropped, not counted locally)", gotOut)
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

func (s *countingErrorStore) incrMulti([]counterIncr) ([]int64, error) {
	s.incrCalls++
	return nil, s.err
}

// incrAndGetMulti counts against the same incrCalls field incrMulti uses:
// checkAndCount's own admission round trip calls incrAndGetMulti
// exclusively since the round-3 fusion (2026-08-22, perf review), so
// TestLimiter_StoreDownLatch_SkipsStoreCallsWithinWindow's "the store must
// not be called" assertion needs this call counted the same way.
func (s *countingErrorStore) incrAndGetMulti([]counterIncr, []string) ([]int64, []int64, error) {
	s.incrCalls++
	return nil, nil, s.err
}

// TestLimiter_StoreDownLatch_SkipsStoreCallsWithinWindow is review-round-3
// item 2: after a store operation fails, every operation for the next
// storeDownLatchFor must short-circuit straight to the fail-open policy
// without calling the store at all — a request that touches several
// counters (min/day/hour, tokens/cost, user/group) must not pay a fresh
// bounded wait against a store that just proved unreachable for each one.
// checkAndCount batches its whole req:min/req:day/req:hour increment into
// ONE incrMulti call (perf review, 2026-08-21), so "the store must not be
// called" now means that single call is skipped entirely during the latch
// window, not that some subset of several per-key calls is. failOpen=true
// is required so every checkAndCount call below returns no violation
// (ok=true via the fallback) instead of a storeDownViolation — the
// incrCalls assertions only make sense if checkAndCount actually
// completes normally each time.
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

// --- perf review, 2026-08-21: single-batch counter writes ---

// countingIncrStore is a counterStore stub whose incrMulti/getMulti/
// incrAndGetMulti all succeed against a real backing map (so repeated
// calls actually accumulate, proving checkAndCount/account read back
// correct values after batching, not just that batching happened at all),
// counting how many times each was called. account still calls incrMulti
// directly; checkAndCount, since the round-3 fusion (2026-08-22, perf
// review), calls incrAndGetMulti exclusively — so the two counters below
// track genuinely different callers, not the same round trip under two
// names. incrBy is never exercised by either caller and just returns a
// zero value.
type countingIncrStore struct {
	values               map[string]int64
	incrMultiCalls       int
	incrAndGetMultiCalls int
	getMultiCalls        int
}

func (s *countingIncrStore) incrBy(string, int64, time.Duration) (int64, error) { return 0, nil }

func (s *countingIncrStore) get(key string) (int64, error) {
	if s.values == nil {
		return 0, nil
	}
	return s.values[key], nil
}

func (s *countingIncrStore) getMulti(keys []string) ([]int64, error) {
	s.getMultiCalls++
	out := make([]int64, len(keys))
	for i, k := range keys {
		if s.values != nil {
			out[i] = s.values[k]
		}
	}
	return out, nil
}

func (s *countingIncrStore) incrMulti(entries []counterIncr) ([]int64, error) {
	s.incrMultiCalls++
	if s.values == nil {
		s.values = make(map[string]int64)
	}
	out := make([]int64, len(entries))
	for i, e := range entries {
		s.values[e.key] += e.delta
		out[i] = s.values[e.key]
	}
	return out, nil
}

func (s *countingIncrStore) incrAndGetMulti(entries []counterIncr, reads []string) ([]int64, []int64, error) {
	s.incrAndGetMultiCalls++
	if s.values == nil {
		s.values = make(map[string]int64)
	}
	incrVals := make([]int64, len(entries))
	for i, e := range entries {
		s.values[e.key] += e.delta
		incrVals[i] = s.values[e.key]
	}
	readVals := make([]int64, len(reads))
	for i, k := range reads {
		readVals[i] = s.values[k]
	}
	return incrVals, readVals, nil
}

// TestCheckAndCount_OneIncrAndGetMultiCallRegardlessOfScopeCount is the
// perf-review round-3 (2026-08-22) case for checkAndCount: a 3-scope
// request (user, group, total — 9 req counters, min/day/hour x 3 scopes)
// must cost exactly ONE incrAndGetMulti round trip, not one per counter
// and not a separate round trip for token/cost budget reads, and the
// batched reply must still drive correct evaluation — a real
// requestsPerMinute violation on the 3rd call, with the exact right
// counter value. Supersedes the pre-fusion incrMulti-only version of this
// test (perf review, 2026-08-21): checkAndCount no longer calls incrMulti
// at all, only incrAndGetMulti (account still does, covered separately
// below by TestAccount_OneIncrMultiCallRegardlessOfScopeOrMetricCount).
func TestCheckAndCount_OneIncrAndGetMultiCallRegardlessOfScopeCount(t *testing.T) {
	store := &countingIncrStore{}
	l := newLimiter(store, true)
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	l.nowFn = func() time.Time { return now }

	scopes := []limitScope{
		{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerMinute: 2}},
		{kind: "group", id: "g", limits: nil},
		{kind: totalScopeKind, id: totalScopeID, limits: nil},
	}

	if v := l.checkAndCount(scopes); v != nil {
		t.Fatalf("1st call should pass, got %+v", v)
	}
	if store.incrAndGetMultiCalls != 1 {
		t.Errorf("incrAndGetMultiCalls after 1st checkAndCount = %d, want 1", store.incrAndGetMultiCalls)
	}

	if v := l.checkAndCount(scopes); v != nil {
		t.Fatalf("2nd call should pass, got %+v", v)
	}
	if store.incrAndGetMultiCalls != 2 {
		t.Errorf("incrAndGetMultiCalls after 2nd checkAndCount = %d, want 2 (one call per checkAndCount, not per scope or per counter)", store.incrAndGetMultiCalls)
	}

	v := l.checkAndCount(scopes)
	if v == nil {
		t.Fatal("3rd call should violate u's requestsPerMinute:2")
	}
	if v.storeDown {
		t.Error("v.storeDown must be false — this is a real limit breach, not a store failure")
	}
	if store.incrAndGetMultiCalls != 3 {
		t.Errorf("incrAndGetMultiCalls after 3rd checkAndCount = %d, want 3", store.incrAndGetMultiCalls)
	}
	if store.incrMultiCalls != 0 {
		t.Errorf("incrMultiCalls = %d, want 0 (checkAndCount must never call the separate incrMulti after fusion)", store.incrMultiCalls)
	}

	// Values correct: batching three scopes into one call must not
	// corrupt any one scope's own counter.
	uMin, ok := l.getCounter("user", "u", metricReq, windowMin, now)
	if !ok || uMin != 3 {
		t.Errorf("user req:min = %d, ok=%v, want 3", uMin, ok)
	}
	totalMin, ok := l.getCounter(totalScopeKind, totalScopeID, metricReq, windowMin, now)
	if !ok || totalMin != 3 {
		t.Errorf("total req:min = %d, ok=%v, want 3", totalMin, ok)
	}
	gDay, ok := l.getCounter("group", "g", metricReq, windowDay, now)
	if !ok || gDay != 3 {
		t.Errorf("group req:day = %d, ok=%v, want 3", gDay, ok)
	}
}

// TestAccount_OneIncrMultiCallRegardlessOfScopeOrMetricCount is the
// perf-review (2026-08-21) case for account: a 3-scope request with all
// three metrics nonzero (27 counters: 3 scopes x 3 metrics x 3 windows)
// must cost exactly ONE incrMulti round trip, and the batched reply must
// land every value in the right scope/metric/window slot.
func TestAccount_OneIncrMultiCallRegardlessOfScopeOrMetricCount(t *testing.T) {
	store := &countingIncrStore{}
	l := newLimiter(store, true)
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	l.nowFn = func() time.Time { return now }

	scopes := []limitScope{
		{kind: "user", id: "u"},
		{kind: "group", id: "g"},
		{kind: totalScopeKind, id: totalScopeID},
	}
	l.account(scopes, usage{prompt: 10, completion: 5}, 700)

	if store.incrMultiCalls != 1 {
		t.Errorf("incrMultiCalls = %d, want 1 (one batch for 27 counters)", store.incrMultiCalls)
	}

	uTokInDay, ok := l.getCounter("user", "u", metricTokIn, windowDay, now)
	if !ok || uTokInDay != 10 {
		t.Errorf("user tokin:day = %d, ok=%v, want 10", uTokInDay, ok)
	}
	totalTokOutHour, ok := l.getCounter(totalScopeKind, totalScopeID, metricTokOut, windowHour, now)
	if !ok || totalTokOutHour != 5 {
		t.Errorf("total tokout:hour = %d, ok=%v, want 5", totalTokOutHour, ok)
	}
	gCostMonth, ok := l.getCounter("group", "g", metricCost, windowMonth, now)
	if !ok || gCostMonth != 700 {
		t.Errorf("group cost:month = %d, ok=%v, want 700", gCostMonth, ok)
	}

	// A second call accumulates rather than overwrites, and still costs
	// exactly one more round trip.
	l.account(scopes, usage{prompt: 2}, 0)
	if store.incrMultiCalls != 2 {
		t.Errorf("incrMultiCalls after 2nd account = %d, want 2", store.incrMultiCalls)
	}
	uTokInDay, ok = l.getCounter("user", "u", metricTokIn, windowDay, now)
	if !ok || uTokInDay != 12 {
		t.Errorf("user tokin:day after 2nd account = %d, ok=%v, want 12", uTokInDay, ok)
	}
}

// TestTokenBudget_ExactBoundaryViolates asserts budgetViolation/
// tokenBudgetViolation's ">= limit" semantics at the exact boundary: a
// scope whose accumulated tokin+tokout lands EXACTLY on the limit (not
// merely over it) must still violate. budgetViolation's own "used <
// limit passes" check makes "used == limit" the smallest value that
// violates; this pins that boundary directly rather than only exercising
// values already over it (TestLimiterTokenBudget, TestTokenBudgetSumsInAndOut).
func TestTokenBudget_ExactBoundaryViolates(t *testing.T) {
	l := newLimiter(nil, true)
	scopes := []limitScope{{kind: "group", id: "g", limits: &LimitsConfig{TokensPerDay: 100}}}

	l.account(scopes, usage{prompt: 63, completion: 36}, 0) // 99 < 100: must not violate yet
	if v := l.checkAndCount(scopes); v != nil {
		t.Fatalf("99 < 100 must not violate, got %+v", v)
	}

	l.account(scopes, usage{completion: 1}, 0) // pushes tokout to 37: total now exactly 100
	v := l.checkAndCount(scopes)
	if v == nil {
		t.Fatal("tokin+tokout landing exactly on the 100 budget must violate (>= limit, not only > limit)")
	}
	if v.storeDown {
		t.Error("storeDown must be false for a real limit breach")
	}
}

// --- perf review round 3, 2026-08-22: fused admission round trip ---

// TestCheckAndCount_FusedViolationPrecedence is the fusion-equivalence
// case: checkAndCount's admission round trip now fuses the request-counter
// increments with every scope's token/cost budget reads into ONE store
// call (buildBudgetProbes, incrAndGetMulti), replacing the pre-fusion
// evaluateScope/budgetViolation/tokenBudgetViolation helpers that made
// those reads as separate, serial round trips. This table pins that the
// VIOLATION PRECEDENCE those helpers enforced is byte-for-byte unchanged:
// within one scope, requests-per-minute, then requests-per-day, then
// tokens-per-day, then tokens-per-month, then cost-per-day, then
// cost-per-month; across scopes, the scope earlier in the given slice
// (buildLimitScopes puts a user before their group) wins. Every subtest
// engineers TWO simultaneously-true violation conditions and asserts only
// the higher-precedence one is ever reported.
func TestCheckAndCount_FusedViolationPrecedence(t *testing.T) {
	t.Run("requests-per-minute beats requests-per-day in the same scope", func(t *testing.T) {
		l := newLimiter(nil, true)
		scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerMinute: 1, RequestsPerDay: 1}}}
		if v := l.checkAndCount(scopes); v != nil {
			t.Fatalf("1st call must pass (both counters land exactly on their limit), got %+v", v)
		}
		v := l.checkAndCount(scopes)
		if v == nil {
			t.Fatal("2nd call must violate: both requests-per-minute and requests-per-day are now over limit")
		}
		assert.Contains(t, v.message, "requests-per-minute")
		assert.NotContains(t, v.message, "requests-per-day")
	})

	t.Run("requests-per-day beats tokens-per-day in the same scope", func(t *testing.T) {
		l := newLimiter(nil, true)
		scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerDay: 1, TokensPerDay: 10}}}
		if v := l.checkAndCount(scopes); v != nil {
			t.Fatalf("1st call must pass (req:day lands exactly on its limit, no tokens seeded yet), got %+v", v)
		}
		l.account(scopes, usage{prompt: 20}, 0) // tokin:day now 20, over the 10 budget
		v := l.checkAndCount(scopes)
		if v == nil {
			t.Fatal("2nd call must violate: both requests-per-day and tokens-per-day are now over limit")
		}
		assert.Contains(t, v.message, "requests-per-day")
		assert.NotContains(t, v.message, "tokens-per-day")
	})

	t.Run("tokens-per-day beats tokens-per-month in the same scope", func(t *testing.T) {
		l := newLimiter(nil, true)
		scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{TokensPerDay: 10, TokensPerMonth: 10}}}
		l.account(scopes, usage{prompt: 20}, 0) // tokin:day and tokin:month both now 20, both over budget
		v := l.checkAndCount(scopes)
		if v == nil {
			t.Fatal("must violate: both tokens-per-day and tokens-per-month are over limit")
		}
		assert.Contains(t, v.message, "tokens-per-day")
		assert.NotContains(t, v.message, "tokens-per-month")
	})

	t.Run("tokens-per-month beats cost-per-day in the same scope", func(t *testing.T) {
		l := newLimiter(nil, true)
		scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{TokensPerMonth: 10, CostPerDayUSD: 0.0001}}}
		l.account(scopes, usage{prompt: 20}, 200) // tokin:month = 20 (>10); cost:day = 200 micros (>100 micros)
		v := l.checkAndCount(scopes)
		if v == nil {
			t.Fatal("must violate: both tokens-per-month and cost-per-day are over limit")
		}
		assert.Contains(t, v.message, "tokens-per-month")
		assert.NotContains(t, v.message, "cost-per-day")
	})

	t.Run("cost-per-day beats cost-per-month in the same scope", func(t *testing.T) {
		l := newLimiter(nil, true)
		scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{CostPerDayUSD: 0.0001, CostPerMonthUSD: 0.0001}}}
		l.account(scopes, usage{}, 200) // cost:day and cost:month both now 200 micros, both over the 100-micro budget
		v := l.checkAndCount(scopes)
		if v == nil {
			t.Fatal("must violate: both cost-per-day and cost-per-month are over limit")
		}
		assert.Contains(t, v.message, "cost-per-day")
		assert.NotContains(t, v.message, "cost-per-month")
	})

	t.Run("user scope beats group scope when both violate", func(t *testing.T) {
		l := newLimiter(nil, true)
		scopes := []limitScope{
			{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerMinute: 1}},
			{kind: "group", id: "g", limits: &LimitsConfig{RequestsPerMinute: 1}},
		}
		if v := l.checkAndCount(scopes); v != nil {
			t.Fatalf("1st call must pass, got %+v", v)
		}
		v := l.checkAndCount(scopes)
		if v == nil {
			t.Fatal("2nd call must violate: both user and group are now over their requests-per-minute limit")
		}
		assert.Contains(t, v.message, `user "u"`)
	})

	t.Run("group scope evaluated when the earlier user scope has no violation", func(t *testing.T) {
		l := newLimiter(nil, true)
		scopes := []limitScope{
			{kind: "user", id: "u", limits: nil},
			{kind: "group", id: "g", limits: &LimitsConfig{RequestsPerMinute: 1}},
		}
		if v := l.checkAndCount(scopes); v != nil {
			t.Fatalf("1st call must pass, got %+v", v)
		}
		v := l.checkAndCount(scopes)
		if v == nil {
			t.Fatal("2nd call must violate: group is now over its requests-per-minute limit")
		}
		assert.Contains(t, v.message, `group "g"`)
	})
}

// TestCheckAndCount_FusedRoundTrip_MemoryFallback proves the fused
// admission round trip works correctly end to end against memoryStore —
// the limiter's ALWAYS-available in-process fallback (newLimiter(nil, _)
// uses it directly; a configured store also falls back to it on error or
// while store-latched) — covering both an increment-only scope and a
// scope with every budget type configured at once, in one round trip.
func TestCheckAndCount_FusedRoundTrip_MemoryFallback(t *testing.T) {
	l := newLimiter(nil, true)
	now := time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC)
	l.nowFn = func() time.Time { return now }

	scopes := []limitScope{
		{kind: "user", id: "u", limits: &LimitsConfig{
			RequestsPerMinute: 100, RequestsPerDay: 100,
			TokensPerDay: 1000, TokensPerMonth: 1000,
			CostPerDayUSD: 1, CostPerMonthUSD: 1,
		}},
		{kind: totalScopeKind, id: totalScopeID, limits: nil},
	}

	// Well under every budget: must pass and must have actually
	// incremented every request counter (proving the fused write side
	// still lands correctly, not just that no violation was reported).
	if v := l.checkAndCount(scopes); v != nil {
		t.Fatalf("want no violation comfortably under every limit, got %+v", v)
	}
	uMin, ok := l.getCounter("user", "u", metricReq, windowMin, now)
	if !ok || uMin != 1 {
		t.Errorf("user req:min = %d, ok=%v, want 1", uMin, ok)
	}
	totalMin, ok := l.getCounter(totalScopeKind, totalScopeID, metricReq, windowMin, now)
	if !ok || totalMin != 1 {
		t.Errorf("total req:min = %d, ok=%v, want 1 (the unlimited total scope is still counted)", totalMin, ok)
	}

	// Push tokens over budget via account, then confirm the SAME fused
	// round trip catches it on the very next call.
	l.account(scopes, usage{prompt: 2000}, 0)
	v := l.checkAndCount(scopes)
	if v == nil {
		t.Fatal("want a tokens-per-day violation now that usage exceeds the 1000 budget")
	}
	if v.storeDown {
		t.Error("v.storeDown must be false — this is a real limit breach, not a store failure")
	}
	assert.Contains(t, v.message, "tokens-per-day")
}

// --- review round 2, 2026-08-21: enforcement-aware fallback TTL clamp ---

// TestClampTTL_ExactMathTable pins clampTTL's ceiling-vs-floor precedence
// for every window's real (ttl, enforceTTL) pair. Round 1's clamp applied
// memoryStoreMaxTTL unconditionally; round 2 corrects it so a window's
// own enforceTTL floor always wins a conflict with that ceiling — this is
// the exact math that keeps min/hour/day capped at 48h while month lands
// back at its own ~32-day floor.
func TestClampTTL_ExactMathTable(t *testing.T) {
	cases := []struct {
		name       string
		ttl        time.Duration
		enforceTTL time.Duration
		want       time.Duration
	}{
		{"min: 2m sits under both ceiling and floor, unchanged", minWindowTTL, enforceTTLFor(windowMin), minWindowTTL},
		{"hour: 48h ttl equals the 48h ceiling; its own 2h floor never applies", hourWindowTTL, enforceTTLFor(windowHour), memoryStoreMaxTTL},
		{"day: 35d ttl clamped down to the 48h ceiling, still above the 25h floor", dayWindowTTL, enforceTTLFor(windowDay), memoryStoreMaxTTL},
		{"month: 400d ttl would clamp to 48h, but the 32d floor wins instead", monthWindowTTL, enforceTTLFor(windowMonth), enforceTTLFor(windowMonth)},
		{"enforceTTL=0 (incrBy's own single-key path): pure ceiling, no floor", 400 * 24 * time.Hour, 0, memoryStoreMaxTTL},
		{"ttl already under both ceiling and floor passes through unchanged", 90 * time.Minute, time.Hour, 90 * time.Minute},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampTTL(c.ttl, c.enforceTTL); got != c.want {
				t.Errorf("clampTTL(%v, %v) = %v, want %v", c.ttl, c.enforceTTL, got, c.want)
			}
		})
	}
}

// TestEnforceTTLFor_MatchesTheOriginalPreHistoryBumpValues pins
// enforceTTLFor's four values directly — they are not arbitrary, they
// match dayWindowTTL/monthWindowTTL's values from before the v0.2
// data-layer task's history-retention bump (25h/32d), which were
// themselves already windowLength+margin for correct single-window
// enforcement.
func TestEnforceTTLFor_MatchesTheOriginalPreHistoryBumpValues(t *testing.T) {
	cases := []struct {
		window string
		want   time.Duration
	}{
		{windowMin, 2 * time.Minute},
		{windowHour, 2 * time.Hour},
		{windowDay, 25 * time.Hour},
		{windowMonth, 32 * 24 * time.Hour},
	}
	for _, c := range cases {
		if got := enforceTTLFor(c.window); got != c.want {
			t.Errorf("enforceTTLFor(%q) = %v, want %v", c.window, got, c.want)
		}
	}
}

// TestMemoryStore_IncrMulti_MonthCounterSurvivesPast48hAndStillEnforces
// is the review-round-2 regression case: a month counter written via
// incrMulti — the real production path, checkAndCount/account — must
// stay alive, not silently reset to 0, well past 48h, and a
// TokensPerMonth limit must still enforce correctly at that point. This
// is exactly what round 1's unconditional 48h ceiling broke: a month
// counter's key would have expired and reset roughly every two days,
// turning the budget into a rolling ~48h one instead of a true
// calendar-month one whenever Redis is absent or down.
func TestMemoryStore_IncrMulti_MonthCounterSurvivesPast48hAndStillEnforces(t *testing.T) {
	l := newLimiter(nil, true) // nil store -> every operation uses the in-process fallback
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "group", id: "g", limits: &LimitsConfig{TokensPerMonth: 100}}}

	l.account(scopes, usage{prompt: 60, completion: 20}, 0) // 80 tokens on day 1

	// Advance 5 days: well past memoryStoreMaxTTL's 48h ceiling, far short
	// of monthWindowTTL's own natural rollover.
	now = now.Add(5 * 24 * time.Hour)
	l.nowFn = func() time.Time { return now }

	tokIn, ok := l.getCounter("group", "g", metricTokIn, windowMonth, now)
	if !ok || tokIn != 60 {
		t.Fatalf("tokin:month after 5 days = %d, ok=%v, want 60 (the key must not have expired at 48h)", tokIn, ok)
	}

	// Push the total over budget and confirm enforcement still fires —
	// proves the surviving counter is actually read by
	// tokenBudgetViolation, not merely present.
	l.account(scopes, usage{completion: 21}, 0) // 60 + 41 = 101 > 100
	if v := l.checkAndCount(scopes); v == nil {
		t.Fatal("want a TokensPerMonth violation 5 days in; the round-1 clamp bug would have reset the counter well before this")
	}
}

// TestMemoryStore_IncrMulti_DayCounterStillClampsAt48h asserts the
// round-1 clamp's original goal — bounding the fallback's own live-key
// growth — still holds for day, whose own enforcement floor (25h) sits
// below the 48h ceiling: a day counter written via incrMulti expires and
// resets once its key crosses 48h, well before dayWindowTTL's own 35-day
// retention would otherwise have kept it alive.
func TestMemoryStore_IncrMulti_DayCounterStillClampsAt48h(t *testing.T) {
	m := newMemoryStore()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	m.nowFn = func() time.Time { return now }

	entry := counterIncr{key: "k", delta: 10, ttl: dayWindowTTL, enforceTTL: enforceTTLFor(windowDay)}
	if _, err := m.incrMulti([]counterIncr{entry}); err != nil {
		t.Fatal(err)
	}

	now = now.Add(49 * time.Hour) // just past the 48h ceiling
	m.nowFn = func() time.Time { return now }

	got, err := m.get("k")
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Errorf("day counter at 49h past write = %d, want 0 (expired at the 48h ceiling; day's own 25h floor never raises it)", got)
	}
}

// --- Feature A (v0.22): per-provider/per-(provider,model) success-rate accounting ---

// newSyncLimiter builds a limiter exactly like newLimiter, but with spawn
// overridden to run synchronously instead of in its own goroutine — every
// recordProviderAttempt test in this file needs its store write to have
// already landed by the time it reads the counter back. Production
// spawns a real goroutine there (SHOULD-5 ruling, v0.22 review round: off
// a streaming response's TTFB path); this is the deterministic-test half
// of that same dependency-injection field, matching nowFn/waitFn's
// existing pattern elsewhere in this package.
func newSyncLimiter(store counterStore, failOpen bool) *limiter {
	l := newLimiter(store, failOpen)
	l.spawn = func(f func()) { f() }
	return l
}

// TestRecordProviderAttempt_ClassifiesUsingIsTransient proves
// recordProviderAttempt's failure classification is exactly isTransient's
// own — reused, not forked (spec ruling) — across the full outcome table:
// success, a non-429 4xx (still a "success" for provider-health purposes:
// the provider answered correctly to a request it did not like), 429, a
// 5xx, a plain network error, and a context-canceled error (not a
// provider fault, per isTransient's own carve-out) — PLUS the SHOULD-1
// addition (v0.22 review round): a context.DeadlineExceeded error (a
// hung upstream the gateway's own deadline finally cut off) counts as a
// failure even though isTransient itself excludes it, both bare and
// wrapped (proving isDeadlineExceeded's manual Unwrap walk actually
// walks) — while context.Canceled, wrapped the identical way, still does
// not.
func TestRecordProviderAttempt_ClassifiesUsingIsTransient(t *testing.T) {
	tests := []struct {
		err      error
		resp     *http.Response
		name     string
		wantFail bool
	}{
		{name: "200 OK", resp: &http.Response{StatusCode: http.StatusOK}, wantFail: false},
		{name: "404 not found (provider answered)", resp: &http.Response{StatusCode: http.StatusNotFound}, wantFail: false},
		{name: "429 too many requests", resp: &http.Response{StatusCode: http.StatusTooManyRequests}, wantFail: true},
		{name: "500 internal server error", resp: &http.Response{StatusCode: http.StatusInternalServerError}, wantFail: true},
		{name: "503 service unavailable", resp: &http.Response{StatusCode: http.StatusServiceUnavailable}, wantFail: true},
		{name: "network error", err: errors.New("dial tcp: connection refused"), wantFail: true},
		{name: "context canceled (not a provider fault)", err: context.Canceled, wantFail: false},
		{name: "context deadline exceeded (hung upstream timed out — SHOULD-1)", err: context.DeadlineExceeded, wantFail: true},
		{name: "wrapped context deadline exceeded (proves errors.Is walks the chain)", err: fmt.Errorf("%w: dial timeout", context.DeadlineExceeded), wantFail: true},
		{name: "wrapped context canceled stays non-failure too", err: fmt.Errorf("%w: client hung up", context.Canceled), wantFail: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newSyncLimiter(nil, true)
			now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
			l.nowFn = func() time.Time { return now }

			l.recordProviderAttempt("openai", "gpt-4o", tt.resp, tt.err)

			attempts, _ := l.getCounter(kindProvider, "openai", metricProvAttempt, windowDay, now)
			if attempts != 1 {
				t.Errorf("provider attempts/day = %d, want 1", attempts)
			}
			fails, _ := l.getCounter(kindProvider, "openai", metricProvFail, windowDay, now)
			wantFails := int64(0)
			if tt.wantFail {
				wantFails = 1
			}
			if fails != wantFails {
				t.Errorf("provider fails/day = %d, want %d", fails, wantFails)
			}

			// Minute window and the (provider, model) scope both mirror the
			// provider/day counters exactly.
			attemptsMin, _ := l.getCounter(kindProvider, "openai", metricProvAttempt, windowMin, now)
			if attemptsMin != 1 {
				t.Errorf("provider attempts/min = %d, want 1", attemptsMin)
			}
			modelAttempts, _ := l.getCounter(kindProviderModel, "openai/gpt-4o", metricProvAttempt, windowDay, now)
			if modelAttempts != 1 {
				t.Errorf("model attempts/day = %d, want 1", modelAttempts)
			}
			modelFails, _ := l.getCounter(kindProviderModel, "openai/gpt-4o", metricProvFail, windowDay, now)
			if modelFails != wantFails {
				t.Errorf("model fails/day = %d, want %d", modelFails, wantFails)
			}
		})
	}
}

// TestRecordProviderAttempt_EmptyModel_ProviderScopeOnly proves a caller
// that cannot cheaply know the upstream model before the attempt resolves
// (native passthrough, routes_passthrough.go's handlePassthrough: the
// model lives in the response body, read only afterward) gets
// provider-level accounting only when it passes model="".
func TestRecordProviderAttempt_EmptyModel_ProviderScopeOnly(t *testing.T) {
	l := newSyncLimiter(nil, true)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	l.nowFn = func() time.Time { return now }

	l.recordProviderAttempt("openai", "", &http.Response{StatusCode: http.StatusInternalServerError}, nil)

	attempts, _ := l.getCounter(kindProvider, "openai", metricProvAttempt, windowDay, now)
	if attempts != 1 {
		t.Errorf("provider attempts/day = %d, want 1", attempts)
	}
	fails, _ := l.getCounter(kindProvider, "openai", metricProvFail, windowDay, now)
	if fails != 1 {
		t.Errorf("provider fails/day = %d, want 1", fails)
	}
}

// TestRecordProviderAttempt_EmptyProvider_NoOp proves a caller with no
// resolved provider name (should never happen in production traffic, but
// must never panic) is a silent no-op.
func TestRecordProviderAttempt_EmptyProvider_NoOp(t *testing.T) {
	l := newSyncLimiter(nil, true)
	l.recordProviderAttempt("", "some-model", &http.Response{StatusCode: http.StatusOK}, nil) // must not panic
}

// TestRecordProviderAttempt_ProductionSpawn_WriteEventuallyLandsAsync
// leaves limiter.spawn at its real production default (newLimiter's own
// bounded, goroutine-spawning closure) instead of overriding it to run
// synchronously like every other recordProviderAttempt test in this file
// — FOLDED-2 (v0.22 review round, round 2): every other test here proves
// the WRITE is correct, but only by bypassing the actual async code path
// (limiter.spawn's own doc comment) entirely; this is the one test that
// exercises it for real, closing a real -race async-path gap. Polls
// (waitUntil, registry_test.go) rather than sleeping a fixed duration —
// a fixed sleep is either flaky (too short, especially under `-race`,
// which distorts goroutine scheduling) or wastefully slow (too long); a
// deadline-bounded poll is neither.
func TestRecordProviderAttempt_ProductionSpawn_WriteEventuallyLandsAsync(t *testing.T) {
	l := newLimiter(nil, true) // production default spawn — deliberately NOT newSyncLimiter
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	l.nowFn = func() time.Time { return now }

	l.recordProviderAttempt("openai", "gpt-4o", &http.Response{StatusCode: http.StatusOK}, nil)

	waitUntil(t, time.Second, func() bool {
		attempts, _ := l.getCounter(kindProvider, "openai", metricProvAttempt, windowDay, now)
		return attempts == 1
	})
	fails, _ := l.getCounter(kindProvider, "openai", metricProvFail, windowDay, now)
	if fails != 0 {
		t.Errorf("fails/day = %d, want 0 (a 200 response is never a failure)", fails)
	}
}

// TestIsDeadlineExceeded_Classification exercises isDeadlineExceeded's
// observable classification across every wrap shape that matters: bare,
// wrapped once, wrapped twice, an unrelated sentinel (context.Canceled,
// which must never match), an unrelated wrapped error, a plain error
// with no Unwrap method — PLUS the prod-shape double-%w/triple-%w rows a
// review round-2 blocker demanded, byte-for-byte the exact format
// strings providers.go's three upstreamJSON/upstreamBytes call sites use.
//
// A load-bearing caveat this test's own name used to hide (it was
// TestIsDeadlineExceeded_YaegiSafeUnwrap): every case here runs COMPILED,
// under `go test`, and compiled-Go correctness is not the same claim as
// Yaegi-interpreted correctness. Round 2's original fix — a hand-rolled
// Unwrap walk against two locally declared interfaces, matched with bare
// comma-ok type assertions — passed every one of these exact rows
// (including the double-/triple-%w ones) under `go test`, while
// SHOULD-A's yaegi-check harness (tools/yaegi-check/main.go's
// exerciseAttemptAccounting) proved it silently returned false for the
// double-%w case under the REAL interpreter: an interpreted interface
// type does not correctly match a compiled concrete value's method set
// in Yaegi, the reverse direction of the already-documented errors.As-
// on-interpreted-types trap. matchesSentinel now calls real errors.Is
// instead (limits.go's own doc comment there has the full account of why
// that is safe here specifically, unlike errors.As on an interpreted
// type) — this table still pins the OBSERVABLE behavior, but
// tools/yaegi-check is what actually proves it under the interpreter;
// this table alone would not have caught the round-2 regression, and a
// future change here must keep running yaegi-check, not just `go test`.
func TestIsDeadlineExceeded_Classification(t *testing.T) {
	tests := []struct {
		err  error
		name string
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "bare sentinel", err: context.DeadlineExceeded, want: true},
		{name: "wrapped once", err: fmt.Errorf("upstream: %w", context.DeadlineExceeded), want: true},
		{name: "wrapped twice", err: fmt.Errorf("outer: %w", fmt.Errorf("upstream: %w", context.DeadlineExceeded)), want: true},
		{name: "unrelated sentinel (context.Canceled)", err: context.Canceled, want: false},
		{name: "unrelated wrapped error", err: fmt.Errorf("dial: %w", errors.New("connection refused")), want: false},
		{name: "plain error, no Unwrap method", err: errors.New("boom"), want: false},
		{
			name: "double-%w, upstreamBytes' client.Do shape — the blocker's own reproduction",
			err:  fmt.Errorf("%w: %w", errUpstream, context.DeadlineExceeded),
			want: true,
		},
		{
			name: "double-%w, upstreamJSON's encode-failure shape",
			err:  fmt.Errorf("%w: encode request body: %w", errUpstream, context.DeadlineExceeded),
			want: true,
		},
		{
			name: "triple-%w, upstreamBytes' build-request shape",
			err:  fmt.Errorf("%w: %w: build request: %w", errUpstream, errRequestBuildFailed, context.DeadlineExceeded),
			want: true,
		},
		{
			name: "double-%w wrapping context.Canceled must still NOT match — multi-wrap does not blur the two sentinels apart",
			err:  fmt.Errorf("%w: %w", errUpstream, context.Canceled),
			want: false,
		},
		{
			name: "double-%w wrapping an unrelated error must still NOT match",
			err:  fmt.Errorf("%w: %w", errUpstream, errors.New("connection refused")),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDeadlineExceeded(tt.err); got != tt.want {
				t.Errorf("isDeadlineExceeded(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestProviderModelScopeID proves the "provider/model" id convention
// providerUsage/recordProviderAttempt share with pricing.go's own
// unifiedCostMicros and the admin API.
func TestProviderModelScopeID(t *testing.T) {
	if got := providerModelScopeID("openai", "gpt-4o"); got != "openai/gpt-4o" {
		t.Errorf("providerModelScopeID = %q, want %q", got, "openai/gpt-4o")
	}
}

// TestProviderUsage_BatchedSingleRoundTrip drives limiter.providerUsage
// against a counting stub store (historyCountingStore, shared with
// TestLimiterHistory_OneBatchCallOldestFirst above): it must issue
// exactly ONE getMulti call for both a provider-level and a
// (provider, model) scope together, and slice the flat result back to the
// right scope in order — including SHOULD-2's variable stride (v0.22
// review round): the provider scope reads its minute window too, the
// model scope does not, so a stray seeded minute-window key under the
// model scope's own id must never leak into its result — providerUsage
// never even asks the store for it.
func TestProviderUsage_BatchedSingleRoundTrip(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	countStore := &historyCountingStore{values: map[string]int64{
		windowKey(kindProvider, "openai", metricProvAttempt, windowMin, now):             3,
		windowKey(kindProvider, "openai", metricProvFail, windowMin, now):                1,
		windowKey(kindProvider, "openai", metricProvAttempt, windowDay, now):             30,
		windowKey(kindProvider, "openai", metricProvFail, windowDay, now):                2,
		windowKey(kindProviderModel, "openai/gpt-4o", metricProvAttempt, windowDay, now): 10,
		// Seeded but must never be read: providerCounterKeys omits a
		// kindProviderModel scope's minute-window keys entirely (SHOULD-2).
		// A bug that started reading them again would make this key's
		// value leak into got[1].attemptsMinute below, catching the
		// regression even though allKeys's own length is never asserted
		// directly.
		windowKey(kindProviderModel, "openai/gpt-4o", metricProvAttempt, windowMin, now): 999,
	}}
	l := newLimiter(countStore, true)
	l.nowFn = func() time.Time { return now }

	scopes := []limitScope{
		{kind: kindProvider, id: "openai"},
		{kind: kindProviderModel, id: "openai/gpt-4o"},
	}
	got := l.providerUsage(scopes)

	if countStore.getMultiCalls != 1 {
		t.Errorf("getMultiCalls = %d, want 1 (both scopes' keys flattened into one call)", countStore.getMultiCalls)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].attemptsMinute != 3 || got[0].failuresMinute != 1 || got[0].attemptsDay != 30 || got[0].failuresDay != 2 {
		t.Errorf("provider counters = %+v, want {3 1 30 2}", got[0])
	}
	if got[1].attemptsDay != 10 || got[1].failuresDay != 0 {
		t.Errorf("model counters = %+v, want attemptsDay 10, failuresDay 0", got[1])
	}
	if got[1].attemptsMinute != 0 || got[1].failuresMinute != 0 {
		t.Errorf("model minute counters = %+v, want the zero value (never read for a model scope — SHOULD-2)", got[1])
	}
}

// TestProviderUsage_OffsetSlicing_ScopeOrderIndependent proves the
// running-offset slicing SHOULD-2 introduced (providerUsage no longer
// assumes a single fixed stride) is correct regardless of scope order —
// a model scope (2 keys) followed by TWO provider scopes (4 keys each)
// followed by another model scope, deliberately not the "providers then
// models" order buildAdminOverview itself always builds, to prove the
// slicing logic itself does not silently depend on that convention.
func TestProviderUsage_OffsetSlicing_ScopeOrderIndependent(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	countStore := &historyCountingStore{values: map[string]int64{
		windowKey(kindProviderModel, "a/m1", metricProvAttempt, windowDay, now): 1,
		windowKey(kindProviderModel, "a/m1", metricProvFail, windowDay, now):    0,
		windowKey(kindProvider, "a", metricProvAttempt, windowMin, now):         2,
		windowKey(kindProvider, "a", metricProvFail, windowMin, now):            0,
		windowKey(kindProvider, "a", metricProvAttempt, windowDay, now):         20,
		windowKey(kindProvider, "a", metricProvFail, windowDay, now):            1,
		windowKey(kindProvider, "b", metricProvAttempt, windowMin, now):         3,
		windowKey(kindProvider, "b", metricProvFail, windowMin, now):            1,
		windowKey(kindProvider, "b", metricProvAttempt, windowDay, now):         30,
		windowKey(kindProvider, "b", metricProvFail, windowDay, now):            2,
		windowKey(kindProviderModel, "b/m2", metricProvAttempt, windowDay, now): 4,
		windowKey(kindProviderModel, "b/m2", metricProvFail, windowDay, now):    4,
	}}
	l := newLimiter(countStore, true)
	l.nowFn = func() time.Time { return now }

	scopes := []limitScope{
		{kind: kindProviderModel, id: "a/m1"},
		{kind: kindProvider, id: "a"},
		{kind: kindProvider, id: "b"},
		{kind: kindProviderModel, id: "b/m2"},
	}
	got := l.providerUsage(scopes)

	if len(got) != 4 {
		t.Fatalf("len(got) = %d, want 4", len(got))
	}
	if got[0].attemptsDay != 1 || got[0].failuresDay != 0 {
		t.Errorf("scope[0] (model a/m1) = %+v, want attemptsDay 1, failuresDay 0", got[0])
	}
	if got[1].attemptsMinute != 2 || got[1].attemptsDay != 20 || got[1].failuresDay != 1 {
		t.Errorf("scope[1] (provider a) = %+v, want attemptsMinute 2, attemptsDay 20, failuresDay 1", got[1])
	}
	if got[2].attemptsMinute != 3 || got[2].failuresMinute != 1 || got[2].attemptsDay != 30 || got[2].failuresDay != 2 {
		t.Errorf("scope[2] (provider b) = %+v, want {3 1 30 2}", got[2])
	}
	if got[3].attemptsDay != 4 || got[3].failuresDay != 4 {
		t.Errorf("scope[3] (model b/m2) = %+v, want attemptsDay 4, failuresDay 4", got[3])
	}
}

// TestProviderUsage_EmptyScopes_ReturnsEmptyWithoutTouchingStore mirrors
// targetUsage's own empty-input short circuit.
func TestProviderUsage_EmptyScopes_ReturnsEmptyWithoutTouchingStore(t *testing.T) {
	countStore := &historyCountingStore{}
	l := newLimiter(countStore, true)

	got := l.providerUsage(nil)
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
	if countStore.getMultiCalls != 0 {
		t.Errorf("getMultiCalls = %d, want 0 (empty scopes must not touch the store)", countStore.getMultiCalls)
	}
}

// TestProviderUsage_StoreDown_ReturnsZeroCounters mirrors targetUsage's
// own fail-closed contract: a store error with failOpen=false must report
// every scope's counters as zero, not partial or stale values.
func TestProviderUsage_StoreDown_ReturnsZeroCounters(t *testing.T) {
	l := newLimiter(alwaysErrStore{}, false)
	got := l.providerUsage([]limitScope{{kind: kindProvider, id: "openai"}})
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0] != (providerCounters{}) {
		t.Errorf("counters = %+v, want the zero value on a storeDown read", got[0])
	}
}
