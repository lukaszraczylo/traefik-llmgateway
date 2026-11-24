package traefikllmgateway

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func calibGateway(t *testing.T, cal *SmartRouterCalibrationConfig) (*Gateway, *smartRouter) {
	t.Helper()
	gw, rt := planTestGateway(t, func(c *Config) {
		c.SmartRouters[routerName].Calibration = cal
	})
	gw.limiter.spawn = syncSpawn
	return gw, rt
}

// seedCalib writes n requests and bad bad answers for a tier into the current hourly bucket.
func seedCalib(t *testing.T, gw *Gateway, tier string, n, bad int64, hoursAgo int) {
	t.Helper()
	bucket := bucketFor(gw.limiter.now().Add(-time.Duration(hoursAgo)*time.Hour), windowHour)
	_, ok := gw.limiter.storeIncrMulti([]counterIncr{
		{key: routerCalibKeyFor(routerName, tier, "n", bucket), delta: n, ttl: hourWindowTTL},
		{key: routerCalibKeyFor(routerName, tier, "bad", bucket), delta: bad, ttl: hourWindowTTL},
	})
	require.True(t, ok)
}

func TestWilsonBounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		bad, n       int64
		wantLo, want float64
	}{
		{"empty is vacuous", 0, 0, 0, 1},
		{"all good", 0, 100, 0, 0.0370},
		{"half of ten", 5, 10, 0.2366, 0.7634},
		{"one in hundred", 1, 100, 0.0018, 0.0545},
		{"all bad", 100, 100, 0.9630, 1},
	}
	for _, tt := range tests {
		lo, hi := smartWilsonBounds(tt.bad, tt.n, 1.96)
		assert.InDelta(t, tt.wantLo, lo, 0.0005, tt.name)
		assert.InDelta(t, tt.want, hi, 0.0005, tt.name)
	}
	lo, hi := smartWilsonBounds(10, 100, 1.96)
	assert.Less(t, lo, 0.10)
	assert.Greater(t, hi, 0.10)
}

func TestCalibBounded(t *testing.T) {
	t.Parallel()
	maxLower := 60
	_, st, err := compileSmartCalibration("r", &SmartRouterCalibrationConfig{Mode: smartCalibLive, MaxLower: &maxLower, Step: 25}, threeTierStructs())
	require.NoError(t, err)
	require.NotNil(t, st)
	c := smartCalibCfg{mode: smartCalibLive, maxLower: maxLower, step: 25, targetBadPct: 3, minSamples: 200}
	cur := 0
	for i := 0; i < 20; i++ {
		cur = smartCalibStep(cur, 500, 1000, &c)
		assert.LessOrEqual(t, cur, maxLower)
	}
	assert.Equal(t, maxLower, cur)
	assert.Equal(t, 25, smartCalibStep(0, 500, 1000, &c), "one step per recompute")
}

func TestCalibNeedsEvidence(t *testing.T) {
	t.Parallel()
	c := smartCalibCfg{mode: smartCalibLive, maxLower: 150, step: 25, targetBadPct: 3, minSamples: 200}
	assert.Equal(t, 0, smartCalibStep(0, 100, 150, &c), "below minSamples")
	assert.Equal(t, 0, smartCalibStep(0, 7, 200, &c), "3.5% with a Wilson lower bound under 3%")
	assert.Equal(t, 25, smartCalibStep(0, 30, 200, &c), "15% clears the bound")
}

func TestCalibOnlyLowersByDefault(t *testing.T) {
	t.Parallel()
	c := smartCalibCfg{mode: smartCalibLive, maxLower: 150, step: 25, targetBadPct: 3, minSamples: 200}
	assert.Equal(t, 0, smartCalibStep(0, 0, 5000, &c), "a clean tier never raises a threshold")
	cur := 40
	for i := 0; i < 10; i++ {
		cur = smartCalibStep(cur, 0, 5000, &c)
		assert.GreaterOrEqual(t, cur, 0)
	}
	assert.Equal(t, 0, cur, "relaxation stops at zero")

	c.maxRaise = 30
	assert.Equal(t, -10, smartCalibStep(0, 0, 5000, &c))
	assert.Equal(t, -30, smartCalibStep(-30, 0, 5000, &c), "raise is capped")
}

func TestCalibFreeze(t *testing.T) {
	gw, rt := calibGateway(t, &SmartRouterCalibrationConfig{Mode: smartCalibLive})
	seedCalib(t, gw, "t0", 1000, 500, 0)
	gw.refreshRouterLearning(gw.limiter.now())
	require.Equal(t, 25, rt.calibState.get(1))
	assert.Equal(t, 275, rt.tierMin(1))

	rt.calib.freeze = true
	assert.Equal(t, 300, rt.tierMin(1), "freeze applies at once")
	gw.refreshRouterLearning(gw.limiter.now())
	assert.Equal(t, []int{0, 0, 0}, rt.calibState.snapshot())
	rt.calib.freeze = false
	assert.Equal(t, 300, rt.tierMin(1))
}

