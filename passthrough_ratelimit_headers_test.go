package traefikllmgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func passthroughHeaderGateway(t *testing.T, limits *LimitsConfig) http.Handler {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "999")
		w.Header().Set("X-Ratelimit-Limit-Tokens", "12345")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "888")
		w.Header().Set("Ratelimit", `"up";r=1;t=1`)
		w.Header().Set("Ratelimit-Policy", `"up";q=1;w=1`)
		_, _ = w.Write([]byte(`{"model":"gpt-test","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	cfg := CreateConfig()
	cfg.RateLimitHeaders = true
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: providerTypeOpenAI, BaseURL: srv.URL, APIKey: "k"}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: limits}}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	require.NoError(t, err)
	return h
}

func passthroughCall(h http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-test"}`))
	req.Header.Set("Authorization", "Bearer sk-alice")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPassthrough_GatewayRateLimitHeadersWinOverUpstream(t *testing.T) {
	rec := passthroughCall(passthroughHeaderGateway(t, &LimitsConfig{RequestsPerMinute: 10, TokensPerMinute: 5000}))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "9", rec.Header().Get("x-ratelimit-remaining-requests"))
	assert.Equal(t, "5000", rec.Header().Get("x-ratelimit-limit-tokens"))
	assert.Equal(t, "9", rec.Header().Get("anthropic-ratelimit-requests-remaining"))
	assert.Contains(t, rec.Header().Get("RateLimit"), `"requests";r=9`)
	assert.NotContains(t, rec.Header().Get("RateLimit"), `"up"`)
	assert.NotContains(t, rec.Header().Get("RateLimit-Policy"), `"up"`)
}

func TestPassthrough_UpstreamRateLimitHeadersKeptWhenGatewayEmitsNone(t *testing.T) {
	rec := passthroughCall(passthroughHeaderGateway(t, &LimitsConfig{RequestsPerDay: 100}))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "999", rec.Header().Get("x-ratelimit-remaining-requests"))
	assert.Equal(t, "12345", rec.Header().Get("x-ratelimit-limit-tokens"))
	assert.Equal(t, "888", rec.Header().Get("anthropic-ratelimit-requests-remaining"))
	assert.Equal(t, `"up";r=1;t=1`, rec.Header().Get("RateLimit"))
}
