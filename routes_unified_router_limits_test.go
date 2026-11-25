package traefikllmgateway

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// routerLimitsGateway wires the three-tier "auto" router with alice's limits and rate-limit headers on.
func routerLimitsGateway(t *testing.T, a, b, c *routerBackend, lim *LimitsConfig, mut func(*Config)) (http.Handler, *Gateway) {
	t.Helper()
	cfg := routerTestConfig(a, b, c, threeTiers())
	cfg.RateLimitHeaders = true
	cfg.Users.Inline[0].Limits = lim
	if mut != nil {
		mut(cfg)
	}
	h, gw := buildRouterGateway(t, cfg)
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return now }
	return h, gw
}

func userCounter(t *testing.T, gw *Gateway, metric, window string) int64 {
	t.Helper()
	v, _ := gw.limiter.getCounter("user", "alice", metric, window, gw.limiter.now())
	return v
}

func TestRouterLimits_EscalationCountsOneRequestAndSettlesPrecharge(t *testing.T) {
	a := newRouterBackend(503, routerFail)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, gw := routerLimitsGateway(t, a, b, c, &LimitsConfig{RequestsPerMinute: 5, TokensPerMinute: 10000}, func(cfg *Config) {
		cfg.MaxConcurrency = 3
	})

	rec := routerChat(t, h, routerName, "hi", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int64(1), a.count())
	assert.Equal(t, int64(1), b.count())

	assert.EqualValues(t, 1, userCounter(t, gw, metricReq, windowMin), "one escalated request is one request")
	assert.EqualValues(t, 1, userCounter(t, gw, metricReq, windowDay))
	assert.EqualValues(t, 3, userCounter(t, gw, metricTokIn, windowDay), "only the served attempt's usage is billed")
	assert.EqualValues(t, 2, userCounter(t, gw, metricTokOut, windowDay))
	assert.EqualValues(t, 3, userCounter(t, gw, metricTokIn, windowMin), "pre-charge is settled to the final usage, not left in the window")
	assert.EqualValues(t, 2, userCounter(t, gw, metricTokOut, windowMin))

	assert.Equal(t, "5", rec.Header().Get("x-ratelimit-limit-requests"))
	assert.Equal(t, "4", rec.Header().Get("x-ratelimit-remaining-requests"), "headers describe admission once, not per attempt")
	assert.Equal(t, "10000", rec.Header().Get("x-ratelimit-limit-tokens"))
	policy := rec.Header().Get("RateLimit-Policy")
	assert.Contains(t, policy, `"requests";q=5;w=60`)
	assert.Contains(t, policy, `"concurrency";q=3;qu="concurrent-requests"`)
	assert.Equal(t, "beta/b1", rec.Header().Get(headerRoutedModel))
	assert.Equal(t, "t1", rec.Header().Get(headerRouterTier))
}

func TestRouterLimits_EscalationDoesNotSelfThrottle(t *testing.T) {
	a := newRouterBackend(503, routerFail)
	b := newRouterBackend(503, routerFail)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, gw := routerLimitsGateway(t, a, b, c, &LimitsConfig{RequestsPerMinute: 1}, nil)

	rec := routerChat(t, h, routerName, "hi", nil)
	require.Equal(t, http.StatusOK, rec.Code, "three attempts must fit one request slot: %s", rec.Body.String())
	assert.EqualValues(t, 1, userCounter(t, gw, metricReq, windowMin))

	rec = routerChat(t, h, routerName, "hi", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "the next request is over the limit")
	assert.Equal(t, int64(1), a.count(), "a rejected request reaches no upstream")
}

