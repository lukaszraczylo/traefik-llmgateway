package traefikllmgateway

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fleetCachedBody = `{"id":"c1","object":"chat.completion","model":"shared","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":8}}}`

// keyCountStore counts the keys each getMulti call carries.
type keyCountStore struct {
	counterStore
	total  atomic.Int64
	maxOne atomic.Int64
}

func (s *keyCountStore) getMulti(keys []string) ([]int64, error) {
	n := int64(len(keys))
	s.total.Add(n)
	for {
		cur := s.maxOne.Load()
		if n <= cur || s.maxOne.CompareAndSwap(cur, n) {
			break
		}
	}
	return s.counterStore.getMulti(keys)
}

func fleetConfig(statuses [3]int, body string, rpm int64) (*Config, func()) {
	mk := func(status int) *routerBackend {
		if status == 200 {
			return newRouterBackend(status, body)
		}
		return newRouterBackend(status, routerFail)
	}
	a, b, c := mk(statuses[0]), mk(statuses[1]), mk(statuses[2])
	cfg := routerTestConfig(a, b, c, threeTiers())
	cfg.Admin = &AdminConfig{Enabled: true, Stats: &AdminStatsConfig{Latency: true}}
	cfg.Metrics = &MetricsConfig{Enabled: true, ModelLabel: true}
	cfg.Users.Inline[0].Limits = &LimitsConfig{RequestsPerMinute: rpm}
	return cfg, func() { closeBackends(a, b, c) }
}

func fleetGateway(t *testing.T, cfg *Config, store counterStore) *Gateway {
	t.Helper()
	_, gw := buildRouterGateway(t, cfg)
	gw.limiter.spawn = func(f func()) { f() }
	gw.limiter.store = store
	return gw
}

func fleetSamples(t *testing.T, gw *Gateway) (map[string]string, []promSample) {
	t.Helper()
	return parsePrometheusText(t, gw.renderMetrics())
}

func fleetHelp(t *testing.T, gw *Gateway, family string) string {
	t.Helper()
	for _, line := range strings.Split(string(gw.renderMetrics()), "\n") {
		if strings.HasPrefix(line, "# HELP "+family+" ") {
			return line
		}
	}
	return ""
}

func TestFleetMetricsStoreBacked(t *testing.T) {
	store := newMemoryStore()
	cfgA, closeA := fleetConfig([3]int{503, 200, 200}, fleetCachedBody, 1)
	defer closeA()
	cfgB, closeB := fleetConfig([3]int{200, 200, 200}, fleetCachedBody, 1)
	defer closeB()
	gwA := fleetGateway(t, cfgA, store)
	gwB := fleetGateway(t, cfgB, store)

	require.Equal(t, 200, routerChat(t, gwA, routerName, "hi", nil).Code)
	require.Equal(t, 429, routerChat(t, gwA, routerName, "hi", nil).Code, "second request exceeds rpm=1")

	require.Zero(t, gwB.routerStats.requests[routerRequestKey{router: routerName, tier: "t0", reason: "score"}], "B handled nothing")
	_, samples := fleetSamples(t, gwB)

	tests := []struct {
		name   string
		family string
		labels map[string]string
		want   string
	}{
		{"router request", "llmgateway_router_requests_total", map[string]string{"router": routerName, "tier": "t0", "reason": "score"}, "1"},
		{"router escalation", "llmgateway_router_escalations_total", map[string]string{"router": routerName, "from_tier": "t0", "to_tier": "t1"}, "1"},
		{"latency count", "llmgateway_upstream_duration_seconds_count", map[string]string{"provider": "beta", "stream": "false"}, "1"},
		{"latency inf bucket", "llmgateway_upstream_duration_seconds_bucket", map[string]string{"provider": "beta", "stream": "false", "le": "+Inf"}, "1"},
		{"ttfb count", "llmgateway_upstream_ttfb_seconds_count", map[string]string{"provider": "beta", "stream": "false"}, "1"},
		{"model latency count", "llmgateway_upstream_model_duration_seconds_count", map[string]string{"provider": "beta", "stream": "false", "model": "b1"}, "1"},
		{"cache read", "llmgateway_prompt_cache_tokens_total", map[string]string{"provider": "beta", "model": "b1", "kind": "read"}, "8"},
		{"cache uncached", "llmgateway_prompt_cache_tokens_total", map[string]string{"provider": "beta", "model": "b1", "kind": "uncached"}, "2"},
		{"provenance requests", "llmgateway_usage_provenance_requests_total", map[string]string{"provider": "beta", "provenance": provenanceReported}, "1"},
		{"provenance tokens", "llmgateway_usage_provenance_tokens_total", map[string]string{"provider": "beta", "provenance": provenanceReported}, "15"},
		{"rejection", "llmgateway_rate_limit_rejections_total", map[string]string{"scope_kind": "user", "scope_id": "alice"}, "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertSample(t, samples, tt.family, tt.labels, tt.want)
		})
	}

	for _, fam := range []string{
		"llmgateway_router_requests_total", "llmgateway_router_escalations_total",
		"llmgateway_upstream_duration_seconds", "llmgateway_upstream_ttfb_seconds",
		"llmgateway_prompt_cache_tokens_total", "llmgateway_usage_provenance_requests_total",
		"llmgateway_usage_provenance_tokens_total", "llmgateway_rate_limit_rejections_total",
	} {
		help := fleetHelp(t, gwB, fam)
		assert.Contains(t, help, "aggregate with max()", fam)
		assert.NotContains(t, help, "aggregate with sum(), which is always correct", fam)
	}
}

