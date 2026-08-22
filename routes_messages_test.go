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
// provider's own Messages endpoint UNTRANSLATED (brief: "pass through, no
// translation of the message body") and its response copied back to the
// client with every field preserved except "model", which is rewritten to
// the client's own requested alias (item 4 fix, 2026-08-22 review: a
// routing rewrite, not message-body translation — every OTHER field,
// including ones this translator does not itself know about, survives
// unchanged), while still authenticating upstream, forwarding the
// anthropic-beta header (item 5), billing prompt-cache tokens (item 6),
// and accounting usage exactly like every other route.
func TestHandleMessages_AnthropicProvider_Passthrough(t *testing.T) {
	const anthResp = `{"id":"msg_01ABC","type":"message","role":"assistant","content":[{"type":"text","text":"hi there"}],"model":"claude-real","stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":6,"cache_creation_input_tokens":100,"cache_read_input_tokens":50}}`

	var gotBody map[string]any
	var gotAPIKey, gotVersion, gotPath, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotBeta = r.Header.Get("anthropic-beta")
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
	req.Header.Set("anthropic-beta", "token-efficient-tools-2025-02-19")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode client response: %v", err)
	}
	if got["model"] != "claude-test" {
		t.Errorf(`response["model"] = %v, want "claude-test" (the client's requested alias echoed back, item 4 fix — not the upstream's real model id)`, got["model"])
	}
	// Every other field must survive the passthrough unchanged, including
	// ones this gateway does not itself interpret (stop_sequence).
	if got["id"] != "msg_01ABC" || got["role"] != "assistant" || got["stop_reason"] != "end_turn" {
		t.Errorf("response id/role/stop_reason = %v/%v/%v, want the upstream's own values preserved", got["id"], got["role"], got["stop_reason"])
	}
	if _, hasStopSeq := got["stop_sequence"]; !hasStopSeq {
		t.Error(`response missing "stop_sequence" — a field this gateway never reads must still survive the passthrough`)
	}
	content, _ := got["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("response content = %v, want the upstream's one text block preserved", got["content"])
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
	if gotBeta != "token-efficient-tools-2025-02-19" {
		t.Errorf("upstream anthropic-beta = %q, want the client's header forwarded (item 5 fix)", gotBeta)
	}
	if gotBody["model"] != "claude-test" {
		t.Errorf("upstream model = %v, want claude-test (resolved upstream id)", gotBody["model"])
	}
	if _, hasAlias := gotBody[gatewayAliasKey]; hasAlias {
		t.Error("upstream request carries gatewayAliasKey; must never reach an upstream provider")
	}

	// 12 input + 100 cache-creation + 50 cache-read (item 6 fix): all
	// three fold into the billed prompt count, not just fresh input.
	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 162 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 162 (12 input + 100 cache-creation + 50 cache-read)", tokIn, ok)
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

// TestHandleMessages_StreamingRejected table-drives every "stream" shape
// this route must reject cleanly, with a typed, Anthropic-shaped error,
// before any upstream call is made or any rate-limit budget is spent —
// never silently ignored, never answered non-streamed. Covers a real
// boolean true and, per item 9's fix (2026-08-22 review), a non-bool
// truthy value ("stream":"true", a string, and "stream":1, a number) that
// a bare req["stream"].(bool) assertion would previously have silently
// treated as false and forwarded upstream.
func TestHandleMessages_StreamingRejected(t *testing.T) {
	cases := []struct {
		stream any
		name   string
	}{
		{name: "boolean true", stream: true},
		{name: "string \"true\" (non-bool, item 9)", stream: "true"},
		{name: "number 1 (non-bool, item 9)", stream: float64(1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			// requestsPerMinute: 1 so a second wrongly-counted request
			// would 429 — proving the rejection happens before
			// admitRequest ever runs (item 2 fix): both requests below
			// must succeed at the "not yet counted" stage.
			cfg := CreateConfig()
			cfg.Providers = map[string]*ProviderConfig{"anthropic": {Type: providerTypeAnthropic, BaseURL: srv.URL, APIKey: "sk-ant-up", Models: []string{"claude-test"}}} // #nosec G101 -- test fixture literal, not a real credential
			cfg.Groups = map[string]*GroupConfig{"default": {}}
			cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{RequestsPerMinute: 1}}}} // #nosec G101 -- test fixture literal, not a real credential
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
			h, err := New(context.Background(), next, cfg, "llmgw")
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			gw, ok := h.(*Gateway)
			if !ok {
				t.Fatal("handler is not *Gateway")
			}

			body := map[string]any{"model": "claude-test", "max_tokens": 10, "stream": tc.stream, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

			for attempt := 1; attempt <= 2; attempt++ {
				req := newMessagesRequest(t, body, "x-api-key", "sk-alice")
				rec := httptest.NewRecorder()
				gw.ServeHTTP(rec, req)

				// 400, not 501 (item 2 fix): anthropic-sdk-python's retry
				// classifies purely on status code and retries anything
				// >= 500 — a 501 here turns every streaming call into a
				// three-request SDK retry storm against this route's own
				// rate limit.
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("attempt %d: status = %d, want 400, body=%s", attempt, rec.Code, rec.Body.String())
				}

				var got map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatalf("decode error body: %v", err)
				}
				if got["type"] != "error" {
					t.Errorf(`response["type"] = %v, want "error"`, got["type"])
				}
				errObj, _ := got["error"].(map[string]any)
				if errObj["type"] != "invalid_request_error" {
					t.Errorf("error.type = %v, want invalid_request_error", errObj["type"])
				}
				msg, _ := errObj["message"].(string)
				if !strings.Contains(msg, "stream") {
					t.Errorf("error message = %q, want it to mention streaming is not supported", msg)
				}
			}
			if called {
				t.Error("upstream was called; streaming must be rejected before any upstream request")
			}

			// item 2's actual fix: TWO streaming-rejected attempts against
			// a requestsPerMinute:1 user must BOTH succeed (as 400s, not
			// as a 429 on the second one) — proving admitRequest's
			// checkAndCount never ran for either.
			reqCount, ok := gw.limiter.getCounter("user", "alice", metricReq, windowMin, time.Now())
			if !ok || reqCount != 0 {
				t.Errorf("user req:min counter = %d (ok=%v), want 0 — a streaming-rejected request must never consume rate-limit budget", reqCount, ok)
			}
		})
	}
}

