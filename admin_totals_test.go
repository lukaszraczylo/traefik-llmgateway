package traefikllmgateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- gate ---

func TestAdminUsageTotals_GateMatrix(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	path := adminUsageTotalsPath + "?kind=user"
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

// --- kind matrix / param validation ---

func TestAdminUsageTotals_KindMatrix(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	cases := []struct {
		name string
		qs   string
		want int
	}{
		{"unknown kind", "?kind=bogus", http.StatusBadRequest},
		{"user", "?kind=user", http.StatusOK},
		{"group", "?kind=group", http.StatusOK},
		{"usermodel missing user", "?kind=usermodel", http.StatusBadRequest},
		{"usermodel disabled", "?kind=usermodel&user=alice", http.StatusNotFound},
		{"modeluser missing model", "?kind=modeluser", http.StatusBadRequest},
		{"modeluser disabled", "?kind=modeluser&model=alpha/a-model-1", http.StatusNotFound},
		{"targetcaller", "?kind=targetcaller", http.StatusOK},
		{"invalid window for usermodel", "?kind=targetcaller&window=hour", http.StatusBadRequest},
		{"invalid metrics", "?kind=user&metrics=bogus", http.StatusBadRequest},
		{"invalid limit", "?kind=user&limit=0", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageTotalsPath+c.qs, "sk-admin1"))
			if rec.Code != c.want {
				t.Errorf("status = %d, want %d, body=%s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// TestAdminUsageTotals_UserModelEnabled404sWhenOff pins that BOTH
// usermodel and modeluser 404 while admin.stats.userModel is off — the
// default (DECISIONS Q4) — and both succeed once it is on.
func TestAdminUsageTotals_UserModelEnabled404sWhenOff(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, _ := newAdminGatewayHandle(t, cfg)

	for _, qs := range []string{"?kind=usermodel&user=alice", "?kind=modeluser&model=alpha/a-model-1"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageTotalsPath+qs, "sk-admin1"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("qs=%q: status = %d, want 404 (userModel stats off)", qs, rec.Code)
		}
	}

	cfg2 := newAdminTestConfig()
	cfg2.Admin.Stats = &AdminStatsConfig{UserModel: true}
	h2, _ := newAdminGatewayHandle(t, cfg2)
	for _, qs := range []string{"?kind=usermodel&user=alice", "?kind=modeluser&model=alpha/a-model-1"} {
		rec := httptest.NewRecorder()
		h2.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageTotalsPath+qs, "sk-admin1"))
		if rec.Code != http.StatusOK {
			t.Errorf("qs=%q: status = %d, want 200 (userModel stats on), body=%s", qs, rec.Code, rec.Body.String())
		}
	}
}

// TestAdminUsageTotals_UsermodelTwoPhaseRead seeds one candidate model
// with nonzero req in the cheap-phase covering window, and one with none
// at all, then confirms only the nonzero candidate's actual umodel
// counter is read and returned — the zero-req model never reaches the
// second (per-user) phase at all, pinning the "usermodel candidates via
// cheap phase... then umodel keys for nonzero candidates" optimization
// (plan §1.3(v)).
func TestAdminUsageTotals_UsermodelTwoPhaseRead(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	cfg := newAdminTestConfig()
	cfg.Admin.Stats = &AdminStatsConfig{UserModel: true}
	// Single-model catalog per provider keeps the cheap-phase scope small
	// and the assertion unambiguous.
	cfg.Providers["alpha"].Models = []string{"a-model-1"}
	cfg.Providers["zeta"].Models = []string{"z-model"}

	h, gw := newAdminGatewayHandle(t, cfg)

	monthAnchor := historyStepBack(fixedNow, windowMonth, 0)
	umodelID := userModelScopeID("alice", "alpha/a-model-1")

	store := &countingMultiStore{values: map[string]int64{
		// Cheap phase: alpha/a-model-1 has usage this month, zeta/z-model
		// does not (absent key == 0 already, nothing to seed).
		windowKey(kindModel, "alpha/a-model-1", metricReq, windowMonth, monthAnchor): 5,
		// Second phase: alice's own umodel counters for that one candidate.
		windowKey(kindUserModel, umodelID, metricReq, windowDay, fixedNow):    3,
		windowKey(kindUserModel, umodelID, metricCost, windowDay, fixedNow):   9000,
		windowKey(kindUserModel, umodelID, metricTokIn, windowDay, fixedNow):  0,
		windowKey(kindUserModel, umodelID, metricTokOut, windowDay, fixedNow): 0,
	}}
	gw.limiter = newLimiter(store, true)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageTotalsPath+"?kind=usermodel&user=alice&window=day", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminTotalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("Rows = %+v, want exactly 1 row (alpha/a-model-1)", got.Rows)
	}
	row := got.Rows[0]
	if row.ID != "alpha/a-model-1" {
		t.Errorf("Rows[0].ID = %q, want alpha/a-model-1", row.ID)
	}
	if row.Values[metricReq] != 3 || row.Values[metricCost] != 9000 {
		t.Errorf("Rows[0].Values = %+v, want req=3 cost=9000", row.Values)
	}
}

