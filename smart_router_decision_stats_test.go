package traefikllmgateway

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decisionStatsPlan(outcome, heuristic, model string) *routerPlan {
	return &routerPlan{
		router: "r", reason: "score", startTier: "mid",
		entries:         []routerPlanEntry{{tier: "mid"}},
		decisionOutcome: outcome, heuristicTier: heuristic, decisionTier: model,
	}
}

func TestRecordRouterDecisionStats(t *testing.T) {
	tests := []struct {
		plan      *routerPlan
		name      string
		wantTiers int
		wantOut   int
	}{
		{decisionStatsPlan(decisionOverride, "mid", "top"), "override", 1, 1},
		{decisionStatsPlan(decisionCache, "mid", "top"), "cache", 1, 1},
		{decisionStatsPlan(decisionTimeout, "", ""), "timeout", 0, 1},
		{decisionStatsPlan(decisionOverride, "mid", ""), "tier missing", 0, 1},
		{decisionStatsPlan("", "", ""), "no call", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &Gateway{cfg: &Config{Metrics: &MetricsConfig{Enabled: true}}, routerStats: newRouterStats()}
			g.recordRouterAttempt(tt.plan, 0, cacheEndpointChat, nil)
			dec, tiers := g.routerStats.decisionSnapshot()
			assert.Len(t, dec, tt.wantOut)
			assert.Len(t, tiers, tt.wantTiers)
			if tt.wantOut == 1 {
				assert.EqualValues(t, 1, dec[routerDecisionKey{router: "r", outcome: tt.plan.decisionOutcome}])
			}
			if tt.wantTiers == 1 {
				assert.EqualValues(t, 1, tiers[routerDecisionTierKey{router: "r", heuristic: "mid", model: "top"}])
			}
		})
	}
}

func TestRouterDecisionMetricsExposition(t *testing.T) {
	g := newDecisionFixture(t, 3, "", okHandler).g
	g.cfg.Metrics = &MetricsConfig{Enabled: true}
	g.recordRouterAttempt(decisionStatsPlan(decisionOverride, "mid", "top"), 0, cacheEndpointChat, nil)
	out := scrape(g)
	assert.Contains(t, out, `llmgateway_router_decision_total{router="r",outcome="override"} 1`)
	assert.Contains(t, out, `llmgateway_router_decision_tiers_total{router="r",heuristic="mid",model="top"} 1`)
}

func TestAdminRouterDecisionView(t *testing.T) {
	f := newDecisionFixture(t, 3, "", okHandler)
	f.rt.decision.live = true
	f.g.cfg.Metrics = &MetricsConfig{Enabled: true}
	for _, p := range []*routerPlan{decisionStatsPlan(decisionOverride, "ta", "tc"), decisionStatsPlan(decisionTimeout, "", "")} {
		p.router = "auto"
		f.g.recordRouterAttempt(p, 0, cacheEndpointChat, nil)
	}
	views := f.g.buildAdminRouterViews()
	require.Len(t, views, 1)
	v := views[0]
	require.NotNil(t, v.Decision)
	assert.Equal(t, "live", v.Decision.Mode)
	assert.Equal(t, "500ms", v.Decision.Timeout)
	assert.Equal(t, "gx10", v.Decision.Provider)
	assert.Equal(t, "/v1/decide", v.Decision.Path)
	require.Len(t, v.Stats.DecisionOutcomes, len(routerDecisionOutcomes))
	for i, name := range routerDecisionOutcomes {
		assert.Equal(t, name, v.Stats.DecisionOutcomes[i].Name)
	}
	counts := map[string]int64{}
	for _, row := range v.Stats.DecisionOutcomes {
		counts[row.Name] = row.Count
	}
	assert.EqualValues(t, 1, counts[decisionOverride])
	assert.EqualValues(t, 1, counts[decisionTimeout])
	assert.EqualValues(t, 0, counts[decisionAgree])
	assert.Equal(t, []adminRouterDecisionTierView{{Heuristic: "ta", Model: "tc", Count: 1}}, v.Stats.Decision)

	f.rt.decision.live = false
	assert.Equal(t, "shadow", f.g.buildAdminRouterViews()[0].Decision.Mode)
}

func TestAdminRouterWithoutDecisionOmitsView(t *testing.T) {
	gw, _ := planTestGateway(t, nil)
	v := gw.buildAdminRouterViews()[0]
	assert.Nil(t, v.Decision)
	assert.Empty(t, v.Stats.DecisionOutcomes)
	assert.NotNil(t, v.Stats.DecisionOutcomes)
	assert.NotNil(t, v.Stats.Decision)
}

func TestRouterDecisionKeysNoText(t *testing.T) {
	k := routerDecisionKeyFor(routerDecisionKey{router: "r", outcome: decisionOverride}, "b")
	assert.Contains(t, k, metricRouterDec+routerKeySep+decisionOverride)
	m := routerDecisionTierKeyFor(routerDecisionTierKey{router: "r", heuristic: "mid", model: "top"}, "b")
	assert.Contains(t, m, metricRouterDmx+routerKeySep+"mid"+routerKeySep+"top")
	assert.False(t, strings.Contains(k+m, "hello"))
}

func TestPlanSmartRouterDecisionLiveOverride(t *testing.T) {
	f := newDecisionFixture(t, 3, "", okHandler)
	f.rt.decision.live = true
	f.rt.decision.band = 1000
	f.g.cfg.Metrics = &MetricsConfig{Enabled: true}
	plan, err := f.g.planSmartRouter(context.Background(), f.rt, userReq("hello"), cacheEndpointChat, nil, &group{}, nil)
	require.NoError(t, err)
	assert.Equal(t, smartReasonModel, plan.reason)
	assert.Equal(t, decisionOverride, plan.decisionOutcome)
	assert.Equal(t, "ta", plan.heuristicTier)
	assert.Equal(t, "tc", plan.decisionTier)
	assert.Equal(t, "tb", plan.startTier)

	f.g.recordRouterAttempt(plan, 0, cacheEndpointChat, nil)
	dec, tiers := f.g.routerStats.decisionSnapshot()
	assert.EqualValues(t, 1, dec[routerDecisionKey{router: "auto", outcome: decisionOverride}])
	assert.EqualValues(t, 1, tiers[routerDecisionTierKey{router: "auto", heuristic: "ta", model: "tc"}])
}
