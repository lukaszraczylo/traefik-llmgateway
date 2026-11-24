package traefikllmgateway

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func v2Router(t testing.TB, mins []int, mut func(*SmartRouterConfig)) *smartRouter {
	t.Helper()
	tiers := make([]SmartRouterTier, len(mins))
	for i, m := range mins {
		tiers[i] = SmartRouterTier{Name: "t" + string(rune('0'+i)), MinScore: m, Models: []string{"a/m" + string(rune('0'+i))}}
	}
	cfg := &SmartRouterConfig{Classifier: smartClassifierV2, Tiers: tiers}
	if mut != nil {
		mut(cfg)
	}
	rt, err := compileSmartRouter("smart/auto", cfg, nil)
	require.NoError(t, err)
	return rt
}

func verdictFor(rt *smartRouter, text string) smartVerdict {
	f := extractSmartV2(chatMsgs(msg("user", text)), cacheEndpointChat)
	return scoreSmartV2(rt, &f, rt.weights, smartSessionHint{})
}

var intentSamples = map[int]string{
	smartIntentChitchat: "hello there, thanks",
	smartIntentRewrite:  "Rewrite this sentence so it sounds friendlier please",
	smartIntentEdit:     "Refactor the config loader to drop its global state",
	smartIntentDebug:    "My worker crashes whenever the queue is empty",
	smartIntentDesign:   "What architecture would you suggest for the audit trail",
	smartIntentMath:     "Prove that the sum of two even numbers is even",
}

func TestScoreIntentBases(t *testing.T) {
	t.Parallel()
	want := map[string][smartIntentCount]int{
		"deployed": {0, 0, 60, 150, 250, 310, 350},
		"wide":     {0, 0, 120, 300, 450, 540, 600},
	}
	for name, mins := range map[string][]int{"deployed": {0, 150, 350}, "wide": {0, 300, 600}} {
		rt := v2Router(t, mins, nil)
		assert.Equal(t, want[name], rt.intentBase, name)
		for intent, text := range intentSamples {
			f := extractSmartV2(chatMsgs(msg("user", text)), cacheEndpointChat)
			require.Equal(t, intent, f.intent, "%s: %q", name, text)
			v := scoreSmartV2(rt, &f, rt.weights, smartSessionHint{})
			assert.GreaterOrEqual(t, v.score, rt.intentBase[intent], "%s %s", name, smartIntentNames[intent])
		}
	}
}

func TestIntentBasesThresholdRelative(t *testing.T) {
	t.Parallel()
	for _, mins := range [][]int{{0, 150, 350}, {0, 300, 600}, {0, 100, 200, 800}, {0, 500}, {0, 40, 60, 900}} {
		rt := v2Router(t, mins, nil)
		top := mins[len(mins)-1]
		b := rt.intentBase
		assert.Equal(t, 0, b[smartIntentChitchat], "%v", mins)
		assert.Less(t, b[smartIntentRewrite], mins[1], "rewrite stays in tier 0 %v", mins)
		assert.Equal(t, mins[1], b[smartIntentEdit], "edit sits on the first boundary %v", mins)
		assert.GreaterOrEqual(t, b[smartIntentDebug], mins[1], "%v", mins)
		assert.LessOrEqual(t, b[smartIntentDebug], b[smartIntentDesign], "%v", mins)
		assert.LessOrEqual(t, b[smartIntentDesign], top, "%v", mins)
		assert.Equal(t, top, b[smartIntentMath], "math sits on the top boundary %v", mins)
		assert.Equal(t, 0, smartTierForScore(rt, b[smartIntentRewrite]), "%v", mins)
		assert.Equal(t, 1, smartTierForScore(rt, b[smartIntentEdit]), "%v", mins)
		assert.Equal(t, len(mins)-1, smartTierForScore(rt, b[smartIntentMath]), "%v", mins)
	}
	single := v2Router(t, []int{0}, nil)
	for i, base := range single.intentBase {
		assert.Zero(t, base, smartIntentNames[i])
	}
}

func TestIntentTiersOverride(t *testing.T) {
	t.Parallel()
	rt := v2Router(t, []int{0, 150, 350}, func(c *SmartRouterConfig) {
		c.IntentTiers = map[string]string{"debug": "t2", "rewrite": "t0", "foreign": "t2"}
	})
	assert.Equal(t, 350, rt.intentBase[smartIntentDebug])
	assert.Equal(t, 0, rt.intentBase[smartIntentRewrite])
	assert.Equal(t, 350, rt.foreignBase)
	assert.Equal(t, 150, rt.intentBase[smartIntentEdit], "unpinned intents keep the ladder position")

	for name, pins := range map[string]map[string]string{
		"unknown tier":   {"debug": "nope"},
		"unknown intent": {"poetry": "t1"},
		"unknown key":    {"unknown": "t1"},
	} {
		_, err := compileSmartRouter("smart/auto", &SmartRouterConfig{Classifier: smartClassifierV2, IntentTiers: pins,
			Tiers: []SmartRouterTier{{Name: "t0", Models: []string{"a/x"}}, {Name: "t1", MinScore: 100, Models: []string{"a/y"}}}}, nil)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "intentTiers", name)
	}
}

