package traefikllmgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyThinkingFloor(t *testing.T) {
	const floor = 4096
	tests := []struct {
		req     map[string]any
		name    string
		wantOff bool
	}{
		{name: "small max_tokens, thinking unset", req: map[string]any{"max_tokens": 400}, wantOff: true},
		{name: "small max_completion_tokens", req: map[string]any{"max_completion_tokens": 400}, wantOff: true},
		{name: "max_tokens one below the floor", req: map[string]any{"max_tokens": floor - 1}, wantOff: true},
		{name: "max_tokens at the floor", req: map[string]any{"max_tokens": floor}},
		{name: "no output cap", req: map[string]any{}},
		{name: "reasoning_effort set", req: map[string]any{"max_tokens": 400, "reasoning_effort": "high"}},
		{name: "reasoning object set", req: map[string]any{"max_tokens": 400, "reasoning": map[string]any{"effort": "low"}}},
		{name: "thinking set", req: map[string]any{"max_tokens": 400, "thinking": map[string]any{"type": "enabled"}}},
		{name: "top-level enable_thinking set", req: map[string]any{"max_tokens": 400, "enable_thinking": true}},
		{name: "chat_template_kwargs enable_thinking set", req: map[string]any{"max_tokens": 400, "chat_template_kwargs": map[string]any{"enable_thinking": true}}},
		{name: "chat_template_kwargs thinking set", req: map[string]any{"max_tokens": 400, "chat_template_kwargs": map[string]any{"thinking": true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := make(map[string]any, len(tt.req))
			for k, v := range tt.req {
				body[k] = v
			}
			applyThinkingFloor(body, tt.req, floor)
			kw, _ := body["chat_template_kwargs"].(map[string]any)
			off := kw != nil && kw["enable_thinking"] == false
			assert.Equal(t, tt.wantOff, off, "body = %v", body)
		})
	}
}

func TestApplyThinkingFloor_ZeroFloorIsOff(t *testing.T) {
	body := map[string]any{"max_tokens": 1}
	applyThinkingFloor(body, body, 0)
	assert.NotContains(t, body, "chat_template_kwargs")
}

func TestApplyThinkingFloor_MergesWithoutTouchingTheClientMap(t *testing.T) {
	clientKW := map[string]any{"other": 1}
	req := map[string]any{"max_tokens": 400, "chat_template_kwargs": clientKW}
	body := map[string]any{"max_tokens": 400, "chat_template_kwargs": clientKW}
	applyThinkingFloor(body, req, 4096)
	assert.Equal(t, map[string]any{"other": 1, "enable_thinking": false}, body["chat_template_kwargs"])
	assert.Equal(t, map[string]any{"other": 1}, clientKW, "the client's map is shared with other attempts and must not change")
}

func TestValidateThinking(t *testing.T) {
	assert.NoError(t, validateThinking("p", providerTypeOpenAI, nil))
	assert.NoError(t, validateThinking("p", providerTypeOpenAI, &ThinkingConfig{OffBelowMaxTokens: 4096}))
	assert.Error(t, validateThinking("p", providerTypeAnthropic, &ThinkingConfig{OffBelowMaxTokens: 4096}), "anthropic has its own thinking switch")
	assert.Error(t, validateThinking("p", providerTypeOpenAI, &ThinkingConfig{OffBelowMaxTokens: -1}))
}

// capturingUpstream answers an OpenAI chat completion and records each request body.
func capturingUpstream(t *testing.T) (*httptest.Server, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(successRespBody))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), bodies...)
	}
}

func thinkingFloorGateway(t *testing.T, upstream string, tc *ThinkingConfig) http.Handler {
	t.Helper()
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: providerTypeOpenAI, BaseURL: upstream, APIKey: "k", Models: []string{"gpt-test"}, Thinking: tc},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	require.NoError(t, err)
	return h
}

func TestHandleChat_ThinkingFloor(t *testing.T) {
	tests := []struct {
		tc      *ThinkingConfig
		name    string
		wantOff bool
	}{
		{name: "opted-in provider turns thinking off", tc: &ThinkingConfig{OffBelowMaxTokens: 4096}, wantOff: true},
		{name: "provider without the setting is unchanged"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := capturingUpstream(t)
			h := thinkingFloorGateway(t, srv.URL, tt.tc)
			body := map[string]any{"model": "gpt-test", "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "title?"}}}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			bodies := got()
			require.Len(t, bodies, 1)
			kw, _ := bodies[0]["chat_template_kwargs"].(map[string]any)
			assert.Equal(t, tt.wantOff, kw != nil && kw["enable_thinking"] == false, "upstream body = %v", bodies[0])
		})
	}
}

func TestHandleMessages_ThinkingFloor_Translated(t *testing.T) {
	tests := []struct {
		extra   map[string]any
		name    string
		wantOff bool
	}{
		{name: "small max_tokens turns thinking off", wantOff: true},
		{name: "client thinking config is left alone", extra: map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, got := capturingUpstream(t)
			h := thinkingFloorGateway(t, srv.URL, &ThinkingConfig{OffBelowMaxTokens: 4096})
			body := map[string]any{"model": "gpt-test", "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "title?"}}}
			for k, v := range tt.extra {
				body[k] = v
			}
			b, err := json.Marshal(body)
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(b)))
			req.Header.Set("x-api-key", "sk-alice")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			bodies := got()
			require.Len(t, bodies, 1)
			kw, _ := bodies[0]["chat_template_kwargs"].(map[string]any)
			assert.Equal(t, tt.wantOff, kw != nil && kw["enable_thinking"] == false, "upstream body = %v", bodies[0])
		})
	}
}