func TestFleetMetricsDecisionStoreBacked(t *testing.T) {
	store := newMemoryStore()
	a := newDecisionFixture(t, 3, "", okHandler).g
	b := newDecisionFixture(t, 3, "", okHandler).g
	for _, g := range []*Gateway{a, b} {
		g.cfg.Metrics = &MetricsConfig{Enabled: true}
		g.limiter.store = store
		g.limiter.statsAdmin = true
		g.limiter.spawn = func(f func()) { f() }
	}
	plan := decisionStatsPlan(decisionOverride, "ta", "tc")
	plan.router = "auto"
	a.recordRouterAttempt(plan, 0, cacheEndpointChat, nil)

	out := scrape(b)
	assert.Contains(t, out, `llmgateway_router_decision_total{router="auto",outcome="override"} 1`)
	assert.Contains(t, out, `llmgateway_router_decision_tiers_total{router="auto",heuristic="ta",model="tc"} 1`)
	assert.Contains(t, out, "aggregate with max()")
}

func TestFleetMetricsHistogramSumAndBuckets(t *testing.T) {
	store := newMemoryStore()
	cfg, closeAll := fleetConfig([3]int{200, 200, 200}, fleetCachedBody, 100)
	defer closeAll()
	gw := fleetGateway(t, cfg, store)
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return now }
	wb := bucketsFor(now)
	et := enforceTTLsFor()

	samples := []latencySample{
		{duration: 300 * time.Millisecond, ttfb: 100 * time.Millisecond, hasTTFB: true},
		{duration: 1500 * time.Millisecond, ttfb: 200 * time.Millisecond, hasTTFB: true, streaming: true},
		{duration: 700 * time.Second, hasTTFB: false},
	}
	for _, s := range samples {
		_, ok := gw.limiter.storeIncrMulti(latencyCounterEntries(kindProvider, "alpha", s, wb, et))
		require.True(t, ok)
	}

	_, got := fleetSamples(t, gw)
	nonStream := map[string]string{"provider": "alpha", "stream": "false"}
	stream := map[string]string{"provider": "alpha", "stream": "true"}
	assertSample(t, got, "llmgateway_upstream_duration_seconds_count", nonStream, "2")
	assertSample(t, got, "llmgateway_upstream_duration_seconds_count", stream, "1")
	assertSample(t, got, "llmgateway_upstream_duration_seconds_sum", nonStream, "700.3")
	assertSample(t, got, "llmgateway_upstream_duration_seconds_sum", stream, "1.5")
	assertSample(t, got, "llmgateway_upstream_ttfb_seconds_count", nonStream, "1")
	assertSample(t, got, "llmgateway_upstream_ttfb_seconds_sum", nonStream, "0.1")

	ne := map[string]string{"provider": "alpha", "stream": "false", "le": "0.25"}
	assertSample(t, got, "llmgateway_upstream_duration_seconds_bucket", ne, "0")
	ne["le"] = "0.5"
	assertSample(t, got, "llmgateway_upstream_duration_seconds_bucket", ne, "1")
	ne["le"] = "600"
	assertSample(t, got, "llmgateway_upstream_duration_seconds_bucket", ne, "1")
	ne["le"] = "+Inf"
	assertSample(t, got, "llmgateway_upstream_duration_seconds_bucket", ne, "2")
}

