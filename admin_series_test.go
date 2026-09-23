package traefikllmgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- gate ---

func TestAdminUsageSeries_GateMatrix(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	path := adminUsageSeriesPath + "?scope=total&metric=req&window=day"
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

// --- param validation ---

func TestAdminUsageSeries_ParamValidation(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	cases := []struct {
		name string
		qs   string
		want int
	}{
		{"no scope", "?metric=req&window=day", http.StatusBadRequest},
		{"unknown window", "?scope=total&metric=req&window=fortnight", http.StatusBadRequest},
		{"unknown metric", "?scope=total&metric=bogus&window=day", http.StatusBadRequest},
		{"invalid span", "?scope=total&metric=req&window=day&span=0", http.StatusBadRequest},
		{"invalid offset", "?scope=total&metric=req&window=day&span=30&offset=10", http.StatusBadRequest},
		{"malformed scope prefix", "?scope=bogus&metric=req&window=day", http.StatusOK}, // skipped, not fatal
		{"metric invalid for scope kind", "?scope=provider:alpha&metric=req&window=day", http.StatusBadRequest},
		{"valid", "?scope=total&metric=req&window=day", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageSeriesPath+c.qs, "sk-admin1"))
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d, body=%s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestAdminUsageSeries_TooManyScopesIs400(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	q := "?metric=req&window=day"
	for i := 0; i <= adminSeriesMaxScopes; i++ {
		q += "&scope=total"
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageSeriesPath+q, "sk-admin1"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (over adminSeriesMaxScopes)", rec.Code)
	}
}

// TestAdminUsageSeries_UnknownScopeSkippedNotFailed pins the "skip,
// don't fail the whole request" contract for an unrecognized scope id
// (parseSeriesScope ok but no matching user/group/model/provider).
func TestAdminUsageSeries_UnknownScopeSkippedNotFailed(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageSeriesPath+"?scope=user:ghost&scope=total&metric=req&window=day", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminSeriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Series) != 1 || got.Series[0].Scope != "total" {
		t.Errorf("Series = %+v, want exactly [total]", got.Series)
	}
	if len(got.Unknown) != 1 || got.Unknown[0] != "user:ghost" {
		t.Errorf("Unknown = %v, want [user:ghost]", got.Unknown)
	}
}

// TestAdminUsageSeries_BucketAlignmentWithOffset seeds two adjacent day
// buckets with distinct known values and asserts an offset=1 request
// reads the OLDER bucket, not the current one — the core offset
// contract every stats_read.go endpoint shares.
func TestAdminUsageSeries_BucketAlignmentWithOffset(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	yesterday := fixedNow.AddDate(0, 0, -1)

	store := &countingMultiStore{values: map[string]int64{
		windowKey(totalScopeKind, totalScopeID, metricReq, windowDay, fixedNow):  111,
		windowKey(totalScopeKind, totalScopeID, metricReq, windowDay, yesterday): 222,
	}}
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.limiter = newLimiter(store, true)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageSeriesPath+"?scope=total&metric=req&window=day&span=1&offset=1", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminSeriesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Offset != 1 {
		t.Errorf("Offset = %d, want 1", got.Offset)
	}
	if len(got.Series) != 1 || len(got.Series[0].Points) != 1 || got.Series[0].Points[0] != 222 {
		t.Errorf("Series = %+v, want one point = 222 (yesterday's bucket)", got.Series)
	}
	if len(got.Buckets) != 1 || got.Buckets[0] != bucketFor(yesterday, windowDay) {
		t.Errorf("Buckets = %v, want [%s]", got.Buckets, bucketFor(yesterday, windowDay))
	}
}

// TestAdminUsageSeries_StoreDownIs503 pins the 503 contract via a store
// that always errors (fail-closed path).
func TestAdminUsageSeries_StoreDownIs503(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.limiter = newLimiter(alwaysErrStore{}, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageSeriesPath+"?scope=total&metric=req&window=day", "sk-admin1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
}

// TestAdminUsageSeries_LatchedFailOpenIs503 pins the "fail-open serves
// zeros from the fallback, but configuredStoreDown must still answer 503"
// rule every admin stats reader in this package shares (currentUsage's
// own doc comment, limits.go): a store that errors, with failOpen=true,
// must NOT silently render a fail-open zero-series as real data.
func TestAdminUsageSeries_LatchedFailOpenIs503(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.limiter = newLimiter(alwaysErrStore{}, true)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageSeriesPath+"?scope=total&metric=req&window=day", "sk-admin1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (latched store-down, not a fail-open zero-series), body=%s", rec.Code, rec.Body.String())
	}
}

// TestAdminUsageSeries_ProviderAndProvmodelScopes exercises the two scope
// kinds unique to this endpoint (parseSeriesScope/seriesScopeExists),
// beyond what GET /admin/api/usage/history ever accepted.
func TestAdminUsageSeries_ProviderAndProvmodelScopes(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	cases := []struct {
		name string
		qs   string
		want int
	}{
		{"known provider", "?scope=provider:alpha&metric=attempt&window=day", http.StatusOK},
		{"unknown provider", "?scope=provider:ghost&metric=attempt&window=day", http.StatusOK}, // skipped -> unknown
		{"known provmodel", "?scope=provmodel:alpha/a-model-1&metric=attempt&window=day", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageSeriesPath+c.qs, "sk-admin1"))
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d, body=%s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// TestAdminUsageSeries_ChunksAtUsageModelsChunkKeys is P6 (admin
// dashboard redesign verify round): this endpoint's own storeGetMulti
// used to build ONE unbounded pipeline for the whole request regardless
// of how many scopes*span keys that worked out to; it is now chunked at
// usageModelsChunkKeys keys per round trip (chunkedGetMulti, limits.go).
// adminSeriesMaxScopes repeated "total" scopes (a scope kind with no
// existence check, so this needs no catalog/user fixture) at the maximum
// hour span (48) is 100*48 = 4,800 keys — 3 chunked round trips, never 1.
func TestAdminUsageSeries_ChunksAtUsageModelsChunkKeys(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	store := &countingMultiStore{values: map[string]int64{}}
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.limiter = newLimiter(store, true)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	qs := "?metric=req&window=hour&span=48"
	for i := 0; i < adminSeriesMaxScopes; i++ {
		qs += "&scope=total"
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageSeriesPath+qs, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	totalKeys := adminSeriesMaxScopes * 48
	wantCalls := (totalKeys + usageModelsChunkKeys - 1) / usageModelsChunkKeys
	if store.getMultiCalls != wantCalls {
		t.Errorf("getMultiCalls = %d, want %d (%d keys chunked at %d/call)", store.getMultiCalls, wantCalls, totalKeys, usageModelsChunkKeys)
	}
	if store.getMultiCalls <= 1 {
		t.Error("want more than 1 round trip — otherwise this test proves nothing about chunking")
	}
}