func TestRiskMarginRoutesUp(t *testing.T) {
	t.Parallel()
	const ask = "Rewrite this sentence so it sounds friendlier please, keep it short"
	prev := -1
	var ranks []int
	for _, z := range []int{0, 64, 128, 256, 400} {
		zz := z
		rt := v2Router(t, []int{0, 150, 350}, func(c *SmartRouterConfig) { c.RiskZ = &zz })
		v := verdictFor(rt, ask)
		assert.GreaterOrEqual(t, v.rank, prev, "rank must not fall as riskZ grows (z=%d)", z)
		prev = v.rank
		ranks = append(ranks, v.rank)
	}
	assert.Equal(t, 0, ranks[0], "no margin keeps the base tier")
	assert.Greater(t, ranks[len(ranks)-1], ranks[0], "a large margin raises the tier")

	rt := v2Router(t, []int{0, 150, 350}, nil)
	assert.Equal(t, defaultSmartRiskZ, rt.riskZ)
	low := verdictFor(rt, "Rewrite this sentence so it sounds friendlier please")
	high := verdictFor(rt, "请把这句话改写得更友好一些，谢谢你，保持简短")
	assert.Greater(t, high.sigma, low.sigma, "a non-Latin script carries more doubt")
}

func TestScoreSigmaTerms(t *testing.T) {
	t.Parallel()
	rt := v2Router(t, []int{0, 150, 350}, nil)
	score := func(f smartFeatures, sess smartSessionHint) smartVerdict {
		return scoreSmartV2(rt, &f, rt.weights, sess)
	}

	base := score(smartFeatures{found: true, prose: "hi", lower: "hi"}, smartSessionHint{})
	assert.Equal(t, smartSigmaBase, base.sigma)
	long := "x" + string(make([]byte, 80))
	assert.Equal(t, smartSigmaBase+smartSigmaUnknown, score(smartFeatures{found: true, prose: long, lower: long}, smartSessionHint{}).sigma)
	assert.Equal(t, smartSigmaBase+smartSigmaNonLatin, score(smartFeatures{found: true, prose: "hi", lower: "hi", nonLatin: true}, smartSessionHint{}).sigma)
	assert.Equal(t, smartSigmaBase+smartSigmaTruncated, score(smartFeatures{truncated: true}, smartSessionHint{}).sigma)
	assert.Equal(t, smartSigmaBase+smartSigmaShort, score(smartFeatures{found: true, prose: "fix it", lower: "fix it", history: true}, smartSessionHint{}).sigma)
	assert.Equal(t, smartSigmaBase, score(smartFeatures{found: true, prose: "fix it", lower: "fix it", history: true}, smartSessionHint{found: true}).sigma)
	assert.Equal(t, smartSigmaBase+smartSigmaSpread, score(smartFeatures{found: true, prose: "hi", lower: "hi", intentSpread: true}, smartSessionHint{}).sigma)

	carried := score(smartFeatures{found: true, prose: "fix it", lower: "fix it", history: true}, smartSessionHint{lastScore: 500, found: true})
	assert.Equal(t, 500*smartCarryPct/100, carried.score, "a short follow-up keeps most of the last score")
	ignored := score(smartFeatures{found: true, prose: "fix it", lower: "fix it"}, smartSessionHint{lastScore: 500, found: true})
	assert.Less(t, ignored.score, 400, "no history, no carry")
}

func TestScoreBonuses(t *testing.T) {
	t.Parallel()
	assert.Equal(t, smartBonusPrompt, smartScoreBonuses(&smartFeatures{promptBytes: 4*smartBonusPromptTokens + 8}))
	assert.Equal(t, smartBonusHistory, smartScoreBonuses(&smartFeatures{msgCount: smartLongHistory + 1}))
	assert.Equal(t, smartBonusOutput, smartScoreBonuses(&smartFeatures{outTokens: smartBonusOutputTokens}))
	assert.Equal(t, smartBonusSchema, smartScoreBonuses(&smartFeatures{jsonMode: smartJSONSchema}))
	assert.Equal(t, smartBonusJSONObject, smartScoreBonuses(&smartFeatures{jsonMode: smartJSONObject}))
	assert.Equal(t, 5, smartScoreBonuses(&smartFeatures{codeBytes: 1000}))
	assert.Equal(t, smartBonusCodeCap, smartScoreBonuses(&smartFeatures{codeBytes: 10_000_000}))
	assert.Equal(t, smartBonusLog, smartScoreBonuses(&smartFeatures{logBytes: 1}))
	assert.Zero(t, smartScoreBonuses(&smartFeatures{}))

	rt := v2Router(t, []int{0, 150, 350}, nil)
	f := smartFeatures{found: true, prose: "write", lower: "write", outTokens: smartBonusOutputTokens}
	assert.GreaterOrEqual(t, scoreSmartV2(rt, &f, rt.weights, smartSessionHint{}).score, rt.intentBase[smartIntentEdit], "long output floors at the first boundary")
}

