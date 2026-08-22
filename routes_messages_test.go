package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newMessagesRequest builds a POST /v1/messages request, JSON-encoding
// body. authHeader/authValue let a test choose which credential header to
// send; authHeader == "" sends no credential at all.
func newMessagesRequest(t *testing.T, body map[string]any, authHeader, authValue string) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, messagesPath, bytes.NewReader(b))
	if authHeader != "" {
		req.Header.Set(authHeader, authValue)
	}
	return req
}

// newMessagesTestGateway builds a *Gateway wired with an "anthropic"
// provider pointed at anthropicURL (skipped when "") and an "openai"
// provider pointed at openaiURL (skipped when ""), one permissive
// "default" group, and one inline user "alice" — the shared fixture
// every handleMessages test below starts from.
func newMessagesTestGateway(t *testing.T, anthropicURL, openaiURL string) *Gateway {
	t.Helper()
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{}
	if anthropicURL != "" {
		cfg.Providers["anthropic"] = &ProviderConfig{Type: providerTypeAnthropic, BaseURL: anthropicURL, APIKey: "sk-ant-up", Models: []string{"claude-test"}} // #nosec G101 -- test fixture literal, not a real credential
	}
	if openaiURL != "" {
		cfg.Providers["openai"] = &ProviderConfig{Type: providerTypeOpenAI, BaseURL: openaiURL, APIKey: "sk-oai-up", Models: []string{"gpt-test"}}
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}
	return gw
}

// TestHandleMessages_AnthropicProvider_Passthrough proves a /v1/messages
// request resolved to an anthropic-type provider is forwarded to that
// provider's own Messages endpoint and its response copied back to the
// client byte-for-byte (brief: "pass through, no translation of the
// message body"), while still authenticating upstream and accounting
// usage exactly like every other route.
func TestHandleMessages_AnthropicProvider_Passthrough(t *testing.T) {
	const anthResp = `{"id":"msg_01ABC","type":"message","role":"assistant","content":[{"type":"text","text":"hi there"}],"model":"claude-real","stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":6}}`

	var gotBody map[string]any
	var gotAPIKey, gotVersion, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthResp))
	}))
	defer srv.Close()

	gw := newMessagesTestGateway(t, srv.URL, "")

	body := map[string]any{
		"model":      "claude-test",
		"max_tokens": 100,
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	}
	req := newMessagesRequest(t, body, "x-api-key", "sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != anthResp {
		t.Errorf("body = %q, want verbatim upstream body %q (no translation)", rec.Body.String(), anthResp)
	}
	if gotPath != anthropicMessagesPath {
		t.Errorf("upstream path = %q, want %q", gotPath, anthropicMessagesPath)
	}
	if gotAPIKey != "sk-ant-up" { // #nosec G101 -- test fixture literal, not a real credential
		t.Errorf("upstream x-api-key = %q, want sk-ant-up", gotAPIKey)
	}
	if gotVersion != anthropicAPIVersion {
		t.Errorf("upstream anthropic-version = %q, want %q", gotVersion, anthropicAPIVersion)
	}
	if gotBody["model"] != "claude-test" {
		t.Errorf("upstream model = %v, want claude-test (resolved upstream id)", gotBody["model"])
	}
	if _, hasAlias := gotBody[gatewayAliasKey]; hasAlias {
		t.Error("upstream request carries gatewayAliasKey; must never reach an upstream provider")
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 12 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 12", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 6 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 6", tokOut, ok)
	}
}

// TestHandleMessages_OpenAIProvider_TranslatesBothDirections proves a
// /v1/messages request resolved to an openai-type provider is translated
// to OpenAI's chat-completion shape going out (Anthropic's top-level
// "system" field folded into a role:"system" message) and the upstream's
// OpenAI-shaped response translated back to Anthropic's Messages shape
// coming back, with usage accounted from the translated numbers.
func TestHandleMessages_OpenAIProvider_TranslatesBothDirections(t *testing.T) {
	const oaiResp = `{"id":"chatcmpl-99","object":"chat.completion","model":"gpt-real","choices":[{"index":0,"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":4}}`

	var gotReq map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(oaiResp))
	}))
	defer srv.Close()

	gw := newMessagesTestGateway(t, "", srv.URL)

	body := map[string]any{
		"model":      "gpt-test",
		"max_tokens": 100,
		"system":     "You are terse.",
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	}
	req := newMessagesRequest(t, body, "x-api-key", "sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", gotPath)
	}

	msgs, _ := gotReq["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("upstream messages = %v, want 2 (system + user)", gotReq["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "You are terse." {
		t.Errorf("upstream messages[0] = %v, want the system message translated into OpenAI's shape", first)
	}
	if _, hasSystem := gotReq["system"]; hasSystem {
		t.Error("upstream request still carries Anthropic's own \"system\" field; want it folded into messages[]")
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode client response: %v", err)
	}
	if got["type"] != "message" || got["role"] != "assistant" {
		t.Errorf("response type/role = %v/%v, want message/assistant (Anthropic shape)", got["type"], got["role"])
	}
	content, _ := got["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("response content = %v, want one text block", got["content"])
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hi there" {
		t.Errorf("response content[0] = %v, want {type: text, text: \"hi there\"}", block)
	}
	if got["model"] != "gpt-test" {
		t.Errorf("response model = %v, want gpt-test (the client's own requested alias echoed back)", got["model"])
	}
	if got["stop_reason"] != "end_turn" {
		t.Errorf("response stop_reason = %v, want end_turn", got["stop_reason"])
	}

	usageMap, _ := got["usage"].(map[string]any)
	if usageMap["input_tokens"] != float64(8) || usageMap["output_tokens"] != float64(4) {
		t.Errorf("response usage = %v, want input_tokens=8 output_tokens=4", usageMap)
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 8 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 8", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 4 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 4", tokOut, ok)
	}
}

