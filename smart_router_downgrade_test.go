package traefikllmgateway

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// downgradeAsk scores 151 under the default weights: it sits 51 points above a tier that starts at 100.
const downgradeAsk = "write a function in python that reverses a linked list and explain the complexity"

// downgradeGateway builds three tiers (t0 a1/alpha, t1 b1/beta, t2 c1/gamma) at minScore 0/100/400
// with the downgrade on and every provider limited to one request.
func downgradeGateway(t *testing.T, mut func(*Config)) (*Gateway, *smartRouter) {
	t.Helper()
	gw, rt := planTestGateway(t, func(c *Config) {
		rc := c.SmartRouters[routerName]
		rc.KeywordRules = nil
		rc.SaturationDowngrade = true
		rc.Tiers = []SmartRouterTier{
			{Name: "t0", MinScore: 0, Models: []string{"a1"}},
			{Name: "t1", MinScore: 100, Models: []string{"b1"}},
			{Name: "t2", MinScore: 400, Models: []string{"c1"}},
		}
		if mut != nil {
			mut(c)
		}
	})
	gw.concurrency = newConcurrencyLearner(1)
	return gw, rt
}

func saturate(t *testing.T, gw *Gateway, providers ...string) {
	t.Helper()
	for _, p := range providers {
		end := gw.concurrency.begin(p)
		t.Cleanup(end)
	}
}

func planTiers(p *routerPlan) []string {
	out := make([]string, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, e.tier)
	}
	return out
}

func TestDowngradeOneTier(t *testing.T) {
	gw, rt := downgradeGateway(t, nil)
	cls := classifySmart(rt, userReq(downgradeAsk), cacheEndpointChat)
	require.Equal(t, 1, cls.rank, "the ask classifies to t1")
	require.Equal(t, 151, cls.score)

	free := planOf(t, gw, rt, userReq(downgradeAsk))
	assert.Equal(t, "t1", free.startTier, "no saturation, no downgrade")
	assert.Equal(t, smartReasonScore, free.reason)
	assert.Empty(t, free.downgradedFrom)

	saturate(t, gw, "beta")
	plan := planOf(t, gw, rt, userReq(downgradeAsk))
	assert.Equal(t, "t0", plan.startTier)
	assert.Equal(t, smartReasonSaturated, plan.reason)
	assert.Equal(t, "t1", plan.downgradedFrom)

	saturate(t, gw, "alpha")
	plan = planOf(t, gw, rt, userReq(downgradeAsk))
	assert.Equal(t, "t0", plan.startTier, "a saturated lower tier does not stop the downgrade, and it never goes two tiers down")
	assert.Equal(t, "t1", plan.downgradedFrom)

	gw3, rt3 := downgradeGateway(t, func(c *Config) {
		c.SmartRouters[routerName].Tiers[2].MinScore = 150
	})
	saturate(t, gw3, "gamma")
	top := planOf(t, gw3, rt3, userReq(downgradeAsk))
	assert.Equal(t, "t1", top.startTier, "from the top tier the request lands one tier down, not at the bottom")
	assert.Equal(t, "t2", top.downgradedFrom)
}

func TestDowngradeNeedsEverythingSaturated(t *testing.T) {
	gw, rt := downgradeGateway(t, func(c *Config) {
		c.SmartRouters[routerName].Tiers[1].Models = []string{"b1", "a2"}
	})
	saturate(t, gw, "beta")
	plan := planOf(t, gw, rt, userReq(downgradeAsk))
	assert.Equal(t, "t1", plan.startTier, "a2 on alpha still has room")
	saturate(t, gw, "alpha")
	plan = planOf(t, gw, rt, userReq(downgradeAsk))
	assert.Equal(t, "t0", plan.startTier)

	assert.False(t, smartAllSaturated(nil, nil))
	assert.False(t, smartAllSaturated([]int{2, 1}, []int{2, 2}))
	assert.False(t, smartAllSaturated([]int{5}, []int{0}), "an unknown limit is never saturated")
	assert.True(t, smartAllSaturated([]int{2, 3}, []int{2, 2}))
	assert.False(t, smartAllSaturated([]int{2}, []int{2, 2}))
}

