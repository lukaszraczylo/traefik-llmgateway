package traefikllmgateway

import (
	"context"
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
	plan, err := gw.planSmartRouter(context.Background(), rt, req, cacheEndpointChat, nil, &group{}, nil)
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
	plan, err := gw.planSmartRouter(context.Background(), rt, req, cacheEndpointChat, nil, &group{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "gamma/c1", plan.entries[0].cand.canonical, "earlier turns must count toward the context estimate")
}

func TestPlanSmartRouter_ContextSkipsEliminatingEverythingFallBackToLargest(t *testing.T) {
	gw, rt := planTestGateway(t, func(c *Config) {
		c.ModelMeta = map[string]*ModelMetaConfig{"a1": {ContextTokens: 50}, "c1": {ContextTokens: 80}}
		twoTiers(c, "a1")
	})
	plan, err := gw.planSmartRouter(context.Background(), rt, userReq(strings.Repeat("word ", 400)), cacheEndpointChat, nil, &group{}, nil)
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
	plan, err := gw.planSmartRouter(context.Background(), rt, userReq("hi"), cacheEndpointChat, nil, &group{}, nil)
	require.NoError(t, err)
	var tiers []string
	for _, e := range plan.entries {
		tiers = append(tiers, e.tier)
	}
	assert.Equal(t, []string{"t0", "t0", "t1"}, tiers, "the last slot must stay reachable for the higher tier")

	rt.maxAttempts = 1
	plan, err = gw.planSmartRouter(context.Background(), rt, userReq("hi"), cacheEndpointChat, nil, &group{}, nil)
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
	_, err := gw.planSmartRouter(context.Background(), rt, userReq("hi"), cacheEndpointChat, nil, grp, nil)
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

func firstPlanModel(t *testing.T, gw *Gateway, rt *smartRouter, req map[string]any, endpoint string) string {
	t.Helper()
	plan, err := gw.planSmartRouter(context.Background(), rt, req, endpoint, nil, &group{}, nil)
	require.NoError(t, err)
	return plan.entries[0].cand.canonical
}

func TestPlanSmartRouter_ContextCheckIncludesRequestedOutput(t *testing.T) {
	gw, rt := planTestGateway(t, func(c *Config) {
		c.ModelMeta = map[string]*ModelMetaConfig{"a1": {ContextTokens: 32000}, "c1": {ContextTokens: 200000}}
		twoTiers(c, "a1")
	})
	prompt := strings.Repeat("a", 4*30000)
	req := userReq(prompt)
	req["max_tokens"] = float64(8000)
	assert.Equal(t, "gamma/c1", firstPlanModel(t, gw, rt, req, cacheEndpointChat), "30K prompt plus 8K output must skip a 32K model")

	short := userReq(strings.Repeat("a", 4*20000))
	short["max_tokens"] = float64(8000)
	assert.Equal(t, "alpha/a1", firstPlanModel(t, gw, rt, short, cacheEndpointChat), "28K total fits 90% of 32K")
}

func TestPlanSmartRouter_ContextCheckMargin(t *testing.T) {
	gw, rt := planTestGateway(t, func(c *Config) {
		c.ModelMeta = map[string]*ModelMetaConfig{"a1": {ContextTokens: 1000}, "c1": {ContextTokens: 200000}}
		twoTiers(c, "a1")
	})
	tests := []struct {
		extra    map[string]any
		name     string
		endpoint string
		want     string
		tokens   int
	}{
		{name: "exactly 90% fits", tokens: 900, endpoint: cacheEndpointChat, want: "alpha/a1"},
		{name: "just over 90% does not fit", tokens: 901, endpoint: cacheEndpointChat, want: "gamma/c1"},
		{name: "max_completion_tokens counts", tokens: 800, endpoint: cacheEndpointChat, extra: map[string]any{"max_completion_tokens": float64(101)}, want: "gamma/c1"},
		{name: "max_completion_tokens wins over max_tokens", tokens: 800, endpoint: cacheEndpointChat, extra: map[string]any{"max_completion_tokens": float64(100), "max_tokens": float64(5000)}, want: "alpha/a1"},
		{name: "max_tokens counts on chat", tokens: 800, endpoint: cacheEndpointChat, extra: map[string]any{"max_tokens": float64(101)}, want: "gamma/c1"},
		{name: "max_tokens counts on messages", tokens: 800, endpoint: cacheEndpointMessages, extra: map[string]any{"max_tokens": float64(101)}, want: "gamma/c1"},
		{name: "messages ignores max_completion_tokens", tokens: 800, endpoint: cacheEndpointMessages, extra: map[string]any{"max_completion_tokens": float64(5000)}, want: "alpha/a1"},
		{name: "negative output counts as zero", tokens: 900, endpoint: cacheEndpointChat, extra: map[string]any{"max_tokens": float64(-50)}, want: "alpha/a1"},
		{name: "huge output does not overflow", tokens: 10, endpoint: cacheEndpointChat, extra: map[string]any{"max_tokens": float64(9e18)}, want: "gamma/c1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := userReq(strings.Repeat("a", 4*tt.tokens))
			for k, v := range tt.extra {
				req[k] = v
			}
			assert.Equal(t, tt.want, firstPlanModel(t, gw, rt, req, tt.endpoint))
		})
	}
}

func TestPlanSingleTierHonoursMaxAttempts(t *testing.T) {
	gw, rt := planTestGateway(t, func(c *Config) {
		rc := c.SmartRouters[routerName]
		rc.KeywordRules = nil
		rc.MaxAttempts = 2
		rc.Tiers = []SmartRouterTier{{Name: "t0", MinScore: 0, Models: []string{"a1", "a2", "b1"}}}
	})
	plan := planOf(t, gw, rt, userReq("hi"))
	assert.Len(t, plan.entries, 2, "the attempt cap holds with no higher tier")
}

func TestPlanReservedSlotNotWastedOnPlacedKey(t *testing.T) {
	gw, rt := planTestGateway(t, func(c *Config) {
		rc := c.SmartRouters[routerName]
		rc.KeywordRules = nil
		rc.MaxAttempts = 2
		rc.Tiers = []SmartRouterTier{
			{Name: "t0", MinScore: 0, Models: []string{"a1", "a2"}},
			{Name: "t1", MinScore: 300, Models: []string{"alpha/a1"}},
		}
	})
	plan := planOf(t, gw, rt, userReq("hi"))
	require.Len(t, plan.entries, 2, "the second slot is not reserved for a key the first tier places")
	assert.Equal(t, "alpha/a1", plan.entries[0].cand.canonical)
	assert.Equal(t, "alpha/a2", plan.entries[1].cand.canonical)
}
