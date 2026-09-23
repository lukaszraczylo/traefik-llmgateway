package traefikllmgateway

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- histogramQuantile ---

func TestHistogramQuantile_NoObservationsReturnsZeroNotOverflow(t *testing.T) {
	t.Parallel()
	bounds := []float64{1, 2, 3}
	counts := []int64{0, 0, 0, 0} // 3 buckets + overflow, all empty
	ms, overflow := histogramQuantile(bounds, counts, 0.5)
	if ms != 0 || overflow {
		t.Errorf("histogramQuantile(empty) = (%v, %v), want (0, false)", ms, overflow)
	}
}

func TestHistogramQuantile_InterpolatesWithinBucket(t *testing.T) {
	t.Parallel()
	bounds := []float64{1, 2, 3}
	// Every observation lands in bucket 0 (0, 1] seconds.
	counts := []int64{10, 0, 0, 0}
	ms, overflow := histogramQuantile(bounds, counts, 0.5)
	if overflow {
		t.Fatal("overflow = true, want false")
	}
	want := 500.0 // midpoint of (0, 1]s in ms
	if math.Abs(ms-want) > 1e-9 {
		t.Errorf("histogramQuantile(bucket0, q=0.5) = %v ms, want %v", ms, want)
	}
}

func TestHistogramQuantile_SpansTwoBuckets(t *testing.T) {
	t.Parallel()
	bounds := []float64{1, 2, 3}
	// 5 observations in (0,1], 5 in (1,2]: the p50 rank (target=5) lands
	// exactly at the boundary between the two buckets.
	counts := []int64{5, 5, 0, 0}
	ms, overflow := histogramQuantile(bounds, counts, 0.5)
	if overflow {
		t.Fatal("overflow = true, want false")
	}
	if math.Abs(ms-1000) > 1e-9 {
		t.Errorf("histogramQuantile(boundary) = %v ms, want 1000", ms)
	}
	// p90: target=9, cum after bucket0=5, falls in bucket1 (1,2], frac=(9-5)/5=0.8
	p90, overflow := histogramQuantile(bounds, counts, 0.9)
	if overflow {
		t.Fatal("overflow = true, want false")
	}
	want := (1 + 0.8*(2-1)) * 1000
	if math.Abs(p90-want) > 1e-9 {
		t.Errorf("histogramQuantile(q=0.9) = %v ms, want %v", p90, want)
	}
}

func TestHistogramQuantile_OverflowBucket(t *testing.T) {
	t.Parallel()
	bounds := []float64{1, 2, 3}
	// Every observation is past the largest bound.
	counts := []int64{0, 0, 0, 7}
	ms, overflow := histogramQuantile(bounds, counts, 0.99)
	if !overflow {
		t.Fatal("overflow = false, want true")
	}
	if ms != 3000 {
		t.Errorf("histogramQuantile(overflow) = %v ms, want 3000 (largest bound in ms)", ms)
	}
}

func TestHistogramQuantile_RealLatencyBounds(t *testing.T) {
	t.Parallel()
	n := len(latencyBucketBounds) + 1
	counts := make([]int64, n)
	counts[3] = 1 // latencyBucketBounds[3] == 1 (index for the 1s bound)
	ms, overflow := histogramQuantile(latencyBucketBounds, counts, 0.5)
	if overflow {
		t.Fatal("overflow = true, want false")
	}
	// Single observation in bucket (0.5, 1]s: midpoint interpolation.
	wantLow, wantHigh := 500.0, 1000.0
	if ms < wantLow || ms > wantHigh {
		t.Errorf("histogramQuantile(real bounds) = %v ms, want in [%v, %v]", ms, wantLow, wantHigh)
	}
}

// --- HTTP endpoint ---

