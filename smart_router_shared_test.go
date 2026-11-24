package traefikllmgateway

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func routerSharedConfig(statuses [3]int, redisAddr string, metrics bool) (*Config, func()) {
	a := newRouterBackend(statuses[0], routerStatusBody(statuses[0]))
	b := newRouterBackend(statuses[1], routerStatusBody(statuses[1]))
	c := newRouterBackend(statuses[2], routerStatusBody(statuses[2]))
	cfg := routerTestConfig(a, b, c, threeTiers())
	cfg.Admin = &AdminConfig{Enabled: true}
	if redisAddr != "" {
		cfg.Redis = &RedisConfig{Address: redisAddr}
	}
	if metrics {
		cfg.Metrics = &MetricsConfig{Enabled: true}
	}
	return cfg, func() { closeBackends(a, b, c) }
}

func syncRouterGateway(t *testing.T, cfg *Config) (func(string) int, *Gateway) {
	t.Helper()
	h, gw := buildRouterGateway(t, cfg)
	gw.limiter.spawn = func(f func()) { f() }
	return func(content string) int { return routerChat(t, h, routerName, content, nil).Code }, gw
}

func routerView(t *testing.T, gw *Gateway) adminRouterView {
	t.Helper()
	for _, v := range gw.buildAdminRouterViews() {
		if v.Name == routerName {
			return v
		}
	}
	t.Fatalf("router %q not in admin views", routerName)
	return adminRouterView{}
}

func TestRouterStatsShared_CountsVisibleFromOtherInstance(t *testing.T) {
	ln := newBehavioralRedisServer(t)
	cfgA, closeA := routerSharedConfig([3]int{503, 200, 200}, ln.Addr().String(), false)
	defer closeA()
	cfgB, closeB := routerSharedConfig([3]int{200, 200, 200}, ln.Addr().String(), false)
	defer closeB()
	chatA, gwA := syncRouterGateway(t, cfgA)
	_, gwB := syncRouterGateway(t, cfgB)

	require.Equal(t, 200, chatA("hi"))
	require.Equal(t, 200, chatA("hi"))

	v := routerView(t, gwB)
	assert.Equal(t, statsScopeShared, v.Stats.Scope)
	assert.EqualValues(t, 2, v.Stats.Requests)
	assert.Equal(t, []adminRouterCountView{{Name: "t0", Count: 2}, {Name: "t1"}, {Name: "t2"}}, v.Stats.Tiers)
	assert.Equal(t, []adminRouterCountView{{Name: "score", Count: 2}}, v.Stats.Reasons)
	assert.Equal(t, []adminRouterEscalationView{{From: "t0", To: "t1", Count: 2}}, v.Stats.Escalations)
	assert.Zero(t, gwB.routerStats.requests[routerRequestKey{router: routerName, tier: "t0", reason: "score"}], "instance B counted nothing itself")
	assert.Equal(t, statsScopeShared, routerView(t, gwA).Stats.Scope)
}

func TestRouterStatsShared_SumsAcrossInstances(t *testing.T) {
	ln := newBehavioralRedisServer(t)
	cfgA, closeA := routerSharedConfig([3]int{200, 200, 200}, ln.Addr().String(), false)
	defer closeA()
	cfgB, closeB := routerSharedConfig([3]int{200, 200, 200}, ln.Addr().String(), false)
	defer closeB()
	chatA, _ := syncRouterGateway(t, cfgA)
	chatB, gwB := syncRouterGateway(t, cfgB)

	require.Equal(t, 200, chatA("hi"))
	require.Equal(t, 200, chatB("hi"))
	require.Equal(t, 200, chatB(routerKeyword))

	v := routerView(t, gwB)
	assert.EqualValues(t, 3, v.Stats.Requests)
	assert.Equal(t, []adminRouterCountView{{Name: "keyword", Count: 1}, {Name: "score", Count: 2}}, v.Stats.Reasons)
}

func TestRouterStatsShared_NoRedisCountsInMemory(t *testing.T) {
	cfg, closeAll := routerSharedConfig([3]int{503, 200, 200}, "", true)
	defer closeAll()
	chat, gw := syncRouterGateway(t, cfg)

	require.Equal(t, 200, chat("hi"))

	v := routerView(t, gw)
	assert.Equal(t, statsScopeInstance, v.Stats.Scope)
	assert.EqualValues(t, 1, v.Stats.Requests)
	assert.Equal(t, []adminRouterEscalationView{{From: "t0", To: "t1", Count: 1}}, v.Stats.Escalations)
}

func TestRouterStatsShared_RedisDownRequestSucceedsAndViewFallsBack(t *testing.T) {
	ln := newFakeListener(t)
	dead := ln.Addr().String()
	require.NoError(t, ln.Close())
	cfg, closeAll := routerSharedConfig([3]int{503, 200, 200}, dead, true)
	defer closeAll()
	chat, gw := syncRouterGateway(t, cfg)

	require.Equal(t, 200, chat("hi"))

	v := routerView(t, gw)
	assert.Equal(t, statsScopeInstance, v.Stats.Scope)
	assert.EqualValues(t, 1, v.Stats.Requests, "in-memory counts survive a Redis outage")
}

func TestRouterStatsShared_RedisDownRecordingNeverPanics(t *testing.T) {
	ln := newFakeListener(t)
	dead := ln.Addr().String()
	require.NoError(t, ln.Close())
	cfg, closeAll := routerSharedConfig([3]int{200, 200, 200}, dead, false)
	defer closeAll()
	_, gw := syncRouterGateway(t, cfg)

	plan := &routerPlan{router: routerName, reason: "score", startTier: "t0", entries: []routerPlanEntry{{tier: "t0"}, {tier: "t1"}}}
	assert.NotPanics(t, func() {
		gw.recordRouterAttempt(plan, 0, cacheEndpointChat, nil)
		gw.recordRouterAttempt(plan, 1, cacheEndpointChat, nil)
	})
}