func TestFleetMetricsNoStoreUnchanged(t *testing.T) {
	cfg, closeAll := fleetConfig([3]int{503, 200, 200}, fleetCachedBody, 1)
	defer closeAll()
	_, gw := buildRouterGateway(t, cfg)
	require.Nil(t, gw.limiter.store)
	require.Equal(t, 200, routerChat(t, gw, routerName, "hi", nil).Code)
	require.Equal(t, 429, routerChat(t, gw, routerName, "hi", nil).Code)

	out := scrape(gw)
	for _, want := range []string{
		`llmgateway_router_requests_total{router="auto",tier="t0",reason="score"} 1`,
		`llmgateway_router_escalations_total{router="auto",from_tier="t0",to_tier="t1"} 1`,
		`llmgateway_prompt_cache_tokens_total{provider="beta",model="b1",kind="read"} 8`,
		`llmgateway_usage_provenance_requests_total{provider="beta",provenance="reported"} 1`,
		`llmgateway_rate_limit_rejections_total{scope_kind="user",scope_id="alice"} 1`,
		`llmgateway_upstream_duration_seconds_count{provider="beta",stream="false"} 1`,
	} {
		assert.Contains(t, out, want)
	}
	for _, fam := range []string{
		"llmgateway_router_requests_total", "llmgateway_upstream_duration_seconds",
		"llmgateway_prompt_cache_tokens_total", "llmgateway_usage_provenance_requests_total",
		"llmgateway_rate_limit_rejections_total",
	} {
		help := fleetHelp(t, gw, fam)
		assert.NotContains(t, help, "aggregate with max()", fam)
		assert.Contains(t, help, "aggregate with sum()", fam)
	}
}

func TestFleetMetricsStoreDownSkips(t *testing.T) {
	store := newMemoryStore()
	cfg, closeAll := fleetConfig([3]int{503, 200, 200}, fleetCachedBody, 1)
	defer closeAll()
	gw := fleetGateway(t, cfg, store)
	require.Equal(t, 200, routerChat(t, gw, routerName, "hi", nil).Code)
	require.Equal(t, 429, routerChat(t, gw, routerName, "hi", nil).Code)

	gw.limiter.store = alwaysErrStore{}
	out := scrape(gw)
	for _, fam := range []string{
		"llmgateway_router_requests_total{", "llmgateway_router_escalations_total{",
		"llmgateway_upstream_duration_seconds_count{", "llmgateway_upstream_ttfb_seconds_count{",
		"llmgateway_prompt_cache_tokens_total{", "llmgateway_usage_provenance_requests_total{",
		"llmgateway_usage_provenance_tokens_total{", "llmgateway_rate_limit_rejections_total{",
	} {
		assert.NotContains(t, out, fam, "store down must not fabricate or fall back to local samples")
	}
	assert.Contains(t, out, "llmgateway_provider_healthy{", "non-store families still render")
	assert.Contains(t, out, "llmgateway_limit_store_up 0")
}

func TestFleetMetricsKeyBudget(t *testing.T) {
	const providers, wantModels = 19, 333
	cfg, closeAll := fleetConfig([3]int{200, 200, 200}, fleetCachedBody, 100)
	defer closeAll()
	cfg.Providers = map[string]*ProviderConfig{}
	for p := 0; p < providers; p++ {
		count := wantModels / providers
		if p < wantModels%providers {
			count++
		}
		models := make([]string, count)
		for m := range models {
			models[m] = fmt.Sprintf("m%02d", m)
		}
		cfg.Providers[fmt.Sprintf("p%02d", p)] = &ProviderConfig{Type: "openai", BaseURL: "http://x.invalid", APIKey: "k", Models: models}
	}
	cfg.SmartRouters = nil
	store := &keyCountStore{counterStore: newMemoryStore()}
	gw := fleetGateway(t, cfg, store)
	require.Len(t, gw.catalogModelScopeIDs(), wantModels)

	out := scrape(gw)
	require.Contains(t, out, "llmgateway_limit_store_up 1")
	assert.LessOrEqual(t, store.total.Load(), int64(adminMaxKeysPerRequest), "one scrape")
	assert.LessOrEqual(t, store.maxOne.Load(), int64(usageModelsChunkKeys), "every read is chunked")
	t.Logf("scrape keys for %d models: %d (max per call %d)", len(gw.catalogModelScopeIDs()), store.total.Load(), store.maxOne.Load())
}