// TestHandleMessages_Auth table-drives the three credential cases the
// brief calls out explicitly: x-api-key authenticates, Authorization:
// Bearer authenticates, and a bad key gets a 401 in ANTHROPIC error
// shape ({"type":"error","error":{"type":...}}), not OpenAI's
// ({"error":{...}}).
func TestHandleMessages_Auth(t *testing.T) {
	const anthResp = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-real","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthResp))
	}))
	defer srv.Close()

	cases := []struct {
		name       string
		authHeader string
		authValue  string
		wantStatus int
	}{
		{"x-api-key header authenticates", "x-api-key", "sk-alice", http.StatusOK},
		{"Authorization Bearer header authenticates", "Authorization", "Bearer sk-alice", http.StatusOK},
		{"bad key is rejected in Anthropic error shape", "x-api-key", "sk-not-a-real-key", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newMessagesTestGateway(t, srv.URL, "")
			body := map[string]any{"model": "claude-test", "max_tokens": 10, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			req := newMessagesRequest(t, body, tc.authHeader, tc.authValue)
			rec := httptest.NewRecorder()
			gw.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body=%s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus != http.StatusUnauthorized {
				return
			}

			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if got["type"] != "error" {
				t.Errorf(`response["type"] = %v, want "error" (Anthropic's envelope shape, not OpenAI's flat one)`, got["type"])
			}
			errObj, _ := got["error"].(map[string]any)
			if errObj["type"] != "authentication_error" {
				t.Errorf("error.type = %v, want authentication_error", errObj["type"])
			}
			if _, hasTopLevelCode := got["code"]; hasTopLevelCode {
				t.Error(`response carries a top-level "code" field — that is OpenAI's writeOAIError shape, not Anthropic's`)
			}
		})
	}
}

// TestHandleMessages_StreamingRejected proves "stream": true is rejected
// cleanly with a typed, Anthropic-shaped error before any upstream call
// is made — never silently ignored, never answered non-streamed.
func TestHandleMessages_StreamingRejected(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	gw := newMessagesTestGateway(t, srv.URL, "")
	body := map[string]any{"model": "claude-test", "max_tokens": 10, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newMessagesRequest(t, body, "x-api-key", "sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501, body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Error("upstream was called; streaming must be rejected before any upstream request")
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got["type"] != "error" {
		t.Errorf(`response["type"] = %v, want "error"`, got["type"])
	}
	errObj, _ := got["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "stream") {
		t.Errorf("error message = %q, want it to mention streaming is not supported", msg)
	}
}

// TestHandleMessages_RateLimited_Returns429 proves this route enforces
// the caller's group/user requestsPerMinute limit exactly like
// routes_unified.go's runUnified: a second request past the limit is
// refused before any upstream call, carries a Retry-After header, and
// only the first, successful request's usage is accounted.
func TestHandleMessages_RateLimited_Returns429(t *testing.T) {
	upstreamCalls := 0
	const anthResp = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-real","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthResp))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"anthropic": {Type: providerTypeAnthropic, BaseURL: srv.URL, APIKey: "sk-ant-up", Models: []string{"claude-test"}}} // #nosec G101 -- test fixture literal, not a real credential
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{RequestsPerMinute: 1}}}}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	body := map[string]any{"model": "claude-test", "max_tokens": 10, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	req1 := newMessagesRequest(t, body, "x-api-key", "sk-alice")
	rec1 := httptest.NewRecorder()
	gw.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200, body=%s", rec1.Code, rec1.Body.String())
	}

	req2 := newMessagesRequest(t, body, "x-api-key", "sk-alice")
	rec2 := httptest.NewRecorder()
	gw.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429, body=%s", rec2.Code, rec2.Body.String())
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("429 response missing Retry-After header")
	}
	if upstreamCalls != 1 {
		t.Errorf("upstream called %d times, want 1 (second request refused before any upstream call)", upstreamCalls)
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 1 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 1 (only the first, successful request accounted)", tokIn, ok)
	}
}