// TestHandleMessages_NullStream_IsNotStreaming is the regression for F7
// (2026-08-23 review): isStreamingRequested's item-9 fix ("anything not
// the literal false is streaming") had the side effect of also
// rejecting "stream": null, since a bare v != false comparison treats a
// JSON null (decoded as a Go nil interface) as "present and not false".
// An SDK that serializes an unset optional field as explicit null must
// still get an ordinary, successful non-streaming response, not a false
// -positive 400.
func TestHandleMessages_NullStream_IsNotStreaming(t *testing.T) {
	const anthResp = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-real","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthResp))
	}))
	defer srv.Close()

	gw := newMessagesTestGateway(t, srv.URL, "")
	body := map[string]any{"model": "claude-test", "max_tokens": 10, "stream": nil, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newMessagesRequest(t, body, "x-api-key", "sk-alice")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !called {
		t.Error("upstream was never called; \"stream\": null must be treated as an ordinary non-streaming request")
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

	// Envelope-shape assertion (item 8 fix, 2026-08-22 review): now that
	// admitRequest threads an envelopeWriter through
	// (routes_unified.go's writeLimitViolationEnvelope), a 429 on this
	// route must answer in Anthropic's shape like every other error this
	// route produces, not OpenAI's flat one.
	var got map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if got["type"] != "error" {
		t.Errorf(`429 response["type"] = %v, want "error" (Anthropic's envelope shape, not OpenAI's flat one)`, got["type"])
	}
	errObj, _ := got["error"].(map[string]any)
	if errObj["type"] != "rate_limit_error" {
		t.Errorf("429 error.type = %v, want rate_limit_error", errObj["type"])
	}
	if _, hasTopLevelCode := got["code"]; hasTopLevelCode {
		t.Error(`429 response carries a top-level "code" field — that is OpenAI's writeOAIError shape, not Anthropic's`)
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 1 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 1 (only the first, successful request accounted)", tokIn, ok)
	}
}

// TestHandleMessages_TranslationFailureAfterBilledUsage_PreservesUsage is
// the regression for item 1 (CRITICAL, 2026-08-22 review): an
// openai-type provider's response can succeed (200, real usage the
// provider already charged for) yet still fail THIS route's own
// translation into Anthropic's shape — here, a tool-call arguments
// string truncated mid-JSON by the provider's own max_tokens cutoff
// (finish_reason "length"). The previous version of callTranslatedMessages
// returned usage{} on that path, discarding tokens the provider had
// already billed — a client who could reliably induce a truncated
// tool-call response got unlimited free tokens against any budget. This
// proves the client gets a clean 502 (translation genuinely failed, no
// valid Anthropic response could be produced) while the usage still
// lands in the limiter's counters, and (item 12) that the log line
// names the real cause — "response translation failed", never "upstream
// connection error", which would misdirect an operator toward a network
// problem that never happened.
func TestHandleMessages_TranslationFailureAfterBilledUsage_PreservesUsage(t *testing.T) {
	// Exact verified repro from the review: usage prompt=5000/
	// completion=900, finish_reason "length", and a tool_calls[].function.
	// arguments string truncated mid-object.
	const oaiResp = `{"id":"chatcmpl-trunc","object":"chat.completion","model":"gpt-real","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\": \"Lond"}}]},"finish_reason":"length"}],"usage":{"prompt_tokens":5000,"completion_tokens":900}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(oaiResp))
	}))
	defer srv.Close()

	gw := newMessagesTestGateway(t, "", srv.URL)

	body := map[string]any{
		"model":      "gpt-test",
		"max_tokens": 100,
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	}
	req := newMessagesRequest(t, body, "x-api-key", "sk-alice")
	var rec *httptest.ResponseRecorder
	logs := captureStderr(t, func() {
		rec = httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
	})

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got["type"] != "error" {
		t.Errorf(`response["type"] = %v, want "error" (Anthropic error shape)`, got["type"])
	}

	// item 1: usage the provider already billed must still land in the
	// counters, even though the client got an error.
	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 5000 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 5000 (the upstream's already-billed usage, not discarded on translation failure)", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 900 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 900", tokOut, ok)
	}

	// item 12: the log must name this a translation failure, never an
	// upstream connection error — the connection and the upstream call
	// both succeeded fine.
	if !strings.Contains(logs, "response translation failed") {
		t.Errorf("log output = %q, want it to contain \"response translation failed\"", logs)
	}
	if strings.Contains(logs, "upstream connection error") {
		t.Errorf("log output = %q, want it to NOT be classified as an upstream connection error", logs)
	}
}

// TestHandleMessages_AnthropicProvider_Passthrough_PreservesExactBytes is
// the regression for F1 (BLOCKING, 2026-08-23 review): the previous
// alias-echo fix decoded the upstream's 200 into map[string]any and
// re-marshaled it, which silently corrupted data on a route documented
// as passthrough — a JSON integer beyond float64's exact-integer range
// (verified repro: 12345678901234567890 became 12345678901234567000),
// a trailing ".0" on a float (1.0 became 1), and HTML-escaped "<" and
// "&". A tool_use block's "input" is arbitrary model-generated JSON, so
// a long numeric id inside tool arguments was silently corrupted on
// every anthropic-type request through this route. This proves the
// fixed version (rewriteModelField, routes_messages.go) reproduces the
// upstream body byte-for-byte apart from the "model" field alone: same
// field order, same big-integer digits, same "1.0", same literal "<".
func TestHandleMessages_AnthropicProvider_Passthrough_PreservesExactBytes(t *testing.T) {
	const anthResp = `{"id":"msg_01ABC","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"calc","input":{"big_id":12345678901234567890,"score":1.0,"note":"1 < 2 & 3"}}],"model":"claude-real","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`
	const wantBody = `{"id":"msg_01ABC","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"calc","input":{"big_id":12345678901234567890,"score":1.0,"note":"1 < 2 & 3"}}],"model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body = %s\nwant  = %s\n(must be byte-for-byte identical apart from the \"model\" field: no float64 rounding, no 1.0->1, no HTML-escaping, no key reordering)", got, wantBody)
	}
}

// TestHandleMessages_AnthropicProvider_Passthrough_HeaderAllowlist is the
// regression for F3 (BLOCKING, 2026-08-23 review): negative assertions
// that a blanket header-forward, or reordering injectAuth ahead of the
// allowlist copy while adding x-api-key to it, would both defeat.
// Authorization, x-api-key (the CLIENT's own value), and Cookie must
// never reach the anthropic-type upstream, and the upstream must see
// exactly ONE x-api-key value: the gateway's own configured key
// ("sk-ant-up"), never the client's own ("sk-alice"). The exactly-ONE
// check matters on its own: injectAuth's Set() call replaces any
// existing x-api-key value, so a mutant that adds x-api-key to the
// allowlist and reorders injectAuth BEFORE that copy would still leave
// Get("x-api-key") returning "sk-ant-up" (Set ran first, Add appended
// after) while a second, leaked "sk-alice" value silently rode along —
// invisible to a bare .Get() check, which only ever returns the first
// value.
func TestHandleMessages_AnthropicProvider_Passthrough_HeaderAllowlist(t *testing.T) {
	const anthResp = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-real","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthResp))
	}))
	defer srv.Close()

	gw := newMessagesTestGateway(t, srv.URL, "")

	body := map[string]any{"model": "claude-test", "max_tokens": 10, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	// Authenticate via Authorization: Bearer (presentedKey, auth.go,
	// gives it priority over x-api-key when both are present) and
	// ADDITIONALLY send an x-api-key header of the client's own — unused
	// for auth here, but still present on the request and must still
	// never reach the upstream — plus a Cookie header. None of the three
	// has any business reaching the upstream.
	req := newMessagesRequest(t, body, "Authorization", "Bearer sk-alice")
	req.Header.Set("x-api-key", "some-other-client-value") // #nosec G101 -- test fixture literal, not a real credential
	req.Header.Set("Cookie", "session=client-side-secret")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	if got := gotHeaders.Get("Authorization"); got != "" {
		t.Errorf("upstream Authorization = %q, want empty (never forwarded)", got)
	}
	if got := gotHeaders.Get("Cookie"); got != "" {
		t.Errorf("upstream Cookie = %q, want empty (never forwarded)", got)
	}
	apiKeyValues := gotHeaders.Values("x-api-key")
	if len(apiKeyValues) != 1 {
		t.Fatalf("upstream x-api-key values = %v, want exactly 1 (the client's own key must never ride along as a second value)", apiKeyValues)
	}
	if apiKeyValues[0] != "sk-ant-up" { // #nosec G101 -- test fixture literal, not a real credential
		t.Errorf("upstream x-api-key = %q, want the gateway's own configured key sk-ant-up, never the client's sk-alice", apiKeyValues[0])
	}
}
