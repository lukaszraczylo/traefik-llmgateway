package traefikllmgateway

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const shadowCodeAsk = "```go\nfunc f() {}\n```\ndebug this exception and refactor"

func zeroCodeShadow() *SmartRouterWeights {
	zero := 0
	return &SmartRouterWeights{Code: &zero}
}

func TestClassifySmart_ShadowRank(t *testing.T) {
	t.Parallel()
	live := newClassifyTestRouter(t, nil)
	if c := classifySmart(live, chatMsgs(msg("user", shadowCodeAsk)), cacheEndpointChat); c.shadowRank != -1 || c.rank != 1 {
		t.Errorf("no shadowWeights: rank=%d shadow=%d, want 1 and -1", c.rank, c.shadowRank)
	}

	rt := newClassifyTestRouter(t, func(c *SmartRouterConfig) { c.ShadowWeights = zeroCodeShadow() })
	c := classifySmart(rt, chatMsgs(msg("user", shadowCodeAsk)), cacheEndpointChat)
	if c.rank != 1 || c.shadowRank != 0 {
		t.Errorf("rank=%d shadow=%d, want live 1 and shadow 0", c.rank, c.shadowRank)
	}

	c = classifySmart(rt, chatMsgs(msg("user", strongAsk)), cacheEndpointChat)
	if c.rank != 2 || c.shadowRank != 2 {
		t.Errorf("reasoning floor must apply to the shadow: rank=%d shadow=%d, want 2 and 2", c.rank, c.shadowRank)
	}

	c = classifySmart(rt, map[string]any{"messages": []any{msg("assistant", "x")}}, cacheEndpointChat)
	if c.shadowRank != c.rank {
		t.Errorf("no user text: shadow=%d, want it equal to rank %d", c.shadowRank, c.rank)
	}
}

func TestClassifySmart_ShadowKeepsLiveResultIdentical(t *testing.T) {
	t.Parallel()
	plain := newClassifyTestRouter(t, nil)
	shadowed := newClassifyTestRouter(t, func(c *SmartRouterConfig) { c.ShadowWeights = zeroCodeShadow() })
	for _, text := range []string{shadowCodeAsk, strongAsk, "hello", ""} {
		req := chatMsgs(msg("user", text))
		r1, w1, t1, i1 := classifySmartRequest(plain, req, cacheEndpointChat)
		r2, w2, t2, i2 := classifySmartRequest(shadowed, req, cacheEndpointChat)
		if r1 != r2 || w1 != w2 || t1 != t2 || i1 != i2 {
			t.Errorf("text %q: live result changed with shadowWeights: (%d,%s,%v) vs (%d,%s,%v)", text, r1, w1, i1, r2, w2, i2)
		}
	}
}