func TestAdminPerformance_GateMatrix(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	path := adminPerformancePath + "?kind=provider&window=day"
	cases := []struct {
		name   string
		apiKey string
		want   int
	}{
		{"unauthenticated", "", http.StatusUnauthorized},
		{"non-admin", "sk-alice", http.StatusForbidden},
		{"admin", "sk-admin1", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, path, c.apiKey))
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d, body=%s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestAdminPerformance_ParamValidation(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	cases := []struct {
		name string
		qs   string
		want int
	}{
		{"unknown kind", "?kind=bogus&window=day", http.StatusBadRequest},
		{"month window rejected", "?kind=provider&window=month", http.StatusBadRequest},
		{"invalid series", "?kind=provider&window=day&series=2", http.StatusBadRequest},
		{"series without exactly one id", "?kind=provider&window=day&series=1&id=alpha&id=zeta", http.StatusBadRequest},
		{"series with zero ids", "?kind=provider&window=day&series=1", http.StatusBadRequest},
		// P1 fix (admin dashboard redesign verify round): exactly one raw
		// id given (satisfying the len(rawIDs)!=1 check above) that does
		// not resolve to a real provider/model — ids ends up empty, and
		// ids[0] used to panic (recovered as a 500 by errors.go, but still
		// a logged panic) instead of this 400.
		{"series with unknown provider id", "?kind=provider&window=day&series=1&id=nope", http.StatusBadRequest},
		{"series with unknown model id", "?kind=model&window=day&series=1&id=nope/x", http.StatusBadRequest},
		{"valid provider", "?kind=provider&window=day", http.StatusOK},
		{"valid model", "?kind=model&window=hour", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, adminPerformancePath+c.qs, "sk-admin1"))
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d, body=%s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestAdminPerformance_TooManyIDsIs400(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	q := "?kind=provider&window=day"
	for i := 0; i <= adminPerfMaxIDs; i++ {
		q += "&id=alpha"
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminPerformancePath+q, "sk-admin1"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (over adminPerfMaxIDs)", rec.Code)
	}
}

// TestAdminPerformance_AttemptsAlwaysLatencyGated seeds provider attempt/
// fail counters (always-on) and latency buckets (opt-in), then confirms:
// with stats.latency OFF, Attempts/Failures are populated but every
// latency field stays zero/nil and LatencyEnabled is false; with it ON,
// the percentile fields populate too.
func TestAdminPerformance_AttemptsAlwaysLatencyGated(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	store := &countingMultiStore{values: map[string]int64{
		windowKey(kindProvider, "alpha", metricProvAttempt, windowDay, fixedNow):        10,
		windowKey(kindProvider, "alpha", metricProvFail, windowDay, fixedNow):           2,
		windowKey(kindProvider, "alpha", latencyDurationMetric(3), windowDay, fixedNow): 4, // bucket for bound index 3
	}}

	// Latency OFF (default).
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.limiter = newLimiter(store, true)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminPerformancePath+"?kind=provider&window=day&id=alpha", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminPerfResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LatencyEnabled {
		t.Error("LatencyEnabled = true, want false (stats.latency off)")
	}
	if len(got.Rows) != 1 {
		t.Fatalf("Rows = %+v, want exactly 1", got.Rows)
	}
	row := got.Rows[0]
	if row.Attempts != 10 || row.Failures != 2 {
		t.Errorf("Attempts/Failures = %d/%d, want 10/2 (always-on)", row.Attempts, row.Failures)
	}
	if row.P50Ms != nil || row.Count != 0 {
		t.Errorf("P50Ms/Count = %v/%d, want nil/0 with latency stats off", row.P50Ms, row.Count)
	}

	// Latency ON.
	cfg2 := newAdminTestConfig()
	cfg2.Admin.Stats = &AdminStatsConfig{Latency: true}
	h2, gw2 := newAdminGatewayHandle(t, cfg2)
	gw2.limiter = newLimiter(store, true)
	gw2.limiter.nowFn = func() time.Time { return fixedNow }

	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, adminRequest(http.MethodGet, adminPerformancePath+"?kind=provider&window=day&id=alpha", "sk-admin1"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec2.Code, rec2.Body.String())
	}
	var got2 adminPerfResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &got2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got2.LatencyEnabled {
		t.Error("LatencyEnabled = false, want true (stats.latency on)")
	}
	if len(got2.Rows) != 1 {
		t.Fatalf("Rows = %+v, want exactly 1", got2.Rows)
	}
	row2 := got2.Rows[0]
	if row2.Count != 4 {
		t.Errorf("Count = %d, want 4", row2.Count)
	}
	if row2.P50Ms == nil {
		t.Error("P50Ms = nil, want populated with latency observations present")
	}
}

// TestAdminPerformance_ModelKindHasNoTimeoutsFailovers pins that
// kind=model rows never populate Timeouts/Failovers (plan §1.2: no
// per-model scope tracks either).
func TestAdminPerformance_ModelKindHasNoTimeoutsFailovers(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	store := &countingMultiStore{values: map[string]int64{
		windowKey(kindProviderModel, "alpha/a-model-1", metricProvAttempt, windowDay, fixedNow): 3,
	}}
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.limiter = newLimiter(store, true)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminPerformancePath+"?kind=model&window=day&id=alpha/a-model-1", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminPerfResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("Rows = %+v, want exactly 1", got.Rows)
	}
	if got.Rows[0].Timeouts != 0 || got.Rows[0].Failovers != 0 {
		t.Errorf("Timeouts/Failovers = %d/%d, want 0/0 for kind=model", got.Rows[0].Timeouts, got.Rows[0].Failovers)
	}
	if got.Rows[0].Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", got.Rows[0].Attempts)
	}
}

// TestAdminPerformance_DefaultProviderIDsAreAllConfigured pins the
// "provider default = all configured" rule (plan §1.3(vi)).
func TestAdminPerformance_DefaultProviderIDsAreAllConfigured(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminPerformancePath+"?kind=provider&window=day", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminPerfResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("Rows = %+v, want 2 (both configured providers)", got.Rows)
	}
}