func TestDowngradeNeedsFittingLowerTier(t *testing.T) {
	gw, rt := downgradeGateway(t, func(c *Config) {
		c.ModelMeta = map[string]*ModelMetaConfig{"a1": {ContextTokens: 5}}
	})
	saturate(t, gw, "beta")
	plan := planOf(t, gw, rt, userReq(downgradeAsk))
	assert.Equal(t, "t1", plan.startTier, "the lower tier cannot hold the prompt, so the request stays")
	assert.Equal(t, smartReasonScore, plan.reason)
	assert.Empty(t, plan.downgradedFrom)
}

func TestDowngradeOnlyScoreReason(t *testing.T) {
	gw, rt := downgradeGateway(t, func(c *Config) {
		c.SmartRouters[routerName].KeywordRules = []SmartRouterKeywordRule{{Tier: "t1", Keywords: []string{"linked list"}}}
	})
	saturate(t, gw, "beta", "gamma")
	plan := planOf(t, gw, rt, userReq(downgradeAsk))
	assert.Equal(t, "t1", plan.startTier, "a keyword that names the tier is a floor")
	assert.Equal(t, smartReasonScore, plan.reason, "the score already reached the keyword's tier")
	assert.Empty(t, plan.downgradedFrom)

	rt.rules[0].rank = 2
	plan = planOf(t, gw, rt, userReq(downgradeAsk))
	assert.Equal(t, "t2", plan.startTier)
	assert.Equal(t, smartReasonKeyword, plan.reason)
	assert.Empty(t, plan.downgradedFrom)

	rt.rules = nil
	floor := map[string]any{"reasoning_effort": "high", "messages": []any{map[string]any{"role": "user", "content": downgradeAsk}}}
	plan = planOf(t, gw, rt, floor)
	assert.Equal(t, "t2", plan.startTier)
	assert.Equal(t, smartReasonFloor, plan.reason)
	assert.Empty(t, plan.downgradedFrom)

	empty := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "be brief"}}}
	rt.defaultRank = 1
	plan = planOf(t, gw, rt, empty)
	assert.Equal(t, smartReasonDefault, plan.reason)
	assert.Equal(t, "t1", plan.startTier)
	assert.Empty(t, plan.downgradedFrom, "the default tier is not a score decision")
}

func TestDowngradeNotWithSessionReasons(t *testing.T) {
	gw, rt, _ := sessionGateway(t, func(c *SmartRouterConfig) { c.SaturationDowngrade = true })
	gw.concurrency = newConcurrencyLearner(1)
	first := planOf(t, gw, rt, sessionReq("u1", routerKeyword))
	require.Equal(t, "t2", first.startTier)
	saturate(t, gw, "gamma", "beta", "alpha")
	second := planOf(t, gw, rt, sessionReq("u1", routerKeyword, "thanks"))
	assert.Equal(t, smartReasonSession, second.reason)
	assert.Equal(t, "t2", second.startTier, "a session pin is never lowered")
	assert.Empty(t, second.downgradedFrom)
}