func TestScoreV2NeverBelowV1(t *testing.T) {
	t.Parallel()
	texts := []string{"hello", "Summarize the council vote in two lines", "Why does this crash with a nil pointer? Please explain the trade-off",
		"Prove that there are infinitely many primes", "请证明根号2是无理数，并说明每一步的理由", strings.Repeat("Please review this service and explain the request path. ", 40)}
	for _, text := range intentSamples {
		texts = append(texts, text)
	}
	for _, mins := range [][]int{{0, 150, 350}, {0, 300, 600}} {
		rt := v2Router(t, mins, nil)
		for _, text := range texts {
			f := extractSmartV2(chatMsgs(msg("user", text)), cacheEndpointChat)
			v := scoreSmartV2(rt, &f, rt.weights, smartSessionHint{})
			sig := smartSignalsFor(f.lower, len(f.prose), countSmartMarkers(f.lower, smartReasoningMarkers), f.hasTools || f.toolHist)
			assert.GreaterOrEqual(t, v.score, sig.score(rt.weights), "%v %q", mins, text)
			assert.GreaterOrEqual(t, v.score, rt.intentBase[f.intent], "%v %q", mins, text)
		}
	}
}

func TestClassifyV2FloorsAndKeywords(t *testing.T) {
	t.Parallel()
	rt := v2Router(t, []int{0, 150, 350}, func(c *SmartRouterConfig) {
		c.ToolsMinTier = "t1"
		c.KeywordRules = []SmartRouterKeywordRule{{Tier: "t2", Keywords: []string{"security review"}}}
	})
	c := classifySmart(rt, chatMsgs(msg("user", "hello")), cacheEndpointChat)
	assert.Equal(t, 0, c.rank)
	assert.Equal(t, smartReasonScore, c.reason)

	c = classifySmart(rt, chatMsgs(msg("user", "hello there, please start the security review")), cacheEndpointChat)
	assert.Equal(t, 2, c.rank)
	assert.Equal(t, smartReasonKeyword, c.reason)

	withTools := chatMsgs(msg("user", "hello"))
	withTools["tools"] = []any{map[string]any{"type": "function"}}
	c = classifySmart(rt, withTools, cacheEndpointChat)
	assert.Equal(t, 1, c.rank)
	assert.Equal(t, smartReasonFloor, c.reason)

	effort := chatMsgs(msg("user", "hello"))
	effort["reasoning_effort"] = "high"
	assert.Equal(t, 2, classifySmart(rt, effort, cacheEndpointChat).rank)

	// Reasoning words in a paste must not trigger the two-marker floor.
	paste := "how many lines is this?\n```\nanalyze evaluate justify critique derive\n```"
	assert.Equal(t, 0, classifySmart(rt, chatMsgs(msg("user", paste)), cacheEndpointChat).rank)
	assert.Equal(t, 2, classifySmart(rt, chatMsgs(msg("user", "analyze and evaluate the plan")), cacheEndpointChat).rank)

	empty := classifySmart(rt, map[string]any{"messages": []any{msg("assistant", "x")}}, cacheEndpointChat)
	assert.Equal(t, smartReasonDefault, empty.reason)
	assert.Equal(t, 0, empty.rank)
}

func TestClassifyV2ScoredTextIsPromptFree(t *testing.T) {
	t.Parallel()
	rt := v2Router(t, []int{0, 150, 350}, nil)
	c := classifySmart(rt, chatMsgs(msg("user", "Fix this:\n```\nsecret-token-123\n```")), cacheEndpointChat)
	assert.Contains(t, c.scoredText, "fix this")
	assert.NotContains(t, c.scoredText, "secret-token-123", "pasted material stays out of the hash input")
}

func TestClassifierDefaultsToV1(t *testing.T) {
	t.Parallel()
	rt, err := compileSmartRouter("smart/auto", &SmartRouterConfig{Tiers: smartTestTiers()}, nil)
	require.NoError(t, err)
	assert.Equal(t, smartClassifierV1, rt.classifier)
	assert.Equal(t, defaultSmartRiskZ, rt.riskZ)
	assert.False(t, rt.shadowEnabled())
}

