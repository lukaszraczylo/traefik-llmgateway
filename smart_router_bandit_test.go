package traefikllmgateway

import (
	"errors"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// banditGateway builds the "auto" router with a t0 of three models over two providers and a t1 of c1.
func banditGateway(t *testing.T, selection string, adaptive *SmartRouterAdaptiveConfig) (*Gateway, *smartRouter) {
	t.Helper()
	gw, rt := planTestGateway(t, func(c *Config) {
		rc := c.SmartRouters[routerName]
		rc.KeywordRules = nil
		rc.Selection = selection
		rc.Adaptive = adaptive
		rc.MaxAttempts = 8
		rc.Tiers = []SmartRouterTier{
			{Name: "t0", MinScore: 0, Models: []string{"a1", "a2", "b1"}},
			{Name: "t1", MinScore: 300, Models: []string{"c1"}},
		}
	})
	gw.limiter.spawn = syncSpawn
	return gw, rt
}

func zeroPct() *int { v := 0; return &v }

func armOf(rt *smartRouter, tier string, cand resolveCandidate) *smartArm {
	rt.bandit.mu.Lock()
	defer rt.bandit.mu.Unlock()
	return rt.bandit.armLocked(tier, cand)
}

func tierCands(t *testing.T, gw *Gateway, rt *smartRouter, rank int) []resolveCandidate {
	t.Helper()
	var skips routerSkips
	return gw.routerTierCandidates(rt, rank, "hi", routerRequestFacts{}, &group{}, &skips)
}

func keysOf(cands []resolveCandidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, routerCandidateKey(c))
	}
	return out
}

func TestBanditPermutationOnly(t *testing.T) {
	ordGw, ordRt := banditGateway(t, smartSelectionOrdered, nil)
	adGw, adRt := banditGateway(t, smartSelectionAdaptive, nil)
	adRt.bandit.rnd = newSmartRandom(7)
	for _, ask := range []string{"hi", "please review the architecture trade-offs of this distributed system design and prove its invariant"} {
		want, err := ordGw.planSmartRouter(ordRt, userReq(ask), cacheEndpointChat, nil, &group{}, nil)
		require.NoError(t, err)
		wantKeys := planShape(want)
		sort.Strings(wantKeys)
		wantTiers := make([]string, 0, len(want.entries))
		for _, e := range want.entries {
			wantTiers = append(wantTiers, e.tier)
		}
		for i := 0; i < 100; i++ {
			got, err := adGw.planSmartRouter(adRt, userReq(ask), cacheEndpointChat, nil, &group{}, nil)
			require.NoError(t, err)
			gotKeys := planShape(got)
			sort.Strings(gotKeys)
			assert.Equal(t, wantKeys, gotKeys, "same candidates, never added or dropped")
			gotTiers := make([]string, 0, len(got.entries))
			for _, e := range got.entries {
				gotTiers = append(gotTiers, e.tier)
			}
			assert.Equal(t, wantTiers, gotTiers, "tier order and tier of every slot are unchanged")
			assert.Equal(t, want.startTier, got.startTier)
			assert.Equal(t, want.reason, got.reason)
		}
	}
}

func TestBanditOrderedUnchangedWhenOff(t *testing.T) {
	plain, plainRt := banditGateway(t, "", nil)
	hash, hashRt := banditGateway(t, smartSelectionHash, &SmartRouterAdaptiveConfig{WindowHours: 3})
	require.Nil(t, plainRt.bandit)
	require.Nil(t, hashRt.bandit, "adaptive settings alone never enable the bandit")
	for _, ask := range []string{"hi", "hello there", "summarize this"} {
		a, err := plain.planSmartRouter(plainRt, userReq(ask), cacheEndpointChat, nil, &group{}, nil)
		require.NoError(t, err)
		assert.Nil(t, a.bandit)
		b, err := hash.planSmartRouter(hashRt, userReq(ask), cacheEndpointChat, nil, &group{}, nil)
		require.NoError(t, err)
		assert.ElementsMatch(t, planShape(a), planShape(b))
	}
	assert.False(t, plain.routerLearn.running, "an ordered router never starts the ticker")
}

