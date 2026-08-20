package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// newUnifiedRequest builds a POST request against the unified routes,
// JSON-encoding body and attaching apiKey as a Bearer token when non-empty.
func newUnifiedRequest(t *testing.T, method, path, apiKey string, body map[string]any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req
}

// recordingWriter is a minimal http.ResponseWriter/http.Flusher that logs
// the sequence of Write and Flush calls it receives, so a streaming test
// can assert flushes happen incrementally rather than once at the end.
type recordingWriter struct {
	hdr    http.Header
	events []string
	buf    bytes.Buffer
	status int
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{hdr: http.Header{}}
}

func (r *recordingWriter) Header() http.Header { return r.hdr }

func (r *recordingWriter) WriteHeader(status int) { r.status = status }

func (r *recordingWriter) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK // mirrors net/http's implicit-200 default when Write precedes any explicit WriteHeader
	}
	n, _ := r.buf.Write(b)
	r.events = append(r.events, "write")
	return n, nil
}

func (r *recordingWriter) Flush() { r.events = append(r.events, "flush") }

// alwaysErrStore is a counterStore stub whose every operation fails, used
// to drive the limiter's fail-closed (storeDown) path deterministically.
type alwaysErrStore struct{}

var errStoreDownStub = errors.New("stub: store down")

func (alwaysErrStore) incrBy(string, int64, time.Duration) (int64, error) {
	return 0, errStoreDownStub
}

func (alwaysErrStore) get(string) (int64, error) {
	return 0, errStoreDownStub
}

// TestHandleChat_HappyPath_NonStreaming_AccountsUsage drives a full
// chat-completion request through Gateway.ServeHTTP against a fake
// openai-shaped upstream, and asserts the response is forwarded verbatim
// and the reported usage lands in the limiter's own counters.
func TestHandleChat_HappyPath_NonStreaming_AccountsUsage(t *testing.T) {
	const respBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-real","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want verbatim upstream body %q", rec.Body.String(), respBody)
	}

	tok, ok := gw.limiter.getCounter("user", "alice", metricTok, windowDay, time.Now())
	if !ok || tok != 15 {
		t.Errorf("user token/day counter = %d (ok=%v), want 15", tok, ok)
	}
	reqCount, ok := gw.limiter.getCounter("user", "alice", metricReq, windowMin, time.Now())
	if !ok || reqCount != 1 {
		t.Errorf("user request/min counter = %d (ok=%v), want 1", reqCount, ok)
	}
}

// TestHandleChat_Streaming_FlushesIncrementally proves a streaming chat
// completion is forwarded to the client chunk by chunk, flushing after
// each one, rather than buffered until the response completes.
func TestHandleChat_Streaming_FlushesIncrementally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Flush")
		}
		for _, c := range []string{
			`{"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"Hel"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","choices":[{"delta":{"content":"lo"}}]}`,
		} {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "sk-up", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "gpt-test", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rw := newRecordingWriter()
	h.ServeHTTP(rw, req)

	if rw.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", rw.status)
	}
	writes, flushes := 0, 0
	for _, e := range rw.events {
		switch e {
		case "write":
			writes++
		case "flush":
			flushes++
		}
	}
	if writes < 3 {
		t.Fatalf("want >=3 writes (2 content chunks + [DONE]), got %d (events=%v)", writes, rw.events)
	}
	if flushes < writes {
		t.Fatalf("want a flush per write (incremental delivery), got %d writes and %d flushes", writes, flushes)
	}
	if !strings.Contains(rw.buf.String(), "data: [DONE]") {
		t.Errorf("body missing terminal [DONE], got %q", rw.buf.String())
	}
}