func TestCalibShadowNoEffect(t *testing.T) {
	shadowGw, shadowRt := calibGateway(t, &SmartRouterCalibrationConfig{Mode: smartCalibShadow})
	offGw, offRt := calibGateway(t, nil)
	seedCalib(t, shadowGw, "t0", 1000, 500, 0)
	shadowGw.refreshRouterLearning(shadowGw.limiter.now())
	require.Equal(t, 25, shadowRt.calibState.get(1), "shadow still computes the offset")
	assert.Equal(t, 300, shadowRt.tierMin(1))

	req := userReq(sessionLongAsk())
	a, err := shadowGw.planSmartRouter(shadowRt, req, cacheEndpointChat, nil, &group{}, nil)
	require.NoError(t, err)
	b, err := offGw.planSmartRouter(offRt, req, cacheEndpointChat, nil, &group{}, nil)
	require.NoError(t, err)
	assert.Equal(t, planShape(b), planShape(a))
	assert.Equal(t, b.reason, a.reason)
	for score := 0; score <= 1000; score += 5 {
		require.Equal(t, smartTierForScore(offRt, score), smartTierForScore(shadowRt, score), "score %d", score)
	}
}

func TestCalibLiveLowersThreshold(t *testing.T) {
	gw, rt := calibGateway(t, &SmartRouterCalibrationConfig{Mode: smartCalibLive})
	assert.Equal(t, 0, smartTierForScore(rt, 290))
	seedCalib(t, gw, "t0", 1000, 500, 0)
	gw.refreshRouterLearning(gw.limiter.now())
	assert.Equal(t, 1, smartTierForScore(rt, 290))
	assert.Equal(t, 0, smartTierForScore(rt, 270))
}

func TestCalibReadsLast24Buckets(t *testing.T) {
	gw, rt := calibGateway(t, &SmartRouterCalibrationConfig{Mode: smartCalibLive})
	seedCalib(t, gw, "t0", 1000, 500, 23)
	gw.refreshRouterLearning(gw.limiter.now())
	assert.Equal(t, 25, rt.calibState.get(1), "bucket 23 hours back counts")

	gw2, rt2 := calibGateway(t, &SmartRouterCalibrationConfig{Mode: smartCalibLive})
	seedCalib(t, gw2, "t0", 1000, 500, 24)
	gw2.refreshRouterLearning(gw2.limiter.now())
	assert.Equal(t, 0, rt2.calibState.get(1), "bucket 24 hours back is outside the window")
}

func TestCalibCountsOnlyScoreOutcomes(t *testing.T) {
	gw, _ := calibGateway(t, &SmartRouterCalibrationConfig{Mode: smartCalibShadow})
	read := func() (int64, int64) {
		bucket := bucketFor(gw.limiter.now(), windowHour)
		v, ok := gw.limiter.storeGetMulti([]string{routerCalibKeyFor(routerName, "t0", "n", bucket), routerCalibKeyFor(routerName, "t0", "bad", bucket)})
		require.True(t, ok)
		return v[0], v[1]
	}
	mk := func(reason string, tiers ...string) *routerPlan {
		p := &routerPlan{router: routerName, reason: reason, startTier: "t0", calibrate: true}
		for _, tier := range tiers {
			p.entries = append(p.entries, routerPlanEntry{tier: tier})
		}
		return p
	}
	gw.countCalibration(mk(smartReasonScore, "t0"), 0, true, false)
	gw.countCalibration(mk(smartReasonScore, "t0"), 0, true, true)
	n, bad := read()
	assert.Equal(t, int64(2), n)
	assert.Equal(t, int64(1), bad)

	gw.countCalibration(mk(smartReasonKeyword, "t0"), 0, true, true)
	gw.countCalibration(mk(smartReasonScore, "t0"), 0, false, false)
	gw.countCalibration(mk(smartReasonScore, "t0", "t1"), 1, true, false)
	n, bad = read()
	assert.Equal(t, int64(2), n, "keyword, upstream error and escalated outcomes are excluded")
	assert.Equal(t, int64(1), bad)

	off := mk(smartReasonScore, "t0")
	off.calibrate = false
	gw.countCalibration(off, 0, true, true)
	n, _ = read()
	assert.Equal(t, int64(2), n)
}

