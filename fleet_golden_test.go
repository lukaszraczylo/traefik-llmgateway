package traefikllmgateway

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const noStoreGoldenPath = "testdata/fleet_metrics_nostore.golden"

// noStoreGoldenScrape renders /metrics from a store-less gateway fed fixed
// observations through the in-memory recording paths, so the body is
// deterministic.
func noStoreGoldenScrape(t *testing.T) string {
	t.Helper()
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	cfg := routerTestConfig(a, b, c, threeTiers())
	cfg.Admin = &AdminConfig{Enabled: true, Stats: &AdminStatsConfig{Latency: true}}
	cfg.Metrics = &MetricsConfig{Enabled: true, ModelLabel: true}
	_, gw := buildRouterGateway(t, cfg)
	require.Nil(t, gw.limiter.store)
	gw.limiter.nowFn = func() time.Time { return time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC) }

	gw.recordLatency("alpha", "a1", latencySample{duration: 300 * time.Millisecond, ttfb: 100 * time.Millisecond, hasTTFB: true})
	gw.recordLatency("alpha", "a1", latencySample{duration: 1500 * time.Millisecond, ttfb: 200 * time.Millisecond, hasTTFB: true, streaming: true})
	gw.recordLatency("beta", "b1", latencySample{duration: 700 * time.Second})
	gw.recordPromptCache("alpha", "a1", usage{prompt: 10, cached: 8, completion: 5})
	gw.recordPromptCache("beta", "b1", usage{prompt: 20, completion: 5})
	gw.recordUsageProvenance("alpha", provenanceReported, 15)
	gw.recordUsageProvenance("beta", provenanceEstimated, 7)
	gw.recordUsageProvenance("beta", provenanceUnbilled, 0)
	gw.limiter.rejections.increment("user", "alice")
	gw.limiter.rejections.increment("user", "alice")
	gw.routerStats.addRequest(routerRequestKey{router: routerName, tier: "t0", reason: "score"})
	gw.routerStats.addRequest(routerRequestKey{router: routerName, tier: "t1", reason: "keyword"})
	gw.routerStats.addEscalation(routerEscalationKey{router: routerName, from: "t0", to: "t1"})
	gw.routerStats.addDecision(routerDecisionKey{router: routerName, outcome: decisionOverride})
	gw.routerStats.addDecisionTier(routerDecisionTierKey{router: routerName, heuristic: "t0", model: "t1"})
	return string(gw.renderMetrics())
}

// TestFleetMetricsNoStoreGolden pins the full no-store /metrics body. The
// golden was captured at the commit before the fleet-metrics change
// (UPDATE_GOLDEN=1 there), so a diff means the no-store output changed.
func TestFleetMetricsNoStoreGolden(t *testing.T) {
	got := noStoreGoldenScrape(t)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		require.NoError(t, os.MkdirAll("testdata", 0o750))
		require.NoError(t, os.WriteFile(noStoreGoldenPath, []byte(got), 0o600))
		return
	}
	want, err := os.ReadFile(noStoreGoldenPath)
	require.NoError(t, err)
	require.Equal(t, string(want), got)
}