func TestFleetMetricsFallsBackWhenPrerequisiteOff(t *testing.T) {
	cfg, closeAll := fleetConfig([3]int{200, 200, 200}, fleetCachedBody, 100)
	defer closeAll()
	cfg.Admin.Stats.Latency = false
	gw := fleetGateway(t, cfg, newMemoryStore())
	require.Equal(t, 200, routerChat(t, gw, routerName, "hi", nil).Code)

	out := scrape(gw)
	assert.Contains(t, out, `llmgateway_upstream_duration_seconds_count{provider="alpha",stream="false"} 1`, "latency stays in memory without statsLatency")
	help := fleetHelp(t, gw, "llmgateway_upstream_duration_seconds")
	assert.Contains(t, help, "aggregate with sum()")
	assert.Contains(t, fleetHelp(t, gw, "llmgateway_router_requests_total"), "aggregate with max()", "router stays store-backed")
}

// familyFailStore fails getMulti only for batches that carry a key matching
// fail; every other read and every write goes through.
type familyFailStore struct {
	counterStore
	fail func(key string) bool
}

func (s familyFailStore) getMulti(keys []string) ([]int64, error) {
	for _, k := range keys {
		if s.fail(k) {
			return nil, errStoreDownStub
		}
	}
	return s.counterStore.getMulti(keys)
}

// A failed read latches the limiter store down (storeLatched), so every read
// after it in the same scrape is skipped too. Families rendered before the
// failing one are unaffected; for the last store-backed family, only it is
// missing.
func TestFleetMetricsPartialStoreFailure(t *testing.T) {
	isLatency := func(k string) bool {
		for _, m := range []string{":ld", ":lt", ":lsd", ":lst"} {
			if strings.Contains(k, m) {
				return true
			}
		}
		return false
	}
	isRejection := func(k string) bool { return strings.Contains(k, ":"+metricRej+":"+windowMonth+":") }
	allFleet := []string{
		"llmgateway_router_requests_total{", "llmgateway_router_escalations_total{",
		"llmgateway_upstream_duration_seconds_count{", "llmgateway_upstream_ttfb_seconds_count{",
		"llmgateway_prompt_cache_tokens_total{", "llmgateway_usage_provenance_requests_total{",
		"llmgateway_usage_provenance_tokens_total{", "llmgateway_rate_limit_rejections_total{",
	}
	except := func(drop ...string) []string {
		var out []string
		for _, f := range allFleet {
			keep := true
			for _, d := range drop {
				if f == d {
					keep = false
				}
			}
			if keep {
				out = append(out, f)
			}
		}
		return out
	}
	tests := []struct {
		name    string
		fail    func(string) bool
		missing []string
		present []string
	}{
		{"last family only", isRejection,
			[]string{"llmgateway_rate_limit_rejections_total{"},
			append(except("llmgateway_rate_limit_rejections_total{"), "llmgateway_requests_total{", "llmgateway_provider_healthy{")},
		{"latency, then latched", isLatency,
			[]string{"llmgateway_upstream_duration_seconds_count{", "llmgateway_upstream_ttfb_seconds_count{"},
			[]string{"llmgateway_requests_total{", "llmgateway_provider_attempts_total{", "llmgateway_provider_healthy{"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem := newMemoryStore()
			cfg, closeAll := fleetConfig([3]int{503, 200, 200}, fleetCachedBody, 1)
			defer closeAll()
			gw := fleetGateway(t, cfg, mem)
			require.Equal(t, 200, routerChat(t, gw, routerName, "hi", nil).Code)
			require.Equal(t, 429, routerChat(t, gw, routerName, "hi", nil).Code)
			base := scrape(gw)
			for _, f := range allFleet {
				require.True(t, strings.Contains(base, f), "baseline lacks %s", f)
			}

			gw.limiter.store = familyFailStore{counterStore: mem, fail: tt.fail}
			out := scrape(gw)
			for _, f := range tt.missing {
				assert.False(t, strings.Contains(out, f), "%s must be absent", f)
			}
			for _, f := range tt.present {
				assert.True(t, strings.Contains(out, f), "%s must be present", f)
			}
		})
	}
}
