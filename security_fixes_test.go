package traefikllmgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPassthroughUsageGateway builds a Gateway with one openai provider
// pointed at an upstream that always answers upstreamBody, and one user
// with the given limits and pricing.
func newPassthroughUsageGateway(t *testing.T, upstreamBody string, models []string, limits *LimitsConfig, pricing map[string]*ModelPricing) *Gateway {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	t.Cleanup(srv.Close)

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up", Models: models},
	}
	cfg.Pricing = pricing
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: limits},
	}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	require.NoError(t, err)
	gw, ok := h.(*Gateway)
	require.True(t, ok)
	gw.limiter.spawn = func(f func()) { f() }
	return gw
}

func doPassthrough(t *testing.T, gw *Gateway) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/native-endpoint", strings.NewReader(`{"model":"gpt-1"}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	return rec
}

func TestPassthrough_EchoedUnknownModel_MintsNoModelScope(t *testing.T) {
	t.Parallel()
	gw := newPassthroughUsageGateway(t, `{"model":"echo-unknown","usage":{"prompt_tokens":4,"completion_tokens":2}}`, []string{"gpt-known"}, &LimitsConfig{}, nil)

	require.Equal(t, http.StatusOK, doPassthrough(t, gw).Code)

	reqs, _ := gw.limiter.getCounter(kindModel, "openai/echo-unknown", metricReq, windowDay, time.Now())
	assert.Zero(t, reqs, "an id the provider does not serve must not get a model scope")
	tokIn, _ := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	assert.EqualValues(t, 4, tokIn, "user accounting must be unchanged")
}

func TestPassthrough_EchoedKnownModel_KeepsModelScope(t *testing.T) {
	t.Parallel()
	gw := newPassthroughUsageGateway(t, `{"model":"gpt-known","usage":{"prompt_tokens":4,"completion_tokens":2}}`, []string{"gpt-known"}, &LimitsConfig{}, nil)

	require.Equal(t, http.StatusOK, doPassthrough(t, gw).Code)

	req, ok := gw.limiter.getCounter(kindModel, "openai/gpt-known", metricReq, windowDay, time.Now())
	assert.True(t, ok)
	assert.EqualValues(t, 1, req)
}

func TestPassthrough_ImplausibleUsage_DoesNotLockOutBudget(t *testing.T) {
	t.Parallel()
	gw := newPassthroughUsageGateway(t, `{"model":"gpt-known","usage":{"prompt_tokens":9000000000000000000,"completion_tokens":1}}`, []string{"gpt-known"}, &LimitsConfig{TokensPerDay: 1000}, nil)

	require.Equal(t, http.StatusOK, doPassthrough(t, gw).Code)
	assert.Equal(t, http.StatusOK, doPassthrough(t, gw).Code, "an absurd reported figure must not exhaust the budget")
}

func TestPassthrough_EchoedUnpricedModel_BilledAtRequestModelPrice(t *testing.T) {
	t.Parallel()
	pricing := map[string]*ModelPricing{"openai/gpt-1": {InputPerM: 10, OutputPerM: 10}}
	gw := newPassthroughUsageGateway(t, `{"model":"unpriced-model","usage":{"prompt_tokens":1000,"completion_tokens":500}}`, nil, &LimitsConfig{CostPerDayUSD: 1}, pricing)

	require.Equal(t, http.StatusOK, doPassthrough(t, gw).Code)

	cost, ok := gw.limiter.getCounter("user", "alice", metricCost, windowDay, time.Now())
	assert.True(t, ok, "cost must accrue even though the upstream echoed an unpriced model")
	assert.EqualValues(t, 15000, cost, "1500 tokens at 10 USD per million = 15000 micro-USD")
}

func TestLimiterAccountWith_ImplausibleUsageDropped(t *testing.T) {
	t.Parallel()
	l := newLimiter(nil, true)
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{TokensPerDay: 1000, CostPerDayUSD: 1}}}

	l.account(scopes, usage{prompt: maxUsageTokensPerRequest + 1, completion: 7}, maxCostMicrosPerRequest+1)

	assert.Nil(t, l.checkAndCount(scopes), "implausible figures must not push the budgets over")
	out, _ := l.getCounter("user", "u", metricTokOut, windowDay, time.Now())
	assert.EqualValues(t, 7, out, "a plausible component of the same request is kept")
}

func TestEstimatePromptTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		bytes int64
		want  int64
	}{
		{"empty", 0, 0},
		{"rounds up", 5, 2},
		{"exact", 400, 100},
		{"capped", 32 << 20, maxEstimatedPromptTokens},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, estimatePromptTokens(tt.bytes))
		})
	}
}

func TestCappedAccountingBuffer_SharedBudgetBoundsConcurrentTees(t *testing.T) {
	t.Parallel()
	budget := newTeeBudget(10)
	a := &cappedAccountingBuffer{budget: budget}
	b := &cappedAccountingBuffer{budget: budget}

	_, _ = a.Write([]byte("0123456789"))
	_, err := b.Write([]byte("abc"))
	require.NoError(t, err)
	assert.False(t, a.truncated)
	assert.True(t, b.truncated, "a tee that cannot reserve budget must stop retaining bytes")
	assert.Zero(t, b.buf.Len())

	a.release()
	c := &cappedAccountingBuffer{budget: budget}
	_, _ = c.Write([]byte("abc"))
	assert.False(t, c.truncated, "released bytes return to the budget")
	a.release()
	var nilTee *cappedAccountingBuffer
	nilTee.release()
}

func TestCacheHit_AccountHits_ChargesServingUser(t *testing.T) {
	const respBody = `{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	for _, tt := range []struct {
		name        string
		accountHits bool
		wantTokIn   int64
	}{
		{"off: a hit is free", false, 10},
		{"on: a hit is charged", true, 20},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(respBody))
			}))
			defer srv.Close()
			gw := newCacheTestGateway(t, srv, nil, defaultCacheMaxBodyBytes)
			gw.cfg.Cache.AccountHits = tt.accountHits
			body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

			for _, want := range []string{"miss", "hit"} {
				rec := httptest.NewRecorder()
				gw.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
				require.Equal(t, want, rec.Header().Get("X-Llmgw-Cache"))
			}

			tokIn, _ := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
			assert.Equal(t, tt.wantTokIn, tokIn)
			reqs, _ := gw.limiter.getCounter("user", "alice", metricReq, windowDay, time.Now())
			assert.EqualValues(t, 2, reqs, "the request counter must not double count a hit")
		})
	}
}