func TestDowngradeRespectsFloors(t *testing.T) {
	gw, rt := downgradeGateway(t, func(c *Config) { c.SmartRouters[routerName].ToolsMinTier = "t1" })
	saturate(t, gw, "beta")
	withTools := map[string]any{
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "f"}}},
		"messages": []any{map[string]any{"role": "user", "content": downgradeAsk}},
	}
	plan := planOf(t, gw, rt, withTools)
	assert.Equal(t, "t1", plan.startTier, "toolsMinTier is a floor the downgrade cannot cross")
	assert.Empty(t, plan.downgradedFrom)
	plain := planOf(t, gw, rt, userReq(downgradeAsk))
	assert.Equal(t, "t0", plain.startTier, "the same ask without tools has no floor")

	gwK, rtK := downgradeGateway(t, func(c *Config) {
		c.SmartRouters[routerName].KeywordRules = []SmartRouterKeywordRule{{Tier: "t1", Keywords: []string{"reverses"}}}
	})
	saturate(t, gwK, "beta")
	assert.Equal(t, "t1", planOf(t, gwK, rtK, userReq(downgradeAsk)).startTier, "a keyword floor at the tier holds even when the score gives the same tier")

	gwB, rtB := downgradeGateway(t, func(c *Config) { v := 50; c.SmartRouters[routerName].DowngradeBand = &v })
	saturate(t, gwB, "beta")
	assert.Equal(t, "t1", planOf(t, gwB, rtB, userReq(downgradeAsk)).startTier, "151 is not below 100 + 50")
	rtB.downgradeBand = 51
	assert.Equal(t, "t1", planOf(t, gwB, rtB, userReq(downgradeAsk)).startTier, "the band is exclusive: 151 is not below 100 + 51")
	rtB.downgradeBand = 52
	assert.Equal(t, "t0", planOf(t, gwB, rtB, userReq(downgradeAsk)).startTier, "151 is below 100 + 52")
}

func TestDowngradeKeepsEscalation(t *testing.T) {
	gw, rt := downgradeGateway(t, nil)
	saturate(t, gw, "beta")
	plan := planOf(t, gw, rt, userReq(downgradeAsk))
	assert.Equal(t, []string{"t0", "t1", "t2"}, planTiers(plan), "the classified and higher tiers stay in the plan")
	assert.Equal(t, "beta/b1", plan.entries[1].cand.canonical)
}

func TestDowngradeOffIsIdentical(t *testing.T) {
	gw, rt := downgradeGateway(t, func(c *Config) { c.SmartRouters[routerName].SaturationDowngrade = false })
	saturate(t, gw, "beta", "alpha", "gamma")
	plan := planOf(t, gw, rt, userReq(downgradeAsk))
	assert.Equal(t, "t1", plan.startTier)
	assert.Equal(t, smartReasonScore, plan.reason)
	assert.Empty(t, plan.downgradedFrom)
	h := http.Header{}
	setRouterHeaders(h, plan, 0)
	assert.Empty(t, h.Values(headerRouterDowngraded))

	gwNoConc, rtNoConc := downgradeGateway(t, nil)
	gwNoConc.concurrency = nil
	assert.Equal(t, "t1", planOf(t, gwNoConc, rtNoConc, userReq(downgradeAsk)).startTier, "without concurrency tracking nothing is saturated")
}

