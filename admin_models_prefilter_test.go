package traefikllmgateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// prefilterCountingStore wraps a memoryStore and records every getMulti.
type prefilterCountingStore struct {
	*memoryStore
	keys    int
	maxCall int
}

func (s *prefilterCountingStore) getMulti(keys []string) ([]int64, error) {
	s.keys += len(keys)
	if len(keys) > s.maxCall {
		s.maxCall = len(keys)
	}
	return s.memoryStore.getMulti(keys)
}

const (
	prefilterCatalogSize = 333
	prefilterActiveCount = 30
)

// prefilterGateway builds a gateway with a 333-model catalog and writes the
// same traffic through the real accounting path. withStore picks a
// non-nil store (prefilter active) or the nil-store fallback (today's
// unfiltered path), so the two answers can be compared byte for byte.
func prefilterGateway(t *testing.T, withStore bool, now time.Time) (http.Handler, *Gateway, *prefilterCountingStore) {
	t.Helper()
	cfg := newAdminTestConfig()
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m"}
	cfg.Providers = map[string]*ProviderConfig{}
	for p := 0; p < 10; p++ {
		var models []string
		for m := 0; m < 34 && p*34+m < prefilterCatalogSize; m++ {
			models = append(models, fmt.Sprintf("m%02d", m))
		}
		name := fmt.Sprintf("prov%d", p)
		cfg.Providers[name] = &ProviderConfig{Type: "openai", BaseURL: "http://" + name + ".invalid", APIKey: "sk-" + name, Models: models}
	}
	h, gw := newAdminGatewayHandle(t, cfg)
	gw.cache = &responseCache{ttl: time.Minute} // turns on chit/csave reads
	if got := len(gw.catalogModelScopeIDs()); got != prefilterCatalogSize {
		t.Fatalf("catalog = %d ids, want %d", got, prefilterCatalogSize)
	}
	var cs *prefilterCountingStore
	if withStore {
		cs = &prefilterCountingStore{memoryStore: newMemoryStore()}
		gw.limiter.store = cs
	}

	instants := []time.Time{
		time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 31, 23, 30, 0, 0, time.UTC),
		time.Date(2026, 9, 1, 0, 10, 0, 0, time.UTC),
		time.Date(2026, 9, 1, 5, 20, 0, 0, time.UTC),
		time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 2, 11, 59, 0, 0, time.UTC),
	}
	ids := gw.catalogModelScopeIDs()
	for k := 0; k < prefilterActiveCount; k++ {
		id := ids[(k*11+3)%len(ids)]
		for j := 0; j <= k%3; j++ {
			at := instants[(k+j*2)%len(instants)]
			gw.limiter.nowFn = func() time.Time { return at }
			u := usage{prompt: int64(10 + k), completion: int64(5 + j)}
			cost := int64(100 * (k + 1))
			if k%7 == 0 {
				u, cost = usage{}, 0 // request-only row, like a media call
			}
			gw.limiter.accountWith(withModelScope(nil, id), u, cost, accountExtras{})
			if k%4 == 0 {
				gw.limiter.recordCacheHit(id, int64(7*(k+1)))
			}
		}
	}
	// r402 without any core traffic must stay out in both modes.
	gw.limiter.nowFn = func() time.Time { return instants[5] }
	gw.limiter.recordUnpriced402(ids[1])
	gw.limiter.nowFn = func() time.Time { return now }
	return h, gw, cs
}