// TestAdminUsageTotals_DropsAllZeroRows pins "Drop all-zero rows" (plan
// §1.3(v)) for the user kind.
func TestAdminUsageTotals_DropsAllZeroRows(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)

	store := &countingMultiStore{values: map[string]int64{
		windowKey("user", "alice", metricReq, windowDay, fixedNow): 4,
		// admin1's own row: every requested metric stays 0 (unseeded).
	}}
	gw.limiter = newLimiter(store, true)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageTotalsPath+"?kind=user&window=day", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminTotalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, row := range got.Rows {
		if row.ID == "admin1" {
			t.Errorf("all-zero row for admin1 present, want dropped: %+v", row)
		}
	}
}

// TestAdminUsageTotals_StoreDownIs503 mirrors the series/models endpoints'
// own store-failure contract.
func TestAdminUsageTotals_StoreDownIs503(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.limiter = newLimiter(alwaysErrStore{}, false)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageTotalsPath+"?kind=user", "sk-admin1"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
}

// TestAdminUsageTotals_TargetCallerRowID pins the targetcaller row id
// shape ("mcp/{target}/{caller}" — plan's own "mcp/fetch/alice" example).
func TestAdminUsageTotals_TargetCallerRowID(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	cfg := newAdminTestConfig()
	cfg.MCPServers = map[string]*TargetConfig{"fetch": {URL: "http://fetch.invalid"}}
	h, gw := newAdminGatewayHandle(t, cfg)

	id := targetCallerScopeID("mcp", "fetch", "alice")
	store := &countingMultiStore{values: map[string]int64{
		windowKey(kindTargetCaller, id, metricReq, windowDay, fixedNow): 2,
	}}
	gw.limiter = newLimiter(store, true)
	gw.limiter.nowFn = func() time.Time { return fixedNow }

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageTotalsPath+"?kind=targetcaller&window=day", "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got adminTotalsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, row := range got.Rows {
		if row.ID == "mcp/fetch/alice" {
			found = true
			if row.Values[metricReq] != 2 {
				t.Errorf("row values = %+v, want req=2", row.Values)
			}
		}
	}
	if !found {
		t.Errorf("Rows = %+v, want a row id 'mcp/fetch/alice'", got.Rows)
	}
}

// TestReadSpanTotals_ChunksAtUsageModelsChunkKeys is P6/P12 (admin
// dashboard redesign verify round): readSpanTotals — the reader behind
// GET /admin/api/usage/totals' kind=user/group/usermodel/targetcaller
// rows and GET /admin/api/performance's own aggregate (series=0) mode —
// used to build ONE unbounded storeGetMulti pipeline covering its whole
// ids*metrics*span key set. It now delegates to the shared, chunked
// spanTotalsMulti reader (limits.go) that also backs admin.go's
// chunkedModelSpanTotals/Multi (TestChunkedModelSpanTotals_
// SplitsIntoMultipleRoundTrips, admin_test.go, pins that side).
func TestReadSpanTotals_ChunksAtUsageModelsChunkKeys(t *testing.T) {
	t.Parallel()
	fixedNow := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	const n = 200
	const span = 48
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("u%03d", i)
	}
	store := &countingMultiStore{values: map[string]int64{}}
	l := newLimiter(store, true)
	l.nowFn = func() time.Time { return fixedNow }
	gw := &Gateway{limiter: l}

	metrics := []string{metricProvAttempt, metricProvFail}
	got, ok := gw.readSpanTotals("user", ids, metrics, windowHour, fixedNow, span, 0)
	if !ok {
		t.Fatal("want ok=true")
	}
	if len(got) != n {
		t.Fatalf("len(got) = %d, want %d", len(got), n)
	}

	// wantCalls mirrors spanTotalsMulti's own chunk-by-ID-count math
	// (limits.go): perChunk = usageModelsChunkKeys / (span*len(metrics)),
	// truncated, so a chunk can carry fewer than usageModelsChunkKeys keys
	// when that division does not land evenly — chunking by whole ids,
	// never splitting one id's own key range across two round trips, so
	// the per-chunk key count is not always maximally packed.
	perChunk := usageModelsChunkKeys / (len(metrics) * span)
	wantCalls := (n + perChunk - 1) / perChunk
	if store.getMultiCalls != wantCalls {
		t.Errorf("getMultiCalls = %d, want %d (%d ids, %d ids/chunk)", store.getMultiCalls, wantCalls, n, perChunk)
	}
	if store.getMultiCalls <= 1 {
		t.Error("want more than 1 round trip — otherwise this test proves nothing about chunking")
	}
}