func TestBanditColdStartExplores(t *testing.T) {
	gw, rt := banditGateway(t, smartSelectionAdaptive, &SmartRouterAdaptiveConfig{ExplorePct: zeroPct()})
	rt.bandit.rnd = newSmartRandom(1)
	lead := map[string]int{}
	for i := 0; i < 900; i++ {
		got := tierCands(t, gw, rt, 0)
		require.Len(t, got, 3)
		lead[routerCandidateKey(got[0])]++
	}
	assert.Len(t, lead, 3, "an arm with no data still leads sometimes")
	for k, n := range lead {
		assert.Greater(t, n, 150, k)
	}
}

func seedArm(rt *smartRouter, tier string, cand resolveCandidate, a, n int64) {
	rt.bandit.mu.Lock()
	arm := rt.bandit.armLocked(tier, cand)
	arm.aMilli, arm.nMilli = a*banditMilli, n*banditMilli
	rt.bandit.mu.Unlock()
}

func TestBanditExploreShare(t *testing.T) {
	gw, rt := banditGateway(t, smartSelectionAdaptive, &SmartRouterAdaptiveConfig{ExplorePct: ptrInt(20)})
	rt.bandit.rnd = newSmartRandom(99)
	cands := tierCands(t, gw, rt, 0)
	best := routerCandidateKey(cands[0])
	for i, c := range cands {
		if i == 0 {
			seedArm(rt, "t0", c, 990, 1000)
		} else {
			seedArm(rt, "t0", c, 10, 1000)
		}
	}
	const draws = 20000
	notBest := 0
	for i := 0; i < draws; i++ {
		got := gw.banditOrder(rt, "t0", cands)
		if routerCandidateKey(got[0]) != best {
			notBest++
		}
	}
	share := float64(notBest) / draws
	assert.InDelta(t, 0.2*2/3, share, 0.015, "explore picks uniformly over the three arms, so two thirds of picks leave the best arm")

	rt.bandit.cfg.explorePct = 0
	notBest = 0
	for i := 0; i < draws; i++ {
		got := gw.banditOrder(rt, "t0", cands)
		if routerCandidateKey(got[0]) != best {
			notBest++
		}
	}
	assert.Zero(t, notBest, "with explore off a clearly better arm always leads")
}

func ptrInt(v int) *int { return &v }

func TestBanditSaturatedLast(t *testing.T) {
	gw, rt := banditGateway(t, smartSelectionAdaptive, &SmartRouterAdaptiveConfig{ExplorePct: ptrInt(50)})
	rt.bandit.rnd = newSmartRandom(5)
	cands := tierCands(t, gw, rt, 0)
	require.Len(t, cands, 3)
	var full []int
	for i, c := range cands {
		if c.providerName == "alpha" {
			full = append(full, i)
		}
	}
	require.Len(t, full, 2)
	// Make the saturated provider look best: it must still go last.
	for _, c := range cands {
		if c.providerName == "alpha" {
			seedArm(rt, "t0", c, 1000, 1000)
		} else {
			seedArm(rt, "t0", c, 0, 1000)
		}
	}
	load := func(p string) (int, int) {
		if p == "alpha" {
			return 4, 4
		}
		return 1, 4
	}
	for i := 0; i < 500; i++ {
		got := rt.bandit.order("t0", cands, load)
		require.Len(t, got, 3)
		assert.Equal(t, "beta", got[0].providerName, "explore and score both pick only unsaturated arms first")
		assert.Equal(t, "alpha", got[1].providerName)
		assert.Equal(t, "alpha", got[2].providerName)
	}
	allFull := func(string) (int, int) { return 2, 2 }
	got := rt.bandit.order("t0", cands, allFull)
	assert.ElementsMatch(t, keysOf(cands), keysOf(got), "a fully saturated tier is still a permutation")
}