func TestRouterLimits_FailedRouterRequestReleasesPrecharge(t *testing.T) {
	a := newRouterBackend(503, routerFail)
	b := newRouterBackend(503, routerFail)
	c := newRouterBackend(503, routerFail)
	defer closeBackends(a, b, c)
	h, gw := routerLimitsGateway(t, a, b, c, &LimitsConfig{RequestsPerMinute: 5, TokensPerMinute: 10000}, nil)

	rec := routerChat(t, h, routerName, "hi", nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.EqualValues(t, 0, userCounter(t, gw, metricTokIn, windowMin), "no served attempt leaves no tokens in the minute window")
	assert.EqualValues(t, 1, userCounter(t, gw, metricReq, windowMin))
}

func TestRouterLimits_PacingWaitsBeforeFirstUpstreamAttempt(t *testing.T) {
	var firstArrival, secondArrival atomic.Int64
	slot := func(dst *atomic.Int64, status int, body string) *routerBackend {
		b := &routerBackend{}
		b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			dst.Store(time.Now().UnixNano())
			atomic.AddInt64(&b.calls, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		return b
	}
	a := slot(&firstArrival, 503, routerFail)
	b := slot(&secondArrival, 200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, _ := routerLimitsGateway(t, a, b, c, &LimitsConfig{
		RequestsPerMinute: 2, Pacing: &PacingConfig{StartPct: 1, MaxDelay: "400ms"},
	}, nil)

	start := time.Now()
	rec := routerChat(t, h, routerName, "hi", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	ms, err := strconv.Atoi(rec.Header().Get(pacedHeader))
	require.NoError(t, err, "paced header must be set")
	require.Positive(t, ms)
	delay := time.Duration(ms) * time.Millisecond

	assert.GreaterOrEqual(t, time.Duration(firstArrival.Load()-start.UnixNano()), delay*9/10, "the first attempt waits for the pacing delay")
	assert.Less(t, time.Duration(secondArrival.Load()-firstArrival.Load()), delay, "escalation is not paced again")
	assert.Equal(t, "beta/b1", rec.Header().Get(headerRoutedModel))
}

func TestRouterLimits_TimeoutFallbackCountsOneRequest(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := fbBackend(fbSlowJSON, fbSlow)
	defer closeBackends(a, b, c)
	h, gw := tfGateway(t, a, b, c, func(cfg *Config) {
		cfg.RateLimitHeaders = true
		cfg.Users.Inline[0].Limits = &LimitsConfig{RequestsPerMinute: 1, TokensPerMinute: 10000}
	})
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return now }

	rec := routerChat(t, h, routerName, routerKeyword, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "beta/b1", rec.Header().Get(headerRoutedModel))
	assert.Equal(t, "0", rec.Header().Get("x-ratelimit-remaining-requests"))
	assert.EqualValues(t, 1, userCounter(t, gw, metricReq, windowMin), "the slow attempt and the fallback attempt are one request")
	assert.EqualValues(t, 3, userCounter(t, gw, metricTokIn, windowMin))
}

func TestRouterLimits_OutcomeFlagsStillReachUsage(t *testing.T) {
	cut := `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":600}}`
	a := newRouterBackend(200, cut)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, gw := routerLimitsGateway(t, a, b, c, &LimitsConfig{RequestsPerMinute: 5}, func(cfg *Config) {
		cfg.Metrics = &MetricsConfig{Enabled: true}
	})

	rec := routerChat(t, h, routerName, "hi", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, int64(1), outcomeCount(gw, routerOutcomeBad), "a length cut still counts as a bad router outcome")
	assert.EqualValues(t, 600, userCounter(t, gw, metricTokOut, windowDay))
}

func TestRouterLimits_EmbeddingsWithRouterNameStill400WhileEmbeddingsExempt(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, gw := routerLimitsGateway(t, a, b, c, &LimitsConfig{RequestsPerMinute: 1}, func(cfg *Config) {
		cfg.LimitExemptKinds = []string{"embedding"}
	})

	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/embeddings", "sk-alice", map[string]any{"model": routerName, "input": "hi"}))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "embeddings")
	}
	assert.Equal(t, int64(0), a.count()+b.count()+c.count())

	rec := routerChat(t, h, routerName, "hi", nil)
	assert.Equal(t, http.StatusOK, rec.Code, "the rejected embeddings requests spent no chat request slot: %s", rec.Body.String())
	assert.EqualValues(t, 1, userCounter(t, gw, metricReq, windowMin))
}