func TestCompileSmartRouter_ShadowWeightsValidated(t *testing.T) {
	t.Parallel()
	tooBig := 5000
	_, err := compileSmartRouter("auto", &SmartRouterConfig{Tiers: smartTestTiers(), ShadowWeights: &SmartRouterWeights{Code: &tooBig}}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shadowWeights")
}

func TestPlanSmartRouter_ShadowNeverChangesRouting(t *testing.T) {
	withShadow, rtShadow := planTestGateway(t, func(c *Config) { c.SmartRouters[routerName].ShadowWeights = zeroCodeShadow() })
	without, rtPlain := planTestGateway(t, nil)
	for _, text := range []string{shadowCodeAsk, strongAsk, "hello"} {
		a, err := withShadow.planSmartRouter(rtShadow, userReq(text), cacheEndpointChat, &group{}, nil)
		require.NoError(t, err)
		b, err := without.planSmartRouter(rtPlain, userReq(text), cacheEndpointChat, &group{}, nil)
		require.NoError(t, err)
		assert.Equal(t, b.startTier, a.startTier, text)
		assert.Equal(t, b.reason, a.reason, text)
		require.Len(t, a.entries, len(b.entries), text)
		for i := range a.entries {
			assert.Equal(t, b.entries[i].cand.canonical, a.entries[i].cand.canonical, text)
			assert.Equal(t, b.entries[i].tier, a.entries[i].tier, text)
		}
	}
	p, err := withShadow.planSmartRouter(rtShadow, userReq(shadowCodeAsk), cacheEndpointChat, &group{}, nil)
	require.NoError(t, err)
	assert.Equal(t, "t1", p.startTier)
	assert.Equal(t, "t0", p.shadowTier)
}

func shadowTestGateway(t *testing.T, statuses [3]int, redisAddr string) (func(string) int, *Gateway) {
	t.Helper()
	cfg, closeAll := routerSharedConfig(statuses, redisAddr, true)
	t.Cleanup(closeAll)
	cfg.SmartRouters[routerName].ShadowWeights = zeroCodeShadow()
	return syncRouterGateway(t, cfg)
}

func TestRouterShadow_MatrixCountsAgreementAndDisagreement(t *testing.T) {
	chat, gw := shadowTestGateway(t, [3]int{200, 200, 200}, "")
	require.Equal(t, 200, chat("hello"))
	require.Equal(t, 200, chat("hello"))
	require.Equal(t, 200, chat(shadowCodeAsk))

	v := routerView(t, gw)
	assert.True(t, v.ShadowEnabled)
	assert.Equal(t, statsScopeInstance, v.Stats.Scope)
	assert.Equal(t, []adminRouterShadowView{
		{Live: "t0", Shadow: "t0", Count: 2},
		{Live: "t1", Shadow: "t0", Count: 1},
	}, v.Stats.Shadow)
}

func TestRouterShadow_NoShadowConfiguredRecordsNothing(t *testing.T) {
	cfg, closeAll := routerSharedConfig([3]int{200, 200, 200}, "", true)
	defer closeAll()
	chat, gw := syncRouterGateway(t, cfg)
	require.Equal(t, 200, chat(shadowCodeAsk))

	v := routerView(t, gw)
	assert.False(t, v.ShadowEnabled)
	assert.Empty(t, v.Stats.Shadow)
	assert.Empty(t, gw.routerStats.shadow)
}

func TestRouterOutcomes_OkEscalatedError(t *testing.T) {
	chat, gw := shadowTestGateway(t, [3]int{200, 200, 200}, "")
	require.Equal(t, 200, chat("hello"))
	assert.Equal(t, []adminRouterOutcomeView{{Tier: "t0", Reason: "score", Outcome: "ok", Count: 1}}, routerView(t, gw).Stats.Outcomes)

	chat, gw = shadowTestGateway(t, [3]int{503, 200, 200}, "")
	require.Equal(t, 200, chat("hello"))
	assert.Equal(t, []adminRouterOutcomeView{{Tier: "t0", Reason: "score", Outcome: "escalated", Count: 1}}, routerView(t, gw).Stats.Outcomes)

	chat, gw = shadowTestGateway(t, [3]int{503, 503, 503}, "")
	require.NotEqual(t, 200, chat("hello"))
	assert.Equal(t, []adminRouterOutcomeView{{Tier: "t0", Reason: "score", Outcome: "error", Count: 1}}, routerView(t, gw).Stats.Outcomes)
}

func TestRouterOutcomes_NonRetryableClientErrorCountsAsError(t *testing.T) {
	chat, gw := shadowTestGateway(t, [3]int{400, 200, 200}, "")
	require.NotEqual(t, 200, chat("hello"))
	assert.Equal(t, []adminRouterOutcomeView{{Tier: "t0", Reason: "score", Outcome: "error", Count: 1}}, routerView(t, gw).Stats.Outcomes)
}

func TestRouterShadowAndOutcomes_SharedAcrossInstances(t *testing.T) {
	ln := newBehavioralRedisServer(t)
	chatA, _ := shadowTestGateway(t, [3]int{200, 503, 200}, ln.Addr().String())
	_, gwB := shadowTestGateway(t, [3]int{200, 200, 200}, ln.Addr().String())
	require.Equal(t, 200, chatA(shadowCodeAsk))

	v := routerView(t, gwB)
	assert.Equal(t, statsScopeShared, v.Stats.Scope)
	assert.Equal(t, []adminRouterShadowView{{Live: "t1", Shadow: "t0", Count: 1}}, v.Stats.Shadow)
	assert.Equal(t, []adminRouterOutcomeView{{Tier: "t1", Reason: "score", Outcome: "escalated", Count: 1}}, v.Stats.Outcomes)
}

func BenchmarkClassifySmart_Shadow(b *testing.B) {
	body := strings.Repeat("Please review this service and explain how the request path handles retries. ", 60)
	req := map[string]any{
		"messages": []any{
			msg("system", "You are a helpful assistant."),
			msg("user", "Earlier question about deployment."),
			msg("assistant", "Sure, here is how it works."),
			msg("user", []any{part("text", "text", body+"```go\nfunc main() {}\n```")}),
		},
		"tools": []any{map[string]any{"type": "function"}},
	}
	for _, shadow := range []bool{false, true} {
		name := "shadow_off"
		if shadow {
			name = "shadow_on"
		}
		b.Run(name, func(b *testing.B) {
			rt := newClassifyTestRouter(b, func(c *SmartRouterConfig) {
				if shadow {
					c.ShadowWeights = zeroCodeShadow()
				}
			})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				classifySmart(rt, req, cacheEndpointChat)
			}
		})
	}
}
