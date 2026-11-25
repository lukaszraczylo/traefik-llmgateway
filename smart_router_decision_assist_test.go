package traefikllmgateway

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assistRouter(t *testing.T, band int, live bool, session bool) *smartRouter {
	t.Helper()
	rt := newClassifyTestRouter(t, func(c *SmartRouterConfig) {
		c.Classifier = smartClassifierV2
		c.Tiers = []SmartRouterTier{
			{Name: "cheap", MinScore: 0, Models: []string{"openai/gpt-mini"}},
			{Name: "mid", MinScore: 350, Models: []string{"openai/gpt-mid"}},
			{Name: "top", MinScore: 600, Models: []string{"openai/gpt-top"}},
		}
		if session {
			c.Session = &SmartRouterSessionConfig{Enabled: true}
		}
	})
	rt.decision = &smartDecisionCfg{band: band, minProb: 700, live: live}
	return rt
}

type stubAssist struct {
	ans   decisionAnswer
	calls int
}

func (s *stubAssist) fn(*smartFeatures) decisionAnswer {
	s.calls++
	return s.ans
}

const assistAsk = "hello there"

func TestSmartNearBoundary(t *testing.T) {
	rt := assistRouter(t, 100, true, false)
	assert.True(t, smartNearBoundary(rt, 300))
	assert.False(t, smartNearBoundary(rt, 120))
	assert.True(t, smartNearBoundary(rt, 690))
}

func TestApplyDecision(t *testing.T) {
	rt := assistRouter(t, 100, true, false)
	tests := []struct {
		name        string
		wantOutcome string
		ans         decisionAnswer
		rank        int
		wantRank    int
	}{
		{"override up one step", decisionOverride, decisionAnswer{rank: 2, prob: 900}, 0, 1},
		{"override down one step", decisionOverride, decisionAnswer{rank: 0, prob: 900}, 2, 1},
		{"agree", decisionAgree, decisionAnswer{rank: 1, prob: 800}, 1, 1},
		{"low confidence", decisionLowConf, decisionAnswer{rank: 0, prob: 650}, 1, 1},
		{"breaker", decisionBreaker, decisionAnswer{outcome: decisionBreaker}, 1, 1},
		{"timeout", decisionTimeout, decisionAnswer{outcome: decisionTimeout, rank: 2, prob: 999}, 1, 1},
		{"cache override", decisionCache, decisionAnswer{outcome: decisionCache, rank: 1, prob: 900}, 0, 1},
		{"cache low confidence keeps rank", decisionCache, decisionAnswer{outcome: decisionCache, rank: 2, prob: 100}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRank, gotOutcome := applyDecision(rt, tt.rank, tt.ans)
			assert.Equal(t, tt.wantRank, gotRank)
			assert.Equal(t, tt.wantOutcome, gotOutcome)
		})
	}
}

func TestClassifyDecisionLiveOverrides(t *testing.T) {
	rt := assistRouter(t, 1000, true, false)
	plain := classifySmartWith(rt, userReq(assistAsk), cacheEndpointChat, nil, nil, nil)
	stub := &stubAssist{ans: decisionAnswer{rank: 2, prob: 900}}
	cls := classifySmartWith(rt, userReq(assistAsk), cacheEndpointChat, nil, nil, stub.fn)
	assert.Equal(t, 1, stub.calls)
	assert.Equal(t, plain.rank+1, cls.rank)
	assert.Equal(t, smartReasonModel, cls.reason)
	assert.Equal(t, decisionOverride, cls.decisionOutcome)
	assert.Equal(t, 2, cls.decisionTier)
	assert.Equal(t, plain.rank, cls.heuristicRank)
}

func TestClassifyDecisionShadowKeepsRank(t *testing.T) {
	rt := assistRouter(t, 1000, false, false)
	plain := classifySmartWith(rt, userReq(assistAsk), cacheEndpointChat, nil, nil, nil)
	stub := &stubAssist{ans: decisionAnswer{rank: 2, prob: 900}}
	cls := classifySmartWith(rt, userReq(assistAsk), cacheEndpointChat, nil, nil, stub.fn)
	assert.Equal(t, 1, stub.calls)
	assert.Equal(t, plain.rank, cls.rank)
	assert.Equal(t, plain.reason, cls.reason)
	assert.Equal(t, decisionOverride, cls.decisionOutcome)
	assert.Equal(t, 2, cls.decisionTier)
}

