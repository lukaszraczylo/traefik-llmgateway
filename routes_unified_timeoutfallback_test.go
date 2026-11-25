package traefikllmgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tfMidKeyword = "mid-ask"

// tfGateway wires the "auto" router with the first-byte deadline and timeoutFallback set on the compiled router,
// bypassing the 1s config floor so tests stay fast.
func tfGateway(t *testing.T, a, b, c *routerBackend, mut func(*Config)) (http.Handler, *Gateway) {
	t.Helper()
	cfg := routerTestConfig(a, b, c, threeTiers())
	cfg.Metrics = &MetricsConfig{Enabled: true}
	if mut != nil {
		mut(cfg)
	}
	h, gw := buildRouterGateway(t, cfg)
	rt := gw.registry.routers[routerName]
	rt.firstByte = fbTimeout
	rt.timeoutFallback = true
	return h, gw
}

func fallbackOutcomes(gw *Gateway) int64 {
	_, outs := gw.routerStats.extraSnapshot()
	var n int64
	for k, v := range outs {
		if k.reason == routerReasonFallback {
			n += v
		}
	}
	return n
}

func fallbackEvents(gw *Gateway) []gatewayEvent {
	var out []gatewayEvent
	for _, ev := range gw.events.ring.snapshot(eventRingCap) {
		if ev.Kind == eventKindRouterTimeoutFallback {
			out = append(out, ev)
		}
	}
	return out
}

func TestTimeoutFallback_TopTierSlowServedByMid(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, gw := tfGateway(t, a, b, c, nil)

	start := time.Now()
	rec := routerChat(t, h, routerName, routerKeyword, nil)
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "beta/b1", rec.Header().Get(headerRoutedModel))
	assert.Equal(t, "t1", rec.Header().Get(headerRouterTier))
	assert.Equal(t, "t2", rec.Header().Get(headerRouterDowngraded), "the header names the tier the request fell back from")
	assert.Less(t, elapsed, fbSlow/2)
	assert.Equal(t, int64(1), c.count())
	assert.Equal(t, int64(1), b.count())
	assert.Equal(t, int64(0), a.count(), "only the tier directly below is tried")
	assert.Equal(t, int64(1), fallbackOutcomes(gw), "the request counts under the fallback reason")
	assert.Equal(t, int64(1), outcomeCount(gw, routerOutcomeSlow))
	_, escs := gw.routerStats.snapshot()
	assert.Empty(t, escs, "a step down is not an escalation")
	evs := fallbackEvents(gw)
	require.Len(t, evs, 1)
	assert.Equal(t, "beta/b1", evs[0].Model)
	assert.Contains(t, evs[0].Message, `"t2"`)
	assert.Contains(t, evs[0].Message, `"t1"`)
}

func TestTimeoutFallback_OffKeeps504(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, gw := tfGateway(t, a, b, c, nil)
	gw.registry.routers[routerName].timeoutFallback = false

	rec := routerChat(t, h, routerName, routerKeyword, nil)

	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Header().Get(headerRouterDowngraded))
	assert.Equal(t, int64(0), a.count()+b.count())
	assert.Zero(t, fallbackOutcomes(gw))
	assert.Empty(t, fallbackEvents(gw))
}

func TestTimeoutFallback_ExtraAttemptKeepsDeadlineThen504(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := fbBackend(fbSlowJSON, fbSlow)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, _ := tfGateway(t, a, b, c, nil)

	start := time.Now()
	rec := routerChat(t, h, routerName, routerKeyword, nil)
	elapsed := time.Since(start)

	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "did not start responding in time")
	assert.Equal(t, int64(1), c.count())
	assert.Equal(t, int64(1), b.count(), "one extra attempt, never a second")
	assert.Equal(t, int64(0), a.count())
	assert.GreaterOrEqual(t, elapsed, 2*fbTimeout)
	assert.Less(t, elapsed, 3*fbTimeout+fbTimeout/2, "the extra attempt keeps the first-byte deadline")
}

func TestTimeoutFallback_ExceedsMaxAttemptsByOne(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, gw := tfGateway(t, a, b, c, func(cfg *Config) { cfg.SmartRouters[routerName].MaxAttempts = 1 })

	rec := routerChat(t, h, routerName, routerKeyword, nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "beta/b1", rec.Header().Get(headerRoutedModel))
	assert.Equal(t, int64(1), c.count())
	assert.Equal(t, int64(1), b.count())
	assert.Equal(t, int64(1), fallbackOutcomes(gw))
}

