package traefikllmgateway

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func planTestGateway(t *testing.T, mut func(*Config)) (*Gateway, *smartRouter) {
	t.Helper()
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	t.Cleanup(func() { closeBackends(a, b, c) })
	cfg := routerTestConfig(a, b, c, threeTiers())
	if mut != nil {
		mut(cfg)
	}
	_, gw := buildRouterGateway(t, cfg)
	return gw, gw.registry.routers[routerName]
}

func userReq(content any) map[string]any {
	return map[string]any{"messages": []any{map[string]any{"role": "user", "content": content}}}
}

func TestPlanSmartRouter_ContextEstimateIgnoresImageBytes(t *testing.T) {
	gw, rt := planTestGateway(t, func(c *Config) {
		c.ModelMeta = map[string]*ModelMetaConfig{"a1": {ContextTokens: 128000}}
		twoTiers(c, "a1")
	})
	huge := strings.Repeat("A", 600_000)
	req := userReq([]any{
		map[string]any{"type": "text", "text": "what is this"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + huge}},
	})
	plan, err := gw.planSmartRouter(rt, req, cacheEndpointChat, &group{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "alpha/a1", plan.entries[0].cand.canonical, "image bytes must not count as prompt tokens")
}

func TestPlanSmartRouter_ContextEstimateCountsWholeConversation(t *testing.T) {
	gw, rt := planTestGateway(t, func(c *Config) {
		c.ModelMeta = map[string]*ModelMetaConfig{"a1": {ContextTokens: 500}}
		twoTiers(c, "a1")
	})
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": strings.Repeat("word ", 800)},
		map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": "hi"},
	}}
	plan, err := gw.planSmartRouter(rt, req, cacheEndpointChat, &group{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "gamma/c1", plan.entries[0].cand.canonical, "earlier turns must count toward the context estimate")
}

func TestPlanSmartRouter_ContextSkipsEliminatingEverythingFallBackToLargest(t *testing.T) {
	gw, rt := planTestGateway(t, func(c *Config) {
		c.ModelMeta = map[string]*ModelMetaConfig{"a1": {ContextTokens: 50}, "c1": {ContextTokens: 80}}
		twoTiers(c, "a1")
	})
	plan, err := gw.planSmartRouter(rt, userReq(strings.Repeat("word ", 400)), cacheEndpointChat, &group{}, nil)
	require.NoError(t, err, "context skips alone must not 404")
	require.Len(t, plan.entries, 1)
	assert.Equal(t, "gamma/c1", plan.entries[0].cand.canonical)
	assert.Equal(t, "t1", plan.entries[0].tier)
}

func TestPlanSmartRouter_ReservesSlotForHigherTier(t *testing.T) {
	gw, rt := planTestGateway(t, func(c *Config) {
		cfgTiers := []SmartRouterTier{
			{Name: "t0", MinScore: 0, Models: []string{"a1", "a2", "b1"}},
			{Name: "t1", MinScore: 300, Models: []string{"c1"}},
		}
		c.SmartRouters[routerName].Tiers = cfgTiers
		c.SmartRouters[routerName].KeywordRules = nil
		c.SmartRouters[routerName].MaxAttempts = 3
	})
	plan, err := gw.planSmartRouter(rt, userReq("hi"), cacheEndpointChat, &group{}, nil)
	require.NoError(t, err)
	var tiers []string
	for _, e := range plan.entries {
		tiers = append(tiers, e.tier)
	}
	assert.Equal(t, []string{"t0", "t0", "t1"}, tiers, "the last slot must stay reachable for the higher tier")

	rt.maxAttempts = 1
	plan, err = gw.planSmartRouter(rt, userReq("hi"), cacheEndpointChat, &group{}, nil)
	require.NoError(t, err)
	require.Len(t, plan.entries, 1)
	assert.Equal(t, "t0", plan.entries[0].tier, "a single attempt still starts at the classified tier")
}

func TestPlanSmartRouter_NoUsableTargetMessageHidesDeniedCount(t *testing.T) {
	gw, rt := planTestGateway(t, func(c *Config) {
		c.Groups["default"].Models = []string{routerName, "b1"}
		c.SmartRouters[routerName].Tiers = []SmartRouterTier{
			{Name: "t0", MinScore: 0, Models: []string{"a1"}},
			{Name: "t1", MinScore: 300, Models: []string{"ghost"}},
		}
		c.SmartRouters[routerName].KeywordRules = nil
	})
	grp := &group{models: []string{routerName, "b1"}}
	_, err := gw.planSmartRouter(rt, userReq("hi"), cacheEndpointChat, grp, nil)
	rerr, ok := err.(*routerTargetError)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, 404, rerr.status)
	assert.NotContains(t, rerr.message, "denied")
	assert.NotContains(t, rerr.message, "skipped")
	gw.registry.warnedMu.Lock()
	logged := gw.registry.warned["router-empty:"+routerName]
	gw.registry.warnedMu.Unlock()
	assert.True(t, logged, "skip detail must be logged server-side")
}

func TestModelRegistry_NoteRouterShadow_CachesCleanResultPerGeneration(t *testing.T) {
	t.Parallel()
	var logs []string
	cfg := &Config{
		Providers:    map[string]*ProviderConfig{"acme": {Discovery: true}},
		SmartRouters: map[string]*SmartRouterConfig{"auto": {Tiers: []SmartRouterTier{{Name: "a", Models: []string{"acme/x"}}}}},
	}
	reg, err := newModelRegistry(map[string]providerAdapter{"acme": newFakeAdapter("acme")}, cfg, func(f string, a ...any) { logs = append(logs, f) })
	require.NoError(t, err)

	reg.noteRouterShadow("auto", allowAllGroup())
	reg.warnedMu.Lock()
	cached := len(reg.routerShadowClean)
	reg.warnedMu.Unlock()
	assert.Equal(t, 1, cached, "a clean result must be remembered")
	assert.Empty(t, logs)

	reg.finishRefresh(reg.states["acme"], reg.now(), []string{"auto"}, nil)
	reg.noteRouterShadow("auto", allowAllGroup())
	require.Len(t, logs, 1, "a new generation must rescan and warn")
	reg.noteRouterShadow("auto", allowAllGroup())
	assert.Len(t, logs, 1, "warn once")
}

func TestModelRegistry_ListFor_RouterHidesBareDiscoveredIDForEveryCaller(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Providers:    map[string]*ProviderConfig{"acme": {Discovery: true}},
		SmartRouters: map[string]*SmartRouterConfig{"auto": {Tiers: []SmartRouterTier{{Name: "a", Models: []string{"acme/x"}}}}},
	}
	reg, err := newModelRegistry(map[string]providerAdapter{"acme": newFakeAdapter("acme")}, cfg, func(string, ...any) {})
	require.NoError(t, err)
	reg.finishRefresh(reg.states["acme"], reg.now(), []string{"auto", "x"}, nil)

	// A caller who cannot see the router still has the request routed to it.
	grp := &group{models: []string{"auto", "acme/auto"}}
	ids := map[string]bool{}
	for _, e := range reg.listFor(grp) {
		id, _ := e["id"].(string)
		ids[id] = true
	}
	assert.False(t, ids["auto"], "bare id is served by the router for every caller, so it must not be listed")
	assert.True(t, ids["acme/auto"], "provider-prefixed form stays addressable")
}