func TestCalibOffWritesNothing(t *testing.T) {
	gw, rt := calibGateway(t, nil)
	require.Nil(t, rt.calibState)
	plan, err := gw.planSmartRouter(rt, userReq("hi"), cacheEndpointChat, nil, &group{}, nil)
	require.NoError(t, err)
	assert.False(t, plan.calibrate)
	gw.countCalibration(plan, 0, true, true)
	gw.touchRouterLearning(rt)
	assert.False(t, gw.routerLearn.running, "a router without learning never starts the ticker")
}

func TestCalibValidation(t *testing.T) {
	t.Parallel()
	over := 301
	bigLower := 250
	gapLower := 300
	tests := []struct {
		name string
		cfg  *SmartRouterCalibrationConfig
		want string
	}{
		{"bad mode", &SmartRouterCalibrationConfig{Mode: "on"}, "calibration.mode"},
		{"maxLower range", &SmartRouterCalibrationConfig{MaxLower: &over}, "maxLower"},
		{"maxLower vs gap", &SmartRouterCalibrationConfig{MaxLower: &bigLower, MaxRaise: 60}, "gap"},
		{"maxLower equals gap", &SmartRouterCalibrationConfig{MaxLower: &gapLower}, "gap"},
		{"maxRaise range", &SmartRouterCalibrationConfig{MaxRaise: 101}, "maxRaise"},
		{"step range", &SmartRouterCalibrationConfig{Step: 101}, "step"},
		{"target range", &SmartRouterCalibrationConfig{TargetBadPct: 51}, "targetBadPct"},
		{"minSamples floor", &SmartRouterCalibrationConfig{MinSamples: 10}, "minSamples"},
	}
	for _, tt := range tests {
		_, _, err := compileSmartCalibration("r", tt.cfg, threeTierStructs())
		require.Error(t, err, tt.name)
		assert.Contains(t, err.Error(), tt.want, tt.name)
	}
	zero := 0
	c, st, err := compileSmartCalibration("r", &SmartRouterCalibrationConfig{Mode: smartCalibLive, MaxLower: &zero}, threeTierStructs())
	require.NoError(t, err)
	assert.Equal(t, 0, c.maxLower)
	assert.NotNil(t, st)
	c, st, err = compileSmartCalibration("r", nil, threeTierStructs())
	require.NoError(t, err)
	assert.Equal(t, smartCalibOff, c.mode)
	assert.Nil(t, st)
	c, st, err = compileSmartCalibration("r", &SmartRouterCalibrationConfig{}, threeTierStructs())
	require.NoError(t, err)
	assert.Equal(t, defaultCalibMaxLower, c.maxLower)
	assert.Nil(t, st, "mode off keeps no state")
}

func TestCalibEffectiveThresholdsStrictlyIncrease(t *testing.T) {
	t.Parallel()
	tiers := []smartTier{{name: "a"}, {name: "b", minScore: 150}, {name: "c", minScore: 350}}
	lower := 150
	_, _, err := compileSmartCalibration("r", &SmartRouterCalibrationConfig{Mode: smartCalibLive, MaxLower: &lower, MaxRaise: 40}, tiers)
	require.Error(t, err, "150 + 40 reaches the 150 gap to tier a")
	lower = 100
	c, st, err := compileSmartCalibration("r", &SmartRouterCalibrationConfig{Mode: smartCalibLive, MaxLower: &lower, MaxRaise: 40}, tiers)
	require.NoError(t, err)
	rt := &smartRouter{tiers: tiers, calib: c, calibState: st}
	for d1 := -c.maxRaise; d1 <= c.maxLower; d1 += 20 {
		for d2 := -c.maxRaise; d2 <= c.maxLower; d2 += 20 {
			st.set([]int{0, d1, d2})
			assert.Less(t, rt.tierMin(0), rt.tierMin(1))
			assert.Less(t, rt.tierMin(1), rt.tierMin(2))
		}
	}
}

func TestCalibAdminViewAndGauge(t *testing.T) {
	gw, _ := calibGateway(t, &SmartRouterCalibrationConfig{Mode: smartCalibShadow})
	seedCalib(t, gw, "t0", 1000, 500, 0)
	gw.refreshRouterLearning(gw.limiter.now())
	views := gw.buildAdminRouterViews()
	require.Len(t, views, 1)
	v := views[0].Calibration
	assert.Equal(t, smartCalibShadow, v.Mode)
	require.Len(t, v.Delta, 2)
	assert.Equal(t, adminRouterDeltaView{Tier: "t1", Delta: 25}, v.Delta[0])
	assert.Equal(t, 150, v.MaxLower)

	m := &metricWriter{}
	gw.writeRouterCalibrationMetrics(m)
	out := m.buf.String()
	assert.Contains(t, out, `llmgateway_router_calibration_delta{router="auto",tier="t1",mode="shadow"} 25`)

	offGw, _ := calibGateway(t, nil)
	assert.Equal(t, smartCalibOff, offGw.buildAdminRouterViews()[0].Calibration.Mode)
	m2 := &metricWriter{}
	offGw.writeRouterCalibrationMetrics(m2)
	assert.Empty(t, m2.buf.String())
}