// TestHandleChat_NoAPIKey_Returns401 asserts the unified routes require
// authentication before any resolve/limit work happens.
func TestHandleChat_NoAPIKey_Returns401(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "", map[string]any{"model": "gpt-test"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleChat_DeniedModel_Returns403 covers a model that a configured
// provider knows, but the caller's group is not authorized to use.
func TestHandleChat_DeniedModel_Returns403(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {Models: []string{"gpt-allowed-only"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "gpt-test", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleChat_UnknownModel_Returns404 covers a model no configured
// provider knows at all.
func TestHandleChat_UnknownModel_Returns404(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k", Models: []string{"gpt-test"}}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "no-such-model", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleChat_RequestLimitExceeded_Returns429WithRetryAfter drives two
// requests against a user limited to one per minute, and asserts the
// second is refused with a Retry-After header.
func TestHandleChat_RequestLimitExceeded_Returns429WithRetryAfter(t *testing.T) {
	const respBody = `{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{RequestsPerMinute: 1}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "gpt-test", "messages": []any{}}
	req1 := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200, body=%s", rec1.Code, rec1.Body.String())
	}

	req2 := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429, body=%s", rec2.Code, rec2.Body.String())
	}
	if ra := rec2.Header().Get("Retry-After"); ra == "" {
		t.Error("want a Retry-After header on 429, got none")
	}
}

// TestHandleChat_StoreDown_FailClosed_Returns503 wires a counterStore
// stub whose every operation fails, with failOpen=false, and asserts the
// request is refused with 503 rather than silently enforcing limits
// against an unreachable backend.
func TestHandleChat_StoreDown_FailClosed_Returns503(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k", Models: []string{"gpt-test"}}}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{RequestsPerMinute: 100}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw, ok := h.(*Gateway)
	if !ok {
		t.Fatal("handler is not *Gateway")
	}
	gw.limiter = newLimiter(alwaysErrStore{}, false)

	body := map[string]any{"model": "gpt-test", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleEmbeddings_HappyPath mirrors the chat-completion happy path
// against POST /v1/embeddings.
func TestHandleEmbeddings_HappyPath(t *testing.T) {
	const respBody = `{"object":"list","data":[{"embedding":[0.1,0.2]}],"model":"text-embedding-3","usage":{"prompt_tokens":5,"total_tokens":5}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %q, want /v1/embeddings", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"text-embedding-3"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "text-embedding-3", "input": "hello"}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/embeddings", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want verbatim upstream body %q", rec.Body.String(), respBody)
	}
}

// TestHandleChat_AnthropicProviderPrefixedModel_EchoesAliasInResponse
// requests a provider-prefixed model id ("anthropic/claude-x"): the
// upstream call must use the bare upstream id, but the translated
// response's "model" field must echo back the exact id the client
// requested (ruling a, ALIAS ECHO), not the bare upstream form.
func TestHandleChat_AnthropicProviderPrefixedModel_EchoesAliasInResponse(t *testing.T) {
	const anthResp = `{"id":"msg_01ABC","type":"message","role":"assistant","content":[{"type":"text","text":"hi there"}],"model":"claude-x","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":4}}`

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthResp))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"anthropic": {Type: "anthropic", BaseURL: srv.URL, APIKey: "sk-ant", Models: []string{"claude-x"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const requestedID = "anthropic/claude-x"
	body := map[string]any{"model": requestedID, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out["model"] != requestedID {
		t.Errorf("response model = %v, want the client-requested alias %q echoed back", out["model"], requestedID)
	}

	sentUpstream := decodeJSONBody(t, gotBody)
	if sentUpstream["model"] != "claude-x" {
		t.Errorf("upstream request model = %v, want bare upstream id %q", sentUpstream["model"], "claude-x")
	}
	if _, present := sentUpstream[gatewayAliasKey]; present {
		t.Errorf("gatewayAliasKey %q leaked into the upstream request body: %v", gatewayAliasKey, sentUpstream)
	}
}

// TestHandleChat_UpstreamNon2xx_WrapsProviderErrorEnvelope asserts a
// non-2xx upstream response is passed through with its status code, but
// wrapped in the gateway's own envelope with the raw upstream body
// embedded under error.upstream (ruling c).
func TestHandleChat_UpstreamNon2xx_WrapsProviderErrorEnvelope(t *testing.T) {
	const upstreamErrBody = `{"error":{"message":"rate limited upstream","type":"rate_limit_error"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(upstreamErrBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "gpt-test", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body=%s", rec.Code, rec.Body.String())
	}

	var out struct {
		Error struct {
			Upstream map[string]any `json:"upstream"`
			Message  string         `json:"message"`
			Type     string         `json:"type"`
			Code     string         `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out.Error.Type != "upstream_error" {
		t.Errorf("error.type = %q, want upstream_error", out.Error.Type)
	}
	if out.Error.Code != "429" {
		t.Errorf("error.code = %q, want %q", out.Error.Code, "429")
	}
	if out.Error.Upstream == nil {
		t.Fatal("error.upstream is nil, want the parsed upstream body")
	}
	errObj, _ := out.Error.Upstream["error"].(map[string]any)
	if errObj["message"] != "rate limited upstream" {
		t.Errorf("error.upstream.error.message = %v, want %q", errObj["message"], "rate limited upstream")
	}
}

// TestHandleChat_MidStreamUpstreamDrop_NoTrailingEnvelope proves an
// adapter error arriving after the response has already started streaming
// gets logged, not turned into a second, conflicting envelope appended to
// a body the client already started receiving (ruling c).
func TestHandleChat_MidStreamUpstreamDrop_NoTrailingEnvelope(t *testing.T) {
	const firstChunk = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Flush")
		}
		_, _ = w.Write([]byte(firstChunk))
		fl.Flush()

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Hijack")
		}
		conn, _, hjErr := hj.Hijack()
		if hjErr == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	origStderr := os.Stderr
	pr, pw, _ := os.Pipe()
	os.Stderr = pw
	defer func() { os.Stderr = origStderr }()

	body := map[string]any{"model": "gpt-test", "stream": true, "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	_ = pw.Close() // closing the pipe write end to unblock the read; error not actionable in a test
	os.Stderr = origStderr
	var logBuf bytes.Buffer
	if _, err := io.Copy(&logBuf, pr); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers already committed before the drop)", rec.Code)
	}
	if got := rec.Body.String(); got != firstChunk {
		t.Errorf("body = %q, want exactly the one chunk written before the drop (no trailing error envelope)", got)
	}
	if !strings.Contains(logBuf.String(), "llmgw[llmgw] ERROR") {
		t.Errorf("want the mid-stream error logged, got %q", logBuf.String())
	}
}