func TestTimeoutFallback_StartsBelowFirstAttemptedTier(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := fbBackend(fbSlowJSON, fbSlow)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, _ := tfGateway(t, a, b, c, func(cfg *Config) {
		cfg.SmartRouters[routerName].KeywordRules = []SmartRouterKeywordRule{{Tier: "t1", Keywords: []string{tfMidKeyword}}}
	})

	rec := routerChat(t, h, routerName, tfMidKeyword, nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "alpha/a1", rec.Header().Get(headerRoutedModel), "t1 and t2 were slow, so the fallback is t0")
	assert.Equal(t, "t1", rec.Header().Get(headerRouterDowngraded), "the first attempted tier, not the last one tried")
	assert.Equal(t, int64(1), b.count())
	assert.Equal(t, int64(1), c.count())
}

func TestTimeoutFallback_NoTierBelowStartTier(t *testing.T) {
	a := fbBackend(fbSlowJSON, fbSlow)
	b := fbBackend(fbSlowJSON, fbSlow)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, gw := tfGateway(t, a, b, c, nil)

	rec := routerChat(t, h, routerName, "hi", nil)

	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	assert.Equal(t, int64(3), a.count()+b.count()+c.count(), "the plan went upward and nothing is lower than t0")
	assert.Zero(t, fallbackOutcomes(gw))
}

func TestTimeoutFallback_SkipsTierWithoutUsableCandidate(t *testing.T) {
	no := false
	tests := []struct {
		name    string
		mut     func(*Config)
		content any
		want    string
	}{
		{
			name:    "access denied on the tier below",
			mut:     func(cfg *Config) { cfg.Groups["default"].Models = []string{routerName, "a1", "c1"} },
			content: routerKeyword,
			want:    "alpha/a1",
		},
		{
			name: "context window too small on the tier below",
			mut: func(cfg *Config) {
				cfg.ModelMeta = map[string]*ModelMetaConfig{"b1": {ContextTokens: 50}}
			},
			content: routerKeyword + " " + strings.Repeat("word ", 400),
			want:    "alpha/a1",
		},
		{
			name: "no vision on the tier below",
			mut: func(cfg *Config) {
				cfg.ModelMeta = map[string]*ModelMetaConfig{"b1": {Vision: &no}}
			},
			content: []any{
				map[string]any{"type": "text", "text": routerKeyword},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
			},
			want: "alpha/a1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newRouterBackend(200, successRespBody)
			b := newRouterBackend(200, successRespBody)
			c := fbBackend(fbSlowJSON, fbSlow)
			defer closeBackends(a, b, c)
			h, _ := tfGateway(t, a, b, c, tt.mut)

			rec := routerChat(t, h, routerName, tt.content, nil)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, tt.want, rec.Header().Get(headerRoutedModel))
			assert.Equal(t, int64(0), b.count(), "the unusable model is never called")
			assert.Equal(t, "t2", rec.Header().Get(headerRouterDowngraded))
		})
	}
}

func TestTimeoutFallback_NeverUsesTooSmallModel(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, _ := tfGateway(t, a, b, c, func(cfg *Config) {
		cfg.ModelMeta = map[string]*ModelMetaConfig{"a1": {ContextTokens: 50}, "b1": {ContextTokens: 50}}
	})

	rec := routerChat(t, h, routerName, routerKeyword+" "+strings.Repeat("word ", 400), nil)

	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	assert.Equal(t, int64(0), a.count()+b.count(), "the 504 stands when every lower tier is too small")
}

func TestTimeoutFallback_NotAfterBytesWritten(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := fbBackend(fbEventThenStall, 4*fbTimeout)
	defer closeBackends(a, b, c)
	h, gw := tfGateway(t, a, b, c, nil)

	rec := routerChat(t, h, routerName, routerKeyword, map[string]any{"stream": true})

	assert.True(t, strings.HasPrefix(rec.Body.String(), fbChunk), rec.Body.String())
	assert.Equal(t, int64(0), a.count()+b.count())
	assert.Empty(t, rec.Header().Get(headerRouterDowngraded))
	assert.Zero(t, fallbackOutcomes(gw))
}

func TestTimeoutFallback_NotOnNonTimeoutFailure(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(503, routerFail)
	defer closeBackends(a, b, c)
	h, gw := tfGateway(t, a, b, c, nil)

	rec := routerChat(t, h, routerName, routerKeyword, nil)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, int64(0), a.count()+b.count())
	assert.Zero(t, fallbackOutcomes(gw))
	assert.Empty(t, fallbackEvents(gw))
}