func prefilterGet(t *testing.T, h http.Handler, query string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageModelsPath+"?"+query, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d, body=%s", query, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestAdminUsageModels_MonthPrefilter_IdenticalAndFewerKeys pins that the
// month prefilter changes no byte of the response and cuts the key reads.
func TestAdminUsageModels_MonthPrefilter_IdenticalAndFewerKeys(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	hFilt, _, cs := prefilterGateway(t, true, now)
	hBase, _, _ := prefilterGateway(t, false, now)

	cases := []struct {
		name       string
		window     string
		span       int
		offset     int
		monthSpan  int
		prefilters bool
	}{
		{"hour24", "hour", 24, 0, 2, true},
		{"hour24-offset-boundary", "hour", 24, 8, 2, true},
		{"day7", "day", 7, 0, 2, true},
		{"day14", "day", 14, 0, 2, true},
		{"day7-offset", "day", 7, 3, 2, true},
		{"day2", "day", 2, 0, 1, true},
		{"month2", "month", 2, 0, 2, false},
		{"month1-prev", "month", 1, 1, 1, false},
	}
	for _, c := range cases {
		for _, detail := range []bool{false, true} {
			for _, metric := range []string{"cost", "req"} {
				q := fmt.Sprintf("metric=%s&window=%s&span=%d&offset=%d&limit=100", metric, c.window, c.span, c.offset)
				if detail {
					q += "&detail=1"
				}
				name := fmt.Sprintf("%s/detail=%v/%s", c.name, detail, metric)
				cs.keys, cs.maxCall = 0, 0
				got := prefilterGet(t, hFilt, q)
				keys := cs.keys
				want := prefilterGet(t, hBase, q)
				if c.name == "day14" && !strings.Contains(got, `"prov`) {
					t.Fatalf("%s: fixture produced no rows: %s", name, got)
				}
				if got != want {
					t.Errorf("%s: response differs\n got: %.300s\nwant: %.300s", name, got, want)
				}
				if cs.maxCall > usageModelsChunkKeys {
					t.Errorf("%s: one read of %d keys exceeds chunk cap %d", name, cs.maxCall, usageModelsChunkKeys)
				}

				// Unfiltered key count for the same request.
				mult := 1
				if detail {
					mult = 5 // 4 core + r402
					if c.window != "hour" {
						mult += 2 // chit, csave
					}
				}
				span := c.span
				before := prefilterCatalogSize * span * mult
				if detail && c.window == windowMonth {
					daySpan, _ := monthRangeToDayBuckets(now, c.span, c.offset)
					before = prefilterCatalogSize * (4*c.span + 3*daySpan)
				}
				if detail {
					before += prefilterActiveCount * 3 * span // page-level prompt-cache read
				}
				// Active ids: 30 plus slack for the r402-only id; the
				// page-level prompt-cache read adds 3 metrics per row.
				active := prefilterActiveCount + 1
				after := active*span*mult + active*3*span
				if c.prefilters {
					n := 1
					if detail {
						n = 4
					}
					after += prefilterCatalogSize * n * c.monthSpan
				}
				if detail && c.window == windowMonth {
					daySpan, _ := monthRangeToDayBuckets(now, c.span, c.offset)
					after = prefilterCatalogSize*4*c.span + active*(3*daySpan) + active*3*span
				}
				if !detail && c.window == windowMonth {
					after = before
				}
				if keys > after {
					t.Errorf("%s: read %d keys, want at most %d (unfiltered %d)", name, keys, after, before)
				}
				if testing.Verbose() {
					t.Logf("%s: keys before=%d after=%d", name, before, keys)
				}
			}
		}
	}
}

func TestMonthsTouched(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		window               string
		span, offset         int
		wantSpan, wantOffset int
	}{
		{windowHour, 24, 0, 1, 0},
		{windowHour, 60, 0, 2, 0},
		{windowHour, 24, 20, 2, 0},
		{windowHour, 24, 40, 1, 1},
		{windowDay, 7, 0, 2, 0},
		{windowDay, 2, 0, 1, 0},
		{windowDay, 7, 5, 1, 1},
		{windowDay, 35, 0, 3, 0},
		{windowMonth, 3, 1, 3, 1},
	}
	for _, c := range cases {
		gotSpan, gotOffset := monthsTouched(c.window, now, c.span, c.offset)
		if gotSpan != c.wantSpan || gotOffset != c.wantOffset {
			t.Errorf("%s span=%d offset=%d: got (%d,%d), want (%d,%d)", c.window, c.span, c.offset, gotSpan, gotOffset, c.wantSpan, c.wantOffset)
		}
	}
}