func TestClassifyDecisionTransportFailureHasNoTier(t *testing.T) {
	rt := assistRouter(t, 1000, true, false)
	plain := classifySmartWith(rt, userReq(assistAsk), cacheEndpointChat, nil, nil, nil)
	stub := &stubAssist{ans: decisionAnswer{outcome: decisionTimeout}}
	cls := classifySmartWith(rt, userReq(assistAsk), cacheEndpointChat, nil, nil, stub.fn)
	assert.Equal(t, plain.rank, cls.rank)
	assert.Equal(t, plain.reason, cls.reason)
	assert.Equal(t, decisionTimeout, cls.decisionOutcome)
	assert.Equal(t, -1, cls.decisionTier)
}

func TestClassifyDecisionFarFromBoundarySkips(t *testing.T) {
	rt := assistRouter(t, 100, true, false)
	stub := &stubAssist{ans: decisionAnswer{rank: 2, prob: 900}}
	cls := classifySmartWith(rt, userReq(assistAsk), cacheEndpointChat, nil, nil, stub.fn)
	require.Less(t, cls.score, 100, "the greeting must score far from the first boundary")
	assert.Equal(t, 0, stub.calls)
	assert.Equal(t, -1, cls.decisionTier)
	assert.Equal(t, "", cls.decisionOutcome)
}

func TestClassifyDecisionNoAssistKeepsDefaults(t *testing.T) {
	rt := assistRouter(t, 100, true, false)
	cls := classifySmartWith(rt, userReq(assistAsk), cacheEndpointChat, nil, nil, nil)
	assert.Equal(t, -1, cls.decisionTier)
	assert.Equal(t, "", cls.decisionOutcome)
}

func TestClassifyDecisionSessionPinnedSkips(t *testing.T) {
	rt := assistRouter(t, 1000, true, true)
	stub := &stubAssist{ans: decisionAnswer{rank: 0, prob: 900}}
	f := extractSmartV2(userReq(assistAsk), cacheEndpointChat)
	sc := &smartSessionRun{rt: rt, key: "k", now: 1010, sess: smartSession{found: true, pin: 2, loop: 2, seenAt: 1000, askAt: 1000, askHash: 1, msgCount: 1}}
	cls := classifySmartWith(rt, userReq(assistAsk), cacheEndpointChat, &f, sc, stub.fn)
	require.Equal(t, smartReasonSession, cls.reason)
	assert.Equal(t, 0, stub.calls)
	assert.Equal(t, -1, cls.decisionTier)
}

func TestClassifyDecisionFloorStillRaises(t *testing.T) {
	rt := assistRouter(t, 1000, true, false)
	req := userReq(assistAsk)
	req["reasoning_effort"] = "high"
	stub := &stubAssist{ans: decisionAnswer{rank: 0, prob: 990}}
	cls := classifySmartWith(rt, req, cacheEndpointChat, nil, nil, stub.fn)
	assert.Equal(t, 2, cls.rank)
	assert.Equal(t, smartReasonFloor, cls.reason)
}

func TestExtractSmartV2KeepsRawAsk(t *testing.T) {
	f := extractSmartV2(userReq("Fix this:\n```\ncode\n```"), cacheEndpointChat)
	assert.Equal(t, "Fix this:\n```\ncode\n```", f.ask)
}

func TestRouterReasonNamesIncludeModel(t *testing.T) {
	assert.Equal(t, 11, routerReasonsMax)
	assert.Equal(t, smartReasonBump, routerReasonNames[7])
	assert.Equal(t, smartReasonModel, routerReasonNames[8])
	assert.Equal(t, routerReasonFallback, routerReasonNames[routerReasonsMax-1])
}

func TestPlanSmartRouterDecisionTimeoutFallsBack(t *testing.T) {
	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(400 * time.Millisecond):
		case <-r.Context().Done():
		}
		okHandler(w, r)
	}
	f := newDecisionFixture(t, 3, "50ms", slow)
	f.rt.decision.live = true
	f.rt.decision.band = 1000
	base := newDecisionFixture(t, 3, "50ms", slow)
	base.rt.decision = nil

	req := userReq(assistAsk)
	got, err := f.g.planSmartRouter(context.Background(), f.rt, req, cacheEndpointChat, nil, &group{}, nil)
	require.NoError(t, err)
	want, err := base.g.planSmartRouter(context.Background(), base.rt, req, cacheEndpointChat, nil, &group{}, nil)
	require.NoError(t, err)
	assert.Equal(t, want.startTier, got.startTier)
	assert.Equal(t, decisionTimeout, got.decisionOutcome)
	assert.Equal(t, got.startTier, got.heuristicTier)
	assert.Equal(t, "", got.decisionTier)
}