func TestTimeoutFallback_NotWhenLastErrorIsClientError(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := fbBackend(fbSlowJSON, fbSlow)
	c := newRouterBackend(400, routerFail)
	defer closeBackends(a, b, c)
	h, gw := tfGateway(t, a, b, c, func(cfg *Config) {
		cfg.SmartRouters[routerName].KeywordRules = []SmartRouterKeywordRule{{Tier: "t1", Keywords: []string{tfMidKeyword}}}
	})

	rec := routerChat(t, h, routerName, tfMidKeyword, nil)

	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, int64(0), a.count(), "a client error after a slow attempt is not a timeout failure")
	assert.Zero(t, fallbackOutcomes(gw))
}

func TestTimeoutFallback_AnthropicMessagesShape(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, _ := tfGateway(t, a, b, c, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/messages", "sk-alice", map[string]any{
		"model": routerName, "max_tokens": 16,
		"messages": []any{map[string]any{"role": "user", "content": routerKeyword}},
	}))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "beta/b1", rec.Header().Get(headerRoutedModel))
	assert.Equal(t, "t2", rec.Header().Get(headerRouterDowngraded))
	var env struct {
		Type string `json:"type"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.Equal(t, "message", env.Type, "the Anthropic envelope survives the extra attempt")
}

func TestTimeoutFallback_AnthropicMessagesStillTimesOut(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := fbBackend(fbSlowJSON, fbSlow)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, _ := tfGateway(t, a, b, c, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/messages", "sk-alice", map[string]any{
		"model": routerName, "max_tokens": 16,
		"messages": []any{map[string]any{"role": "user", "content": routerKeyword}},
	}))

	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"type":"error"`)
}

func TestTimeoutFallback_StreamingPath(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := fbBackend(fbHeadersThenPingThenStall, 0)
	c := fbBackend(fbHeadersThenPingThenStall, fbSlow)
	defer closeBackends(a, b, c)
	h, gw := tfGateway(t, a, b, c, nil)

	start := time.Now()
	rec := routerChat(t, h, routerName, routerKeyword, map[string]any{"stream": true})
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "beta/b1", rec.Header().Get(headerRoutedModel))
	assert.Equal(t, "t2", rec.Header().Get(headerRouterDowngraded))
	assert.Contains(t, rec.Body.String(), "[DONE]")
	assert.Less(t, elapsed, fbSlow/2)
	assert.Equal(t, int64(1), fallbackOutcomes(gw))
}

func TestTimeoutFallback_ClientGoneNoFallback(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, _ := tfGateway(t, a, b, c, nil)

	ctx, cancel := context.WithTimeout(context.Background(), fbTimeout/2)
	defer cancel()
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", map[string]any{
		"model": routerName, "messages": []any{map[string]any{"role": "user", "content": routerKeyword}},
	}).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, int64(0), b.count()+a.count(), "a client that left gets no extra attempt")
}

func TestTimeoutFallback_SessionPinNotRaised(t *testing.T) {
	empty := `{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":0}}`
	a := newRouterBackend(200, empty)
	b := fbBackend(fbSlowJSON, fbSlow)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, gw := tfGateway(t, a, b, c, func(cfg *Config) {
		rc := cfg.SmartRouters[routerName]
		rc.Classifier = smartClassifierV2
		rc.Session = &SmartRouterSessionConfig{Enabled: true}
		rc.KeywordRules = []SmartRouterKeywordRule{{Tier: "t1", Keywords: []string{tfMidKeyword}}}
	})
	gw.routerSessions = newRouterSessions(nil, syncSpawn, time.Now, nil)
	meta := map[string]any{"metadata": map[string]any{"user_id": "u1"}}

	rec := routerChat(t, h, routerName, tfMidKeyword, meta)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "t0", rec.Header().Get(headerRouterTier))
	assert.Equal(t, routerReasonFallback, rec.Header().Get(headerRouterReason))
	assert.Equal(t, 1, onlySession(t, gw).pin, "a bad answer from the fallback tier must not pin the session upward")
}

func TestTimeoutFallback_NextTurnReplansNormally(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, _ := tfGateway(t, a, b, c, nil)

	first := routerChat(t, h, routerName, routerKeyword, nil)
	require.Equal(t, "t1", first.Header().Get(headerRouterTier))
	second := routerChat(t, h, routerName, "hi", nil)

	require.Equal(t, http.StatusOK, second.Code)
	assert.Equal(t, "t0", second.Header().Get(headerRouterTier))
	assert.Empty(t, second.Header().Get(headerRouterDowngraded), "the fallback does not outlive its request")
}