func TestBanditLearnsFromCounters(t *testing.T) {
	gw, rt := banditGateway(t, smartSelectionAdaptive, &SmartRouterAdaptiveConfig{ExplorePct: zeroPct()})
	rt.bandit.rnd = newSmartRandom(3)
	cands := tierCands(t, gw, rt, 0)
	good, bad := cands[0], cands[1]
	now := gw.limiter.now()
	bucket := bucketFor(now, windowHour)
	_, ok := gw.limiter.storeIncrMulti([]counterIncr{
		{key: routerBanditKeyFor(routerName, "t0", smartArmHash(good), "a", bucket), delta: 480000, ttl: banditCounterTTL},
		{key: routerBanditKeyFor(routerName, "t0", smartArmHash(good), "n", bucket), delta: 500000, ttl: banditCounterTTL},
		{key: routerBanditKeyFor(routerName, "t0", smartArmHash(bad), "a", bucket), delta: 50000, ttl: banditCounterTTL},
		{key: routerBanditKeyFor(routerName, "t0", smartArmHash(bad), "n", bucket), delta: 500000, ttl: banditCounterTTL},
	})
	require.True(t, ok)
	gw.refreshRouterLearning(now)
	assert.Equal(t, int64(480000), armOf(rt, "t0", good).aMilli)
	wins := 0
	for i := 0; i < 200; i++ {
		if routerCandidateKey(gw.banditOrder(rt, "t0", cands)[0]) == routerCandidateKey(good) {
			wins++
		}
	}
	assert.Greater(t, wins, 150, "the arm with the better reward leads")
	assert.NotEqual(t, routerCandidateKey(bad), routerCandidateKey(gw.banditOrder(rt, "t0", cands)[0]))

	old := now.Add(-time.Duration(rt.bandit.cfg.windowHours+1) * time.Hour)
	_, ok = gw.limiter.storeIncrMulti([]counterIncr{{key: routerBanditKeyFor(routerName, "t0", smartArmHash(bad), "n", bucketFor(old, windowHour)), delta: 777000, ttl: banditCounterTTL}})
	require.True(t, ok)
	gw.refreshRouterLearning(now)
	assert.Equal(t, int64(500000), armOf(rt, "t0", bad).nMilli, "a bucket outside the window does not count")
}

func TestBanditObserveCountsReward(t *testing.T) {
	gw, rt := banditGateway(t, smartSelectionAdaptive, &SmartRouterAdaptiveConfig{ExplorePct: zeroPct()})
	plan, err := gw.planSmartRouter(rt, userReq("hi"), cacheEndpointChat, nil, &group{}, nil)
	require.NoError(t, err)
	require.NotNil(t, plan.bandit)
	read := func(i int) (int64, int64) {
		e := plan.entries[i]
		bucket := bucketFor(gw.limiter.now(), windowHour)
		v, ok := gw.limiter.storeGetMulti([]string{
			routerBanditKeyFor(routerName, e.tier, smartArmHash(e.cand), "a", bucket),
			routerBanditKeyFor(routerName, e.tier, smartArmHash(e.cand), "n", bucket),
		})
		require.True(t, ok)
		return v[0], v[1]
	}
	gw.observeBandit(plan, 0, nil, false, latencySample{}, false)
	a, n := read(0)
	assert.Equal(t, int64(1000), a, "a clean answer earns the full reward")
	assert.Equal(t, int64(1000), n)

	gw.observeBandit(plan, 0, nil, true, latencySample{}, false)
	a, n = read(0)
	assert.Equal(t, int64(1000), a, "a bad answer earns nothing")
	assert.Equal(t, int64(2000), n)

	gw.observeBandit(plan, 1, errors.New("boom"), false, latencySample{}, false)
	a, n = read(1)
	assert.Equal(t, int64(0), a, "an upstream error earns nothing")
	assert.Equal(t, int64(1000), n)

	gw.observeBandit(plan, 2, &providerHTTPError{status: 400}, false, latencySample{}, false)
	_, n = read(2)
	assert.Zero(t, n, "a 400 is the request's fault, not the model's")
}

