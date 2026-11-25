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

func exemptGateway(t *testing.T, exempt []string, limits *LimitsConfig, kinds map[string]string) (http.Handler, *Gateway) {
	t.Helper()
	groups := map[string]*GroupConfig{"friends": {}}
	users := []*UserConfig{{Name: "a", Group: "friends", APIKey: "sk-a", Limits: limits}}
	return memberLimitsGateway(t, groups, users, withRateLimitHeaders, func(c *Config) {
		c.LimitExemptKinds = exempt
		c.Providers["openai"].Kinds = kinds
	})
}

func exemptPost(h http.Handler, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-a")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const embedBody = `{"model":"gpt-test","input":"hi"}`

func TestLimitExempt_EmbeddingsNeverLimitedWhileChatIs(t *testing.T) {
	h, gw := exemptGateway(t, []string{"embedding"}, &LimitsConfig{
		RequestsPerMinute: 2, TokensPerMinute: 100, RequestsPerDay: 3,
		Pacing: &PacingConfig{StartPct: 1, MaxDelay: "2s"},
	}, nil)
	start := time.Now()
	for i := 0; i < 6; i++ {
		rec := exemptPost(h, "/v1/embeddings", embedBody)
		require.Equal(t, http.StatusOK, rec.Code, "embedding %d: %s", i+1, rec.Body.String())
		assert.Empty(t, rec.Header().Get(pacedHeader), "embedding %d is never paced", i+1)
		for k := range rec.Header() {
			assert.False(t, isRateLimitHeader(k), "exempt responses carry no rate-limit header, got %s", k)
		}
	}
	assert.Less(t, time.Since(start), time.Second)

	n, _ := gw.limiter.getCounter("user", "a", metricReq, windowDay, gw.limiter.now())
	assert.EqualValues(t, 6, n, "exempt requests are still counted")
	tok, _ := gw.limiter.getCounter("user", "a", metricTokIn, windowDay, gw.limiter.now())
	assert.EqualValues(t, 6*30, tok, "exempt usage is still accounted")

	rec := memberLimitsChat(t, h, "sk-a")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "chat is still limited")
	assert.NotEmpty(t, rec.Header().Get("x-ratelimit-limit-requests"))
}

func TestLimitExempt_UnsetKeepsEmbeddingsLimited(t *testing.T) {
	h, _ := exemptGateway(t, nil, &LimitsConfig{RequestsPerMinute: 2}, nil)
	assert.Equal(t, http.StatusOK, exemptPost(h, "/v1/embeddings", embedBody).Code)
	rec := exemptPost(h, "/v1/embeddings", embedBody)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "0", rec.Header().Get("x-ratelimit-remaining-requests"))
	assert.Equal(t, http.StatusTooManyRequests, exemptPost(h, "/v1/embeddings", embedBody).Code)
}

func TestLimitExempt_OtherKindStaysLimited(t *testing.T) {
	h, _ := exemptGateway(t, []string{"reranking"}, &LimitsConfig{RequestsPerMinute: 1}, nil)
	assert.Equal(t, http.StatusOK, exemptPost(h, "/v1/embeddings", embedBody).Code)
	assert.Equal(t, http.StatusTooManyRequests, exemptPost(h, "/v1/embeddings", embedBody).Code, "embedding is not in the exempt list")
}

func TestLimitExempt_RerankPassthroughPath(t *testing.T) {
	for name, kinds := range map[string]map[string]string{
		"configured path kind": {"/v1/score": "reranking"},
		"built-in rerank path": nil,
	} {
		t.Run(name, func(t *testing.T) {
			path := "/openai/v1/rerank"
			if kinds != nil {
				path = "/openai/v1/score"
			}
			h, gw := exemptGateway(t, []string{"reranking"}, &LimitsConfig{RequestsPerMinute: 1}, kinds)
			for i := 0; i < 4; i++ {
				rec := exemptPost(h, path, `{"query":"q","documents":["a"]}`)
				require.Equal(t, http.StatusOK, rec.Code, "rerank %d: %s", i+1, rec.Body.String())
			}
			n, _ := gw.limiter.getCounter("user", "a", metricReq, windowDay, gw.limiter.now())
			assert.EqualValues(t, 4, n, "still counted")
		})
	}
}

func TestLimitExempt_RerankPassthroughByModelKind(t *testing.T) {
	h, _ := exemptGateway(t, []string{"reranking"}, &LimitsConfig{RequestsPerMinute: 1},
		map[string]string{"bge-reranker": "reranking"})
	for i := 0; i < 4; i++ {
		rec := exemptPost(h, "/openai/v1/score", `{"model":"bge-reranker","query":"q"}`)
		require.Equal(t, http.StatusOK, rec.Code, "scored %d: %s", i+1, rec.Body.String())
	}
}

func TestLimitExempt_UnlistedModelKindStaysLimited(t *testing.T) {
	h, _ := exemptGateway(t, []string{"reranking"}, &LimitsConfig{RequestsPerMinute: 1},
		map[string]string{"bge-reranker": "reranking"})
	assert.Equal(t, http.StatusOK, exemptPost(h, "/openai/v1/score", `{"model":"gpt-test"}`).Code)
	assert.Equal(t, http.StatusTooManyRequests, exemptPost(h, "/openai/v1/score", `{"model":"gpt-test"}`).Code)
}

func TestLimitExempt_TokenBudgetsNeverBlockExemptRequests(t *testing.T) {
	h, _ := exemptGateway(t, []string{"embedding"}, &LimitsConfig{TokensPerMinute: 10, TokensPerDay: 10, CostPerDayUSD: 0.000001}, nil)
	for i := 0; i < 4; i++ {
		require.Equal(t, http.StatusOK, exemptPost(h, "/v1/embeddings", embedBody).Code, "embedding %d", i+1)
	}
}

func TestLimitExempt_UnsetPassthroughDoesNotPeekBody(t *testing.T) {
	h, _ := exemptGateway(t, nil, &LimitsConfig{RequestsPerMinute: 1}, map[string]string{"/v1/rerank": "reranking"})
	assert.Equal(t, http.StatusOK, exemptPost(h, "/openai/v1/rerank", `{"q":1}`).Code)
	assert.Equal(t, http.StatusTooManyRequests, exemptPost(h, "/openai/v1/rerank", `{"q":1}`).Code)
}

func TestLimitExempt_RejectsBlankKind(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: providerTypeOpenAI, BaseURL: "http://127.0.0.1:1", APIKey: "k"}}
	cfg.LimitExemptKinds = []string{"embedding", " "}
	_, err := newGateway(context.Background(), nil, cfg, "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "limitExemptKinds")
}