func TestTimeoutFallback_CalibrationNotCounted(t *testing.T) {
	gw, _ := calibGateway(t, &SmartRouterCalibrationConfig{Mode: smartCalibShadow})
	read := func(tier string) int64 {
		bucket := bucketFor(gw.limiter.now(), windowHour)
		v, ok := gw.limiter.storeGetMulti([]string{routerCalibKeyFor(routerName, tier, "n", bucket)})
		require.True(t, ok)
		return v[0]
	}
	p := &routerPlan{router: routerName, reason: smartReasonScore, startTier: "t1", calibrate: true}
	for _, tier := range []string{"t1", "t2", "t0"} {
		p.entries = append(p.entries, routerPlanEntry{tier: tier})
	}
	p.fallbackAt = 2
	p.fallbackFrom = "t1"
	gw.countCalibration(p, 2, true, true)
	gw.countCalibration(p, 2, true, false)
	gw.countCalibration(p, 0, false, false)
	for _, tier := range []string{"t0", "t1", "t2"} {
		assert.Zero(t, read(tier), tier)
	}
}

func TestTimeoutFallback_OutcomeReasonAndRequestReason(t *testing.T) {
	p := &routerPlan{router: routerName, reason: smartReasonScore, startTier: "t1"}
	p.entries = []routerPlanEntry{{tier: "t1"}, {tier: "t0"}}
	assert.Equal(t, smartReasonScore, routerRequestReason(p))
	p.fallbackAt = 1
	assert.Equal(t, routerReasonFallback, routerRequestReason(p))
	assert.Equal(t, routerReasonFallback, routerReasonNames[routerReasonsMax-1])
}

func TestTimeoutFallbackRank(t *testing.T) {
	t.Parallel()
	usable := func(set ...int) func(int) bool {
		return func(r int) bool {
			for _, s := range set {
				if s == r {
					return true
				}
			}
			return false
		}
	}
	tests := []struct {
		ok    func(int) bool
		name  string
		first int
		want  int
	}{
		{name: "directly below", first: 2, ok: usable(0, 1), want: 1},
		{name: "skips an empty tier", first: 2, ok: usable(0), want: 0},
		{name: "nothing below", first: 2, ok: usable(2, 3), want: -1},
		{name: "first tier has no lower tier", first: 0, ok: usable(0, 1, 2), want: -1},
		{name: "highest usable below wins", first: 3, ok: usable(0, 1, 2), want: 2},
		{name: "never at or above the first tier", first: 1, ok: usable(1, 2), want: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, routerFallbackRank(tt.first, tt.ok))
		})
	}
}

func TestTimeoutFallbackConfig(t *testing.T) {
	two := []SmartRouterTier{{Name: "t0", Models: []string{"a"}}, {Name: "t1", MinScore: 300, Models: []string{"b"}}}
	one := two[:1]
	tests := []struct {
		name    string
		wantErr string
		cfg     SmartRouterConfig
	}{
		{name: "valid", cfg: SmartRouterConfig{TimeoutFallback: true, FirstByteTimeout: "5s", Tiers: two}, wantErr: ""},
		{name: "off needs nothing", cfg: SmartRouterConfig{Tiers: one}, wantErr: ""},
		{name: "needs firstByteTimeout", cfg: SmartRouterConfig{TimeoutFallback: true, Tiers: two}, wantErr: "firstByteTimeout"},
		{name: "needs two tiers", cfg: SmartRouterConfig{TimeoutFallback: true, FirstByteTimeout: "5s", Tiers: one}, wantErr: "two tiers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt, err := compileSmartRouter("r", &tt.cfg, nil)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "timeoutFallback")
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.cfg.TimeoutFallback, rt.timeoutFallback)
		})
	}
}

func TestTimeoutFallbackShownInAdminRouterView(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, gw := tfGateway(t, a, b, c, nil)

	views := gw.buildAdminRouterViews()
	require.Len(t, views, 1)
	assert.True(t, views[0].TimeoutFallback)
	assert.Zero(t, views[0].FellBack)

	require.Equal(t, http.StatusOK, routerChat(t, h, routerName, routerKeyword, nil).Code)

	views = gw.buildAdminRouterViews()
	assert.Equal(t, int64(1), views[0].FellBack)
	raw, err := json.Marshal(views[0])
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"timeoutFallback":true`)
	assert.Contains(t, string(raw), `"fellBack":1`)
}