func TestBanditLatencyPenalty(t *testing.T) {
	t.Parallel()
	b := newSmartBandit(smartAdaptiveCfg{explorePct: 0, windowHours: 6, latencyPct: 30})
	slow := resolveCandidate{providerName: "p", upstreamModel: "slow"}
	fast := resolveCandidate{providerName: "p", upstreamModel: "fast"}
	assert.InDelta(t, 1.0, b.observe("t", fast, false, 1.0), 1e-9, "first sample has no tier average to compare with")
	assert.InDelta(t, 1.0, b.observe("t", fast, false, 1.0), 1e-9)
	assert.InDelta(t, 0.85, b.observe("t", slow, false, 1.5), 1e-9, "50% over the tier average costs 0.3*0.5")
	assert.Greater(t, armOf2(b, "t", slow).ttfbEWMA, 0.0)
	assert.Equal(t, 0.0, b.observe("t", slow, true, 9))
}

func armOf2(b *smartBandit, tier string, cand resolveCandidate) *smartArm {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.armLocked(tier, cand)
}

func TestBanditArmsBounded(t *testing.T) {
	t.Parallel()
	b := newSmartBandit(smartAdaptiveCfg{windowHours: 6})
	for i := 0; i < banditArmsMax+50; i++ {
		b.observe("t", resolveCandidate{providerName: "p", upstreamModel: "m" + strconv.Itoa(i)}, false, 0.1)
	}
	assert.LessOrEqual(t, len(b.arms), banditArmsMax)
	assert.InDelta(t, 1.0, b.observe("t", resolveCandidate{providerName: "p", upstreamModel: "overflow"}, false, 0), 1e-9, "a full arm table still returns a reward")
}

func TestBanditThompsonScore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		a, n, z, util float64
		want          float64
	}{
		{"prior mean", 0, 0, 0, 0, 2.0 / 3.0},
		{"data mean", 90, 100, 0, 0, 92.0 / 103.0},
		{"half load", 90, 100, 0, 1, 92.0 / 103.0 * 0.5},
		{"quarter util", 90, 100, 0, 0.5, 92.0 / 103.0 * 0.875},
		{"negative draw floors at zero", 0, 1000, -50, 0, 0},
		{"a above n is clamped", 20, 10, 0, 0, 12.0 / 13.0},
	}
	for _, tt := range tests {
		assert.InDelta(t, tt.want, smartThompsonScore(tt.a, tt.n, tt.z, tt.util), 1e-9, tt.name)
	}
	assert.Greater(t, smartThompsonScore(5, 10, 1, 0), smartThompsonScore(5, 10, 0, 0))
	assert.Greater(t, smartThompsonScore(5, 10, 1, 0), smartThompsonScore(5, 10, -1, 0))
}

func TestBanditRewardAndSaturationHelpers(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0.0, smartBanditReward(true, 1, 1, 30))
	assert.Equal(t, 1.0, smartBanditReward(false, 0, 1, 30))
	assert.Equal(t, 1.0, smartBanditReward(false, 1, 0, 30))
	assert.Equal(t, 1.0, smartBanditReward(false, 0.5, 1, 30), "faster than average costs nothing")
	assert.InDelta(t, 0.7, smartBanditReward(false, 5, 1, 30), 1e-9, "the penalty caps at the weight")
	assert.InDelta(t, 0.5, smartBanditReward(false, 5, 1, 50), 1e-9)
	assert.True(t, smartSaturated(4, 4))
	assert.True(t, smartSaturated(9, 4))
	assert.False(t, smartSaturated(3, 4))
	assert.False(t, smartSaturated(100, 0), "an unknown limit is never saturated")
	assert.Equal(t, 0.0, smartUtil(5, 0))
	assert.Equal(t, 0.5, smartUtil(2, 4))
	assert.Equal(t, 1.0, smartUtil(9, 4))
}

func TestBanditOrderIsPermutation(t *testing.T) {
	t.Parallel()
	scores := []float64{0.2, 0.9, 0.5, 0.7}
	sat := []bool{false, true, false, false}
	assert.Equal(t, []int{3, 2, 0, 1}, smartBanditOrder(scores, sat, -1))
	assert.Equal(t, []int{0, 3, 2, 1}, smartBanditOrder(scores, sat, 0))
	got := smartBanditOrder(scores, sat, 2)
	sorted := append([]int(nil), got...)
	sort.Ints(sorted)
	assert.Equal(t, []int{0, 1, 2, 3}, sorted)
}

func TestBanditConfigValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  *SmartRouterAdaptiveConfig
		want string
	}{
		{"explore high", &SmartRouterAdaptiveConfig{ExplorePct: ptrInt(51)}, "explorePct"},
		{"explore negative", &SmartRouterAdaptiveConfig{ExplorePct: ptrInt(-1)}, "explorePct"},
		{"window high", &SmartRouterAdaptiveConfig{WindowHours: 13}, "windowHours"},
		{"latency high", &SmartRouterAdaptiveConfig{LatencyWeightPct: ptrInt(101)}, "latencyWeightPct"},
	}
	for _, tt := range tests {
		_, err := compileSmartAdaptive("r", tt.cfg)
		require.Error(t, err, tt.name)
		assert.Contains(t, err.Error(), tt.want, tt.name)
	}
	c, err := compileSmartAdaptive("r", nil)
	require.NoError(t, err)
	assert.Equal(t, smartAdaptiveCfg{explorePct: 5, windowHours: 6, latencyPct: 30}, c)
	c, err = compileSmartAdaptive("r", &SmartRouterAdaptiveConfig{ExplorePct: zeroPct(), LatencyWeightPct: zeroPct(), WindowHours: 12})
	require.NoError(t, err)
	assert.Equal(t, smartAdaptiveCfg{explorePct: 0, windowHours: 12, latencyPct: 0}, c)
}

func TestBanditSelectionValidationAndAdminView(t *testing.T) {
	gw, rt := banditGateway(t, smartSelectionAdaptive, &SmartRouterAdaptiveConfig{ExplorePct: ptrInt(10)})
	require.NotNil(t, rt.bandit)
	v := gw.buildAdminRouterViews()[0]
	assert.Equal(t, smartSelectionAdaptive, v.Selection)
	require.NotNil(t, v.Adaptive)
	assert.Equal(t, adminRouterAdaptiveView{ExplorePct: 10, WindowHours: 6, LatencyWeightPct: 30}, *v.Adaptive)

	offGw, _ := banditGateway(t, smartSelectionOrdered, nil)
	assert.Nil(t, offGw.buildAdminRouterViews()[0].Adaptive)

	_, err := compileSmartRouter("r", &SmartRouterConfig{Selection: "random", Tiers: []SmartRouterTier{{Name: "a", Models: []string{"p/a"}}}}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "adaptive")
}

func TestBanditStartsLearningTicker(t *testing.T) {
	gw, rt := banditGateway(t, smartSelectionAdaptive, nil)
	_, err := gw.planSmartRouter(rt, userReq("hi"), cacheEndpointChat, nil, &group{}, nil)
	require.NoError(t, err)
	assert.True(t, gw.routerLearn.running)
	require.NoError(t, gw.Close())
}

func TestBanditCountsServedRequest(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	t.Cleanup(func() { closeBackends(a, b, c) })
	conf := routerTestConfig(a, b, c, threeTiers())
	conf.SmartRouters[routerName].Selection = smartSelectionAdaptive
	h, gw := buildRouterGateway(t, conf)
	gw.limiter.spawn = syncSpawn
	rec := routerChat(t, h, routerName, "hi", nil)
	require.Equal(t, 200, rec.Code)
	rt := gw.registry.routers[routerName]
	rt.bandit.mu.Lock()
	var arm *smartArm
	for _, x := range rt.bandit.arms {
		arm = x
	}
	rt.bandit.mu.Unlock()
	require.NotNil(t, arm)
	bucket := bucketFor(gw.limiter.now(), windowHour)
	v, ok := gw.limiter.storeGetMulti([]string{
		routerBanditKeyFor(routerName, "t0", smartArmHash(resolveCandidate{providerName: "alpha", upstreamModel: "a1"}), "a", bucket),
		routerBanditKeyFor(routerName, "t0", smartArmHash(resolveCandidate{providerName: "alpha", upstreamModel: "a1"}), "n", bucket),
	})
	require.True(t, ok)
	assert.Equal(t, []int64{1000, 1000}, v, "one served request adds one full-reward sample to its arm")
}