func TestCompileSmartRouter_ClassifierValidation(t *testing.T) {
	t.Parallel()
	neg, big := -1, maxSmartRiskZ+1
	ok, zero := maxSmartRiskZ, 0
	tests := []struct {
		mut     func(*SmartRouterConfig)
		name    string
		wantErr string
	}{
		{func(c *SmartRouterConfig) { c.Classifier = "v3" }, "bad classifier", "classifier"},
		{func(c *SmartRouterConfig) { c.ShadowClassifier = "v1" }, "bad shadowClassifier", "shadowClassifier"},
		{func(c *SmartRouterConfig) { c.ShadowClassifier = "v2"; c.ShadowWeights = zeroCodeShadow() }, "shadowClassifier with shadowWeights", "cannot both be set"},
		{func(c *SmartRouterConfig) { c.RiskZ = &neg }, "negative riskZ", "riskZ"},
		{func(c *SmartRouterConfig) { c.RiskZ = &big }, "oversized riskZ", "riskZ"},
		{func(c *SmartRouterConfig) { c.RiskZ = &ok }, "max riskZ", ""},
		{func(c *SmartRouterConfig) { c.RiskZ = &zero }, "zero riskZ", ""},
		{func(c *SmartRouterConfig) { c.Classifier = "v2"; c.ShadowClassifier = "v2" }, "both v2", ""},
		{func(c *SmartRouterConfig) { c.ShadowClassifier = "v2" }, "shadow v2 beside v1", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &SmartRouterConfig{Tiers: smartTestTiers()}
			tc.mut(cfg)
			_, err := compileSmartRouter("smart/auto", cfg, nil)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestShadowClassifier_RankAndLiveUnchanged(t *testing.T) {
	t.Parallel()
	const designAsk = "What architecture and strategy would you recommend for the audit trail service"
	plain := newClassifyTestRouter(t, nil)
	shadowed := newClassifyTestRouter(t, func(c *SmartRouterConfig) { c.ShadowClassifier = smartClassifierV2 })
	live := v2Router(t, []int{0, 300, 600}, nil)
	for _, text := range []string{designAsk, "hello", shadowCodeAsk, ""} {
		req := chatMsgs(msg("user", text))
		r1, w1, t1, i1 := classifySmartRequest(plain, req, cacheEndpointChat)
		r2, w2, t2, i2 := classifySmartRequest(shadowed, req, cacheEndpointChat)
		assert.Equal(t, []any{r1, w1, t1, i1}, []any{r2, w2, t2, i2}, "live result must not change for %q", text)
		c := classifySmart(shadowed, req, cacheEndpointChat)
		assert.Equal(t, classifySmart(live, req, cacheEndpointChat).rank, c.shadowRank, "shadow rank is the v2 rank for %q", text)
	}
	assert.Equal(t, -1, classifySmart(plain, chatMsgs(msg("user", designAsk)), cacheEndpointChat).shadowRank)
	assert.True(t, shadowed.shadowEnabled())
}

func shadowClassifierGateway(t *testing.T, statuses [3]int, redisAddr string) (func(string) int, *Gateway) {
	t.Helper()
	cfg, closeAll := routerSharedConfig(statuses, redisAddr, true)
	t.Cleanup(closeAll)
	cfg.SmartRouters[routerName].ShadowClassifier = smartClassifierV2
	return syncRouterGateway(t, cfg)
}

func TestShadowClassifierMatrix(t *testing.T) {
	chat, gw := shadowClassifierGateway(t, [3]int{200, 200, 200}, "")
	const designAsk = "What architecture and strategy would you recommend for the audit trail service"
	require.Equal(t, 200, chat("hello"))
	require.Equal(t, 200, chat(designAsk))

	v := routerView(t, gw)
	assert.True(t, v.ShadowEnabled)
	assert.Equal(t, "v1", v.Classifier)
	assert.Equal(t, "v2", v.ShadowClassifier)
	assert.Equal(t, defaultSmartRiskZ, v.RiskZ)
	require.Len(t, v.Stats.Shadow, 2)
	assert.Equal(t, adminRouterShadowView{Live: "t0", Shadow: "t0", Count: 1}, v.Stats.Shadow[0])
	assert.Equal(t, adminRouterShadowView{Live: "t0", Shadow: "t2", Count: 1}, v.Stats.Shadow[1], "v1 keeps the design ask on t0 while v2 would send it to t2")
	for _, o := range v.Stats.Outcomes {
		assert.Equal(t, "t0", o.Tier, "live routing stays on v1")
	}
}