// TestAdminPerformance_SeriesModeBucketPerPoint pins series=1's shape:
// Points has exactly `span` entries, one per bucket, ID set to the
// bucket label, Rows left empty.
func TestAdminPerformance_SeriesModeBucketPerPoint(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	yesterday := fixedNow.AddDate(0, 0, -1)
	store := &countingMultiStore{values: map[string]int64{
		windowKey(kindProvider, "alpha", metricProvAttempt, windowDay, fixedNow):  5,
		windowKey(kindProvider, "alpha", metricProvAttempt, windowDay, yesterday): 9,
	}}
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.limiter = newLimiter(store, true)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminPerformancePath+"?kind=provider&window=day&span=2&series=1&id=alpha", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminPerfResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rows) != 0 {
		t.Errorf("Rows = %+v, want empty in series mode", got.Rows)
	}
	if len(got.Points) != 2 || len(got.Buckets) != 2 {
		t.Fatalf("Points/Buckets = %d/%d, want 2/2", len(got.Points), len(got.Buckets))
	}
	if got.Points[0].ID != got.Buckets[0] || got.Points[1].ID != got.Buckets[1] {
		t.Errorf("Points ID mismatch with Buckets: points=%+v buckets=%v", got.Points, got.Buckets)
	}
	if got.Points[0].Attempts != 9 || got.Points[1].Attempts != 5 {
		t.Errorf("Points attempts = [%d, %d], want [9, 5] (oldest first)", got.Points[0].Attempts, got.Points[1].Attempts)
	}

	// "rows" must always be present in the raw JSON, never omitted/null,
	// even in series=1 mode where it is semantically empty — a client
	// that always does `for (const r of resp.rows)` without a null check
	// must never see this one field be the exception.
	if !strings.Contains(rec.Body.String(), `"rows":[]`) {
		t.Errorf("series=1 response body does not contain a non-null empty \"rows\":[] field: %s", rec.Body.String())
	}
}

func TestAdminPerformance_StoreDownIs503(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.limiter = newLimiter(alwaysErrStore{}, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminPerformancePath+"?kind=provider&window=day", "sk-admin1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
}

// TestAdminPerformance_SeriesModeLatencyOn_BatchesIntoOneRoundTrip is P6
// (admin dashboard redesign verify round): perfSeries used to read every
// metric via its OWN storeGetMulti round trip — a provider's series=1 with
// latency on cost 4 core + 28 latency metrics = 32 sequential round trips.
// They now batch into one chunked pipeline: (4+28)*48 = 1,536 keys, under
// usageModelsChunkKeys (1,600) — exactly 1 round trip.
func TestAdminPerformance_SeriesModeLatencyOn_BatchesIntoOneRoundTrip(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	store := &countingMultiStore{values: map[string]int64{}}
	cfg := newAdminTestConfig()
	cfg.Admin.Stats = &AdminStatsConfig{Latency: true}
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.limiter = newLimiter(store, true)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminPerformancePath+"?kind=provider&window=hour&span=48&series=1&id=alpha", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if store.getMultiCalls != 1 {
		t.Errorf("getMultiCalls = %d, want 1 (batched into one chunked pipeline, not 32 sequential round trips)", store.getMultiCalls)
	}
}

// TestAdminUsageModels_DetailKeyCap_AccountsForExtraMetrics is P7 (admin
// dashboard redesign verify round): this cap used to check len(ids)*span
// alone, regardless of detail mode — but detail mode reads up to 7
// metrics per id (req/tokin/tokout/cost + r402 + chit/csave when cache is
// configured), not the 1 the plain check assumed. A catalog/span
// combination whose SINGLE-metric size fits under adminMaxKeysPerRequest
// but whose true 7-metric detail read does not must 400, and the
// identical catalog/span in non-detail mode (truly 1 metric) must still
// be allowed through.
func TestAdminUsageModels_DetailKeyCap_AccountsForExtraMetrics(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	models := make([]string, 200)
	for i := range models {
		models[i] = fmt.Sprintf("m%03d", i)
	}
	cfg.Providers["alpha"].Models = models
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m"}
	ln := newBehavioralRedisServer(t)
	cfg.Redis = &RedisConfig{Address: ln.Addr().String()}
	h, _ := newAdminGatewayHandle(t, cfg)

	// 201 catalog ids (200 alpha + 1 zeta) * span 48 = 9,648 keys for a
	// single metric — under adminMaxKeysPerRequest (64,000) alone, but
	// 9,648*7 = 67,536 once detail mode's true metric count is accounted
	// for, which is over it.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageModelsPath+"?metric=req&window=hour&span=48&detail=1", "sk-admin1"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("detail status = %d, want 400 (range too large once detail mode's extra metrics are accounted for)", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, adminRequest(http.MethodGet, adminUsageModelsPath+"?metric=req&window=hour&span=48", "sk-admin1"))
	if rec2.Code != http.StatusOK {
		t.Errorf("non-detail status = %d, want 200 (same catalog/span, single metric, must not be rejected)", rec2.Code)
	}
}
