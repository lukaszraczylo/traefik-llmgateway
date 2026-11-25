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

const (
	anthropicCachedBody = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":8000,"cache_creation_input_tokens":2000,"cache_read_input_tokens":160000,"output_tokens":500}}`
	anthropicCachedSSE  = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[],\"usage\":{\"input_tokens\":8000,\"cache_creation_input_tokens\":2000,\"cache_read_input_tokens\":160000,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":500}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	openAICachedBody = `{"id":"c1","model":"gpt-test","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":170000,"completion_tokens":500,"prompt_tokens_details":{"cached_tokens":160000}}}`
	openAICachedSSE  = "data: {\"id\":\"c1\",\"model\":\"gpt-test\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"gpt-test\",\"choices\":[],\"usage\":{\"prompt_tokens\":170000,\"completion_tokens\":500,\"prompt_tokens_details\":{\"cached_tokens\":160000}}}\n\n" +
		"data: [DONE]\n\n"
)

const tpmCacheLimit = 1_200_000

// cachedUsageGateway serves upstream (content type ct, body upstreamBody) for
// both providers and gives alice a tokensPerMinute limit.
func cachedUsageGateway(t *testing.T, ct, upstreamBody string) *Gateway {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ct)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	t.Cleanup(srv.Close)
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"anthropic": {Type: providerTypeAnthropic, BaseURL: srv.URL, APIKey: "k", Models: []string{"claude-test"}},
		"openai":    {Type: providerTypeOpenAI, BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{TokensPerMinute: tpmCacheLimit}}}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	require.NoError(t, err)
	gw := h.(*Gateway)
	gw.limiter.spawn = func(f func()) { f() }
	return gw
}

func TestTokensPerMinute_ExcludesCachedPromptTokens(t *testing.T) {
	cases := []struct {
		name, path, ct, upstream, reqBody, authHeader string
	}{
		{"messages passthrough", "/v1/messages", "application/json", anthropicCachedBody, `{"model":"claude-test","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`, "x-api-key"},
		{"chat translated anthropic", "/v1/chat/completions", "application/json", anthropicCachedBody, `{"model":"claude-test","messages":[{"role":"user","content":"hi"}]}`, "Authorization"},
		{"chat translated anthropic stream", "/v1/chat/completions", "text/event-stream", anthropicCachedSSE, `{"model":"claude-test","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "Authorization"},
		{"native anthropic", "/anthropic/v1/messages", "application/json", anthropicCachedBody, `{"model":"claude-test","messages":[]}`, "Authorization"},
		{"native anthropic stream", "/anthropic/v1/messages", "text/event-stream", anthropicCachedSSE, `{"model":"claude-test","stream":true,"messages":[]}`, "Authorization"},
		{"chat openai", "/v1/chat/completions", "application/json", openAICachedBody, `{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`, "Authorization"},
		{"chat openai stream", "/v1/chat/completions", "text/event-stream", openAICachedSSE, `{"model":"gpt-test","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`, "Authorization"},
		{"native openai", "/openai/v1/chat/completions", "application/json", openAICachedBody, `{"model":"gpt-test","messages":[]}`, "Authorization"},
		{"native openai stream", "/openai/v1/chat/completions", "text/event-stream", openAICachedSSE, `{"model":"gpt-test","stream":true,"messages":[]}`, "Authorization"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gw := cachedUsageGateway(t, c.ct, c.upstream)
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(c.reqBody))
			if c.authHeader == "x-api-key" {
				req.Header.Set("x-api-key", "sk-alice")
			} else {
				req.Header.Set("Authorization", "Bearer sk-alice")
			}
			rec := httptest.NewRecorder()
			gw.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			now := time.Now()
			minIn, _ := gw.limiter.getCounter("user", "alice", metricTokIn, windowMin, now)
			minOut, _ := gw.limiter.getCounter("user", "alice", metricTokOut, windowMin, now)
			dayIn, _ := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, now)
			assert.EqualValues(t, 500, minOut)
			assert.Equal(t, int64(170000), dayIn, "day window keeps the full prompt count")
			assert.EqualValues(t, 10000, minIn, "minute window counts uncached input only (170K prompt - 160K cached)")
		})
	}
}

func TestUsageMinutePrompt(t *testing.T) {
	assert.EqualValues(t, 100, usage{prompt: 100}.minutePrompt())
	assert.EqualValues(t, 40, usage{prompt: 100, cached: 60}.minutePrompt())
	assert.EqualValues(t, 0, usage{prompt: 100, cached: 100}.minutePrompt())
	assert.EqualValues(t, 0, usage{prompt: 100, cached: 500}.minutePrompt(), "cached beyond prompt clamps to zero")
	assert.EqualValues(t, 100, usage{prompt: 100, cached: -5}.minutePrompt())
}

func TestTokensPerMinute_CachedHeavyClientIsNotLockedOut(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	l := newLimiter(nil, true)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{TokensPerMinute: 100_000}}}
	for i := 0; i < 5; i++ {
		require.Nil(t, l.checkAndCount(scopes), "request %d", i+1)
		l.account(scopes, usage{prompt: 170_000, cached: 160_000, completion: 1_000}, 0)
	}
	assert.Nil(t, l.checkAndCount(scopes), "55K uncached input + output stays under 100K")
	l.account(scopes, usage{prompt: 170_000, cached: 100_000, completion: 1_000}, 0)
	assert.NotNil(t, l.checkAndCount(scopes), "uncached usage past the limit still blocks")
}