func TestDowngradeHeaderAndCounter(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	t.Cleanup(func() { closeBackends(a, b, c) })
	cfg := routerMetricsConfig(a, b, c)
	rc := cfg.SmartRouters[routerName]
	rc.KeywordRules = nil
	rc.SaturationDowngrade = true
	rc.Tiers = []SmartRouterTier{
		{Name: "t0", MinScore: 0, Models: []string{"a1"}},
		{Name: "t1", MinScore: 100, Models: []string{"b1"}},
		{Name: "t2", MinScore: 400, Models: []string{"c1"}},
	}
	h, gw := buildRouterGateway(t, cfg)
	gw.concurrency = newConcurrencyLearner(1)
	saturate(t, gw, "beta")

	rec := routerChat(t, h, routerName, downgradeAsk, nil)
	require.Equal(t, 200, rec.Code)
	assert.Equal(t, "t0", rec.Header().Get(headerRouterTier))
	assert.Equal(t, "t1", rec.Header().Get(headerRouterDowngraded))
	assert.EqualValues(t, 1, a.count())
	assert.EqualValues(t, 0, b.count())
	reqs, _ := gw.routerStats.snapshot()
	assert.EqualValues(t, 1, reqs[routerRequestKey{router: routerName, tier: "t0", reason: smartReasonSaturated}])
	assert.EqualValues(t, 0, reqs[routerRequestKey{router: routerName, tier: "t0", reason: smartReasonScore}])

	views := gw.buildAdminRouterViews()
	require.Len(t, views, 1)
	assert.True(t, views[0].SaturationDowngrade)
	assert.Equal(t, defaultSmartDowngradeBand, views[0].DowngradeBand)
	assert.EqualValues(t, 1, views[0].Downgraded)

	// An escalation back to the classified tier drops the header: the request is no longer downgraded.
	a2 := newRouterBackend(500, routerFail)
	b2 := newRouterBackend(200, successRespBody)
	c2 := newRouterBackend(200, successRespBody)
	t.Cleanup(func() { closeBackends(a2, b2, c2) })
	cfg2 := routerMetricsConfig(a2, b2, c2)
	rc2 := cfg2.SmartRouters[routerName]
	rc2.KeywordRules = nil
	rc2.SaturationDowngrade = true
	rc2.Tiers = rc.Tiers
	h2, gw2 := buildRouterGateway(t, cfg2)
	gw2.concurrency = newConcurrencyLearner(1)
	saturate(t, gw2, "beta")
	rec2 := routerChat(t, h2, routerName, downgradeAsk, nil)
	require.Equal(t, 200, rec2.Code)
	assert.Equal(t, "t1", rec2.Header().Get(headerRouterTier), "the failed t0 attempt escalated to the classified tier")
	assert.Empty(t, rec2.Header().Values(headerRouterDowngraded))
	assert.EqualValues(t, 1, a2.count())
	assert.EqualValues(t, 1, b2.count())
}

func TestDowngradeConfigValidation(t *testing.T) {
	t.Parallel()
	one := []SmartRouterTier{{Name: "a", Models: []string{"p/a"}}}
	_, err := compileSmartRouter("r", &SmartRouterConfig{SaturationDowngrade: true, Tiers: one}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "two tiers")
	_, err = compileSmartRouter("r", &SmartRouterConfig{Tiers: one}, nil)
	require.NoError(t, err, "a single tier without the downgrade is fine")
	for _, band := range []int{-1, 301} {
		v := band
		_, err = compileSmartRouter("r", &SmartRouterConfig{DowngradeBand: &v, Tiers: one}, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "downgradeBand")
	}
	zero := 0
	rt, err := compileSmartRouter("r", &SmartRouterConfig{DowngradeBand: &zero, Tiers: one}, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, rt.downgradeBand)
	rt, err = compileSmartRouter("r", &SmartRouterConfig{Tiers: one}, nil)
	require.NoError(t, err)
	assert.Equal(t, 100, rt.downgradeBand)
	assert.False(t, rt.satDowngrade)
}

func TestRouterReasonNamesIncludeSaturated(t *testing.T) {
	t.Parallel()
	assert.Equal(t, smartReasonSaturated, routerReasonNames[routerReasonsMax-1])
	assert.Equal(t, smartReasonScore, routerReasonNames[0])
}

func TestDowngradeNeverStartsBelowSessionPin(t *testing.T) {
	gw, rt, _ := sessionGateway(t, func(c *SmartRouterConfig) {
		c.KeywordRules = nil
		c.SaturationDowngrade = true
		band := 150
		c.DowngradeBand = &band
		c.Tiers = []SmartRouterTier{
			{Name: "t0", MinScore: 0, Models: []string{"a1"}},
			{Name: "t1", MinScore: 200, Models: []string{"b1"}},
			{Name: "t2", MinScore: 400, Models: []string{"c1"}},
		}
	})
	gw.concurrency = newConcurrencyLearner(1)
	first := planOf(t, gw, rt, sessionReq("u1", downgradeAsk))
	require.Equal(t, "t1", first.startTier)
	require.Equal(t, 1, onlySession(t, gw).pin)

	saturate(t, gw, "beta")
	second := planOf(t, gw, rt, sessionReq("u1", "an earlier question", downgradeAsk))
	assert.Equal(t, "t1", second.startTier, "the session pin is a floor the downgrade cannot cross")
	assert.Empty(t, second.downgradedFrom)
}