func TestRouterLearningStopsOnClose(t *testing.T) {
	gw, rt := calibGateway(t, &SmartRouterCalibrationConfig{Mode: smartCalibShadow})
	gw.touchRouterLearning(rt)
	gw.touchRouterLearning(rt)
	assert.True(t, gw.routerLearn.running)
	done := gw.routerLearn.done
	assert.NoError(t, gw.Close())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh goroutine still running after Close")
	}
	gw.touchRouterLearning(rt)
	assert.False(t, gw.routerLearn.running, "a closed gateway never restarts the ticker")
}

func TestRouterLearningStopsWhenIdle(t *testing.T) {
	gw, rt := calibGateway(t, &SmartRouterCalibrationConfig{Mode: smartCalibShadow})
	gw.routerLearn.every = 5 * time.Millisecond
	gw.touchRouterLearning(rt)
	gw.routerLearn.mu.Lock()
	gw.routerLearn.lastUse = time.Now().Add(-2 * routerLearnIdleStop)
	gw.routerLearn.mu.Unlock()
	assert.Eventually(t, func() bool {
		gw.routerLearn.mu.Lock()
		defer gw.routerLearn.mu.Unlock()
		return !gw.routerLearn.running
	}, 2*time.Second, 5*time.Millisecond)
}

func TestRouterLearningRecoversFromPanic(t *testing.T) {
	gw, rt := calibGateway(t, &SmartRouterCalibrationConfig{Mode: smartCalibShadow})
	gw.routerLearn.every = 5 * time.Millisecond
	gw.registry.routers["broken"] = nil
	gw.touchRouterLearning(rt)
	done := gw.routerLearn.done
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the refresh goroutine never returned")
	}
	gw.routerLearn.mu.Lock()
	running := gw.routerLearn.running
	gw.routerLearn.mu.Unlock()
	assert.False(t, running, "a panicked refresh marks the learner stopped so the next request restarts it")
	delete(gw.registry.routers, "broken")
	gw.touchRouterLearning(rt)
	assert.True(t, gw.routerLearn.running)
	assert.NoError(t, gw.Close())
}

func threeTierStructs() []smartTier {
	return []smartTier{{name: "t0"}, {name: "t1", minScore: 300}, {name: "t2", minScore: 600}}
}

func sessionLongAsk() string {
	return "please explain the trade-offs of event sourcing against plain CRUD for a billing system"
}

func planShape(p *routerPlan) []string {
	out := make([]string, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, e.tier+"/"+routerCandidateKey(e.cand))
	}
	return out
}

func TestCalibOffPlansIdentical(t *testing.T) {
	plain, plainRt := calibGateway(t, nil)
	off, offRt := calibGateway(t, &SmartRouterCalibrationConfig{Mode: smartCalibOff})
	for _, ask := range []string{"hi", sessionLongAsk(), "prove the lemma and derive the invariant for this scheduler", "summarize this"} {
		a, err := plain.planSmartRouter(plainRt, userReq(ask), cacheEndpointChat, nil, &group{}, nil)
		require.NoError(t, err)
		b, err := off.planSmartRouter(offRt, userReq(ask), cacheEndpointChat, nil, &group{}, nil)
		require.NoError(t, err)
		assert.Equal(t, planShape(a), planShape(b), ask)
		assert.Equal(t, a.reason, b.reason, ask)
		assert.Equal(t, a.startTier, b.startTier, ask)
	}
}

func TestCalibCountsServedRequest(t *testing.T) {
	cfg := &SmartRouterCalibrationConfig{Mode: smartCalibShadow}
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	t.Cleanup(func() { closeBackends(a, b, c) })
	conf := routerTestConfig(a, b, c, threeTiers())
	conf.SmartRouters[routerName].Calibration = cfg
	h, gw := buildRouterGateway(t, conf)
	gw.limiter.spawn = syncSpawn
	rec := routerChat(t, h, routerName, "hi", nil)
	require.Equal(t, 200, rec.Code)
	bucket := bucketFor(gw.limiter.now(), windowHour)
	v, ok := gw.limiter.storeGetMulti([]string{routerCalibKeyFor(routerName, "t0", "n", bucket), routerCalibKeyFor(routerName, "t0", "bad", bucket)})
	require.True(t, ok)
	assert.Equal(t, []int64{1, 0}, v)
}
