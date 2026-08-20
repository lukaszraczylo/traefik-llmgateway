package traefikllmgateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func (alwaysErrStore) getMulti([]string) ([]int64, error) {
	return nil, errStoreDownStub
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

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 10 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 10", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 5 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 5", tokOut, ok)
	}
	reqCount, ok := gw.limiter.getCounter("user", "alice", metricReq, windowMin, time.Now())
	if !ok || reqCount != 1 {
		t.Errorf("user request/min counter = %d (ok=%v), want 1", reqCount, ok)
	}

	// withTotalScope (routes_unified.go) must have appended the synthetic
	// total scope to runUnified's own checkAndCount/account calls: its
	// req/tokin/tokout counters mirror alice's own (v0.2 data-layer task).
	totalReq, ok := gw.limiter.getCounter(totalScopeKind, totalScopeID, metricReq, windowMin, time.Now())
	if !ok || totalReq != 1 {
		t.Errorf("total request/min counter = %d (ok=%v), want 1", totalReq, ok)
	}
	totalTokIn, ok := gw.limiter.getCounter(totalScopeKind, totalScopeID, metricTokIn, windowDay, time.Now())
	if !ok || totalTokIn != 10 {
		t.Errorf("total tokin/day counter = %d (ok=%v), want 10", totalTokIn, ok)
	}
	totalTokOut, ok := gw.limiter.getCounter(totalScopeKind, totalScopeID, metricTokOut, windowDay, time.Now())
	if !ok || totalTokOut != 5 {
		t.Errorf("total tokout/day counter = %d (ok=%v), want 5", totalTokOut, ok)
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

// TestHandleChat_OpenAI_AliasKeyStrippedFromUpstreamBody asserts the
// gatewayAliasKey convention field never reaches a real openai-type
// provider on POST /v1/chat/completions: unlike anthropic/gemini, this
// adapter marshals req verbatim as the upstream wire body, so a leftover
// alias entry would arrive as an unrecognized request field. This test
// fails if provider_openai.go's chatCompletion ever drops its
// delete(req, gatewayAliasKey) call.
func TestHandleChat_OpenAI_AliasKeyStrippedFromUpstreamBody(t *testing.T) {
	const respBody = `{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
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
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const requestedID = "openai/gpt-test"
	body := map[string]any{"model": requestedID, "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	sentUpstream := decodeJSONBody(t, gotBody)
	if _, present := sentUpstream[gatewayAliasKey]; present {
		t.Errorf("gatewayAliasKey %q leaked into the openai chat upstream request body: %v", gatewayAliasKey, sentUpstream)
	}
	if sentUpstream["model"] != "gpt-test" {
		t.Errorf("upstream request model = %v, want bare upstream id %q", sentUpstream["model"], "gpt-test")
	}
}

// TestHandleEmbeddings_OpenAI_AliasKeyStrippedFromUpstreamBody mirrors
// TestHandleChat_OpenAI_AliasKeyStrippedFromUpstreamBody for POST
// /v1/embeddings: this test fails if provider_openai.go's embeddings ever
// drops its delete(req, gatewayAliasKey) call.
func TestHandleEmbeddings_OpenAI_AliasKeyStrippedFromUpstreamBody(t *testing.T) {
	const respBody = `{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
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

	const requestedID = "openai/text-embedding-3"
	body := map[string]any{"model": requestedID, "input": "hello"}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/embeddings", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	sentUpstream := decodeJSONBody(t, gotBody)
	if _, present := sentUpstream[gatewayAliasKey]; present {
		t.Errorf("gatewayAliasKey %q leaked into the openai embeddings upstream request body: %v", gatewayAliasKey, sentUpstream)
	}
	if sentUpstream["model"] != "text-embedding-3" {
		t.Errorf("upstream request model = %v, want bare upstream id %q", sentUpstream["model"], "text-embedding-3")
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

// --- model aliases end-to-end (spec §5, v0.2) ---

// TestHandleChat_ModelAlias_AnthropicTarget_EchoesAliasInResponse mirrors
// TestHandleChat_AnthropicProviderPrefixedModel_EchoesAliasInResponse
// above, but through an operator-defined modelAliases entry rather than a
// bare provider-prefixed id: the upstream call uses the target's own
// upstream model id, and the translated response's "model" field echoes
// the client's exact alias string back (ruling a, ALIAS ECHO — aliases
// inherit the existing __alias machinery for free, per registry.go's
// resolve doc comment).
func TestHandleChat_ModelAlias_AnthropicTarget_EchoesAliasInResponse(t *testing.T) {
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
	cfg.ModelAliases = map[string]string{"aliased/coding": "anthropic/claude-x"}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const requestedID = "aliased/coding"
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
		t.Errorf("upstream request model = %v, want the alias target's bare upstream id %q", sentUpstream["model"], "claude-x")
	}
	if _, present := sentUpstream[gatewayAliasKey]; present {
		t.Errorf("gatewayAliasKey %q leaked into the upstream request body: %v", gatewayAliasKey, sentUpstream)
	}
}

// TestHandleChat_ModelAlias_OpenAITarget_UpstreamEchoNotAlias documents the
// v0.1 passthrough asymmetry (routes_unified.go's gatewayAliasKey doc
// comment) as it applies to aliases: an openai-type target's response is
// forwarded verbatim, so its own "model" field carries whatever the mock
// upstream itself returned — the target's bare upstream id, NOT the
// alias. The gateway makes no attempt, and is not expected, to rewrite an
// openai-type body in flight.
func TestHandleChat_ModelAlias_OpenAITarget_UpstreamEchoNotAlias(t *testing.T) {
	const respBody = `{"id":"c1","model":"gpt-test","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.ModelAliases = map[string]string{"aliased/fast": "openai/gpt-test"}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "aliased/fast", "messages": []any{}}
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
	if out["model"] != "gpt-test" {
		t.Errorf("response model = %v, want the openai-type passthrough's own upstream id %q, not the alias", out["model"], "gpt-test")
	}

	sentUpstream := decodeJSONBody(t, gotBody)
	if sentUpstream["model"] != "gpt-test" {
		t.Errorf("upstream request model = %v, want bare upstream id %q", sentUpstream["model"], "gpt-test")
	}
}

// TestHandleEmbeddings_ModelAlias_ResolvesToTargetUpstreamModel proves
// aliasing applies to /v1/embeddings exactly like /v1/chat/completions —
// the upstream call uses the target's bare upstream model id.
func TestHandleEmbeddings_ModelAlias_ResolvesToTargetUpstreamModel(t *testing.T) {
	const respBody = `{"object":"list","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"text-embedding-3"}},
	}
	cfg.ModelAliases = map[string]string{"aliased/embed": "openai/text-embedding-3"}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "aliased/embed", "input": "hello"}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/embeddings", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	sentUpstream := decodeJSONBody(t, gotBody)
	if sentUpstream["model"] != "text-embedding-3" {
		t.Errorf("upstream request model = %v, want the alias target's bare upstream id %q", sentUpstream["model"], "text-embedding-3")
	}
}

// TestHandleChat_ModelAlias_AuthorizedViaAliasNameAlone_EndToEnd proves
// spec §5's authorization rule end-to-end: a group whose models glob
// matches only the ALIAS name (not the target, in either form) can still
// use it through the real ServeHTTP pipeline, not just modelRegistry.resolve
// in isolation.
func TestHandleChat_ModelAlias_AuthorizedViaAliasNameAlone_EndToEnd(t *testing.T) {
	const respBody = `{"id":"c1","model":"gpt-test","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
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
	cfg.ModelAliases = map[string]string{"aliased/coding": "openai/gpt-test"}
	// Matches only the alias name, not "openai/gpt-test" nor "gpt-test".
	cfg.Groups = map[string]*GroupConfig{"narrow": {Models: []string{"aliased/coding"}}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "narrow", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "aliased/coding", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (authorized via the alias name alone), body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleChat_ModelAlias_UnresolvedTarget_Returns404NamingAliasAndTarget
// covers spec §5's lazy-resolution 404 end-to-end: an alias whose target
// names no currently-known model returns 404 with a message naming both
// the alias and the missing target — not the generic "unknown model" text
// a plain unresolved bare/prefixed id gets.
func TestHandleChat_ModelAlias_UnresolvedTarget_Returns404NamingAliasAndTarget(t *testing.T) {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.ModelAliases = map[string]string{"aliased/future": "openai/not-yet-discovered"}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "aliased/future", "messages": []any{}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(out.Error.Message, "aliased/future") {
		t.Errorf("error message = %q, want it to name the alias %q", out.Error.Message, "aliased/future")
	}
	if !strings.Contains(out.Error.Message, "openai/not-yet-discovered") {
		t.Errorf("error message = %q, want it to name the target %q", out.Error.Message, "openai/not-yet-discovered")
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

// TestHandleChat_MidStreamDropAfterUsageChunk_AccountsPartialUsage is the
// regression test for the critical accounting gap: an adapter that
// captures a usage frame before the connection drops must still have that
// usage billed, even though the request as a whole fails. The fake
// upstream sends one content chunk, then the terminal usage-only chunk
// (openaiAdapter.forwardStream always captures this into its returned
// usage, whether or not the client asked to see it), then hijacks and
// closes the connection without a clean chunked terminator — forcing
// readSSE to return a genuine error after the usage frame was already
// parsed. Without ruling (the CRITICAL fix), that captured usage would be
// silently discarded instead of reaching the limiter, letting a client
// dodge every token/cost budget by aborting right after the usage chunk
// arrives.
func TestHandleChat_MidStreamDropAfterUsageChunk_AccountsPartialUsage(t *testing.T) {
	const contentChunk = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"
	const usageChunk = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("fake upstream ResponseWriter does not support Flush")
		}
		_, _ = w.Write([]byte(contentChunk))
		fl.Flush()
		_, _ = w.Write([]byte(usageChunk))
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
	if !strings.Contains(logBuf.String(), "llmgw[llmgw] ERROR") {
		t.Errorf("want the mid-stream error logged, got %q", logBuf.String())
	}

	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 7 {
		t.Errorf("user tokin/day counter = %d (ok=%v), want 7 (the usage chunk captured before the drop)", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 3 {
		t.Errorf("user tokout/day counter = %d (ok=%v), want 3 (the usage chunk captured before the drop)", tokOut, ok)
	}
}

// --- response cache: end-to-end through Gateway.ServeHTTP (task 2) ---

// behavioralRedisServer is a minimal in-memory RESP2 server implementing
// just enough of the protocol (SELECT/AUTH, GET, SET ... EX, INCRBY,
// EXPIRE) to drive a real Gateway end to end. Unlike resp_test.go's
// respStep-scripted fake server — exact command count and order, used for
// resp.go's own unit tests — this one behaves like a tiny real store: the
// limiter's counters and the response cache share one respClient
// (spec §2), so a scripted sequence would have to predict every INCRBY/
// EXPIRE/GET/SET this test's whole request pipeline issues, in order.
// Behaving like real Redis instead of asserting on the wire trace is more
// robust for a multi-request end-to-end test, and no less faithful: the
// wire-level framing (encodeCommand, RESP replies) is exactly what
// resp_test.go's scripted tests already pin.
type behavioralRedisServer struct {
	data map[string]string
	// lastSetEX is the EX seconds argument from the most recent SET...EX
	// command this server has handled — item B's per-group cache TTL
	// override needs a way to observe the actual wire value a real
	// Gateway.ServeHTTP request produced, not just the reply data's
	// content. Empty until the first such SET arrives.
	lastSetEX string
	mu        sync.Mutex
}

// newBehavioralRedisServer starts the server on an OS-assigned port,
// closed automatically at test cleanup (via newFakeListener), and returns
// its listener.
func newBehavioralRedisServer(t *testing.T) net.Listener {
	t.Helper()
	ln, _ := newBehavioralRedisServerAndHandle(t)
	return ln
}

// newBehavioralRedisServerAndHandle is newBehavioralRedisServer, additionally
// returning the *behavioralRedisServer itself so a caller can inspect state
// the wire protocol alone does not expose — namely lastSetEX (see setEX
// below), used by TestHandleChat_CachePerGroupTTL_EndToEnd_DifferentEXPerGroup
// to prove effectiveTTL's per-group override (cache.go) actually reaches
// the SET...EX command a real Gateway.ServeHTTP request issues.
func newBehavioralRedisServerAndHandle(t *testing.T) (net.Listener, *behavioralRedisServer) {
	t.Helper()
	ln := newFakeListener(t)
	srv := &behavioralRedisServer{data: make(map[string]string)}
	go srv.acceptLoop(ln)
	return ln, srv
}

// setEX returns the EX seconds argument from the most recent SET...EX
// command this server has handled, and whether any such SET has arrived
// yet.
func (s *behavioralRedisServer) setEX() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSetEX, s.lastSetEX != ""
}

func (s *behavioralRedisServer) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed by t.Cleanup; test is finishing
		}
		go s.serve(conn)
	}
}

func (s *behavioralRedisServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	for {
		args, err := readRESPCommand(r)
		if err != nil {
			return
		}
		if _, err := conn.Write(s.handle(args)); err != nil {
			return
		}
	}
}

// handle dispatches one command to its RESP2 reply. SELECT and AUTH
// always succeed (this fake never enforces auth or multiple databases);
// EXPIRE always succeeds without tracking any real expiry — every test
// using this server runs well within any TTL it configures.
func (s *behavioralRedisServer) handle(args []string) []byte {
	if len(args) == 0 {
		return []byte("-ERR empty command\r\n")
	}
	switch strings.ToUpper(args[0]) {
	case "SELECT", "AUTH", "EXPIRE":
		return []byte("+OK\r\n")
	case "GET":
		s.mu.Lock()
		v, ok := s.data[args[1]]
		s.mu.Unlock()
		if !ok {
			return []byte("$-1\r\n")
		}
		return []byte(fmt.Sprintf("$%d\r\n%s\r\n", len(v), v))
	case "SET":
		s.mu.Lock()
		s.data[args[1]] = args[2]
		if len(args) >= 5 && strings.EqualFold(args[3], "EX") {
			s.lastSetEX = args[4]
		}
		s.mu.Unlock()
		return []byte("+OK\r\n")
	case "INCRBY":
		n, _ := strconv.ParseInt(args[2], 10, 64)
		s.mu.Lock()
		cur, _ := strconv.ParseInt(s.data[args[1]], 10, 64)
		cur += n
		s.data[args[1]] = strconv.FormatInt(cur, 10)
		s.mu.Unlock()
		return []byte(fmt.Sprintf(":%d\r\n", cur))
	default:
		return []byte("-ERR unknown command\r\n")
	}
}

// newCacheTestGateway builds a *Gateway with cfg.Redis pointed at a
// behavioralRedisServer and cfg.Cache enabled, an openai provider pointed
// at srv, group "default" (its Cache override left to groupCache, nil
// unless the caller sets it), and one user ("alice") with an (empty,
// unlimited) LimitsConfig — required so buildLimitScopes actually
// produces a scope for the limiter to count against; without one,
// checkAndCount/account never touch the store at all, and this test's
// counter assertions would trivially "pass" against uncounted zeros.
func newCacheTestGateway(t *testing.T, srv *httptest.Server, groupCache *bool, maxBodyBytes int) *Gateway {
	t.Helper()
	redisLn := newBehavioralRedisServer(t)

	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {Cache: groupCache}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	cfg.Redis = &RedisConfig{Address: redisLn.Addr().String()}
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m", MaxBodyBytes: maxBodyBytes}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
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

// TestHandleChat_CacheHitMiss_EndToEnd_CountersAndHeaders drives two
// identical chat-completion requests through a real Gateway with caching
// enabled: the first is a miss (upstream called, response stored,
// X-Llmgw-Cache: miss), the second is a hit served straight from the
// cache (upstream NOT called again, X-Llmgw-Cache: hit) with the exact
// same body. Counters prove the operator's "count requests, free tokens"
// decision (spec §2): both requests increment req:day by 1, but only the
// first (the real upstream call) moves tok:day — the hit adds zero.
func TestHandleChat_CacheHitMiss_EndToEnd_CountersAndHeaders(t *testing.T) {
	const respBody = `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	var upstreamCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	gw := newCacheTestGateway(t, srv, nil, defaultCacheMaxBodyBytes)
	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	// First request: miss.
	req1 := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec1 := httptest.NewRecorder()
	gw.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK || rec1.Body.String() != respBody {
		t.Fatalf("first request: status=%d body=%q, want 200 and the upstream body verbatim", rec1.Code, rec1.Body.String())
	}
	if got := rec1.Header().Get("X-Llmgw-Cache"); got != "miss" {
		t.Errorf("first request X-Llmgw-Cache = %q, want %q", got, "miss")
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after first request = %d, want 1", upstreamCalls)
	}

	reqCount, ok := gw.limiter.getCounter("user", "alice", metricReq, windowDay, time.Now())
	if !ok || reqCount != 1 {
		t.Errorf("req:day after first request = %d (ok=%v), want 1", reqCount, ok)
	}
	tokIn, ok := gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 10 {
		t.Errorf("tokin:day after first request = %d (ok=%v), want 10", tokIn, ok)
	}
	tokOut, ok := gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	if !ok || tokOut != 5 {
		t.Errorf("tokout:day after first request = %d (ok=%v), want 5", tokOut, ok)
	}

	// Second, identical request: hit.
	req2 := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec2 := httptest.NewRecorder()
	gw.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK || rec2.Body.String() != respBody {
		t.Fatalf("second request: status=%d body=%q, want 200 and the identical cached body", rec2.Code, rec2.Body.String())
	}
	if got := rec2.Header().Get("X-Llmgw-Cache"); got != "hit" {
		t.Errorf("second request X-Llmgw-Cache = %q, want %q", got, "hit")
	}
	if upstreamCalls != 1 {
		t.Errorf("upstreamCalls after second (cache-hit) request = %d, want still 1 (upstream must not be called again)", upstreamCalls)
	}

	reqCount, ok = gw.limiter.getCounter("user", "alice", metricReq, windowDay, time.Now())
	if !ok || reqCount != 2 {
		t.Errorf("req:day after the cache hit = %d (ok=%v), want 2 (hit still counts as one more request)", reqCount, ok)
	}
	tokIn, ok = gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	if !ok || tokIn != 10 {
		t.Errorf("tokin:day after the cache hit = %d (ok=%v), want still 10 (a hit accounts zero tokens, no estimation)", tokIn, ok)
	}
}

// TestHandleChat_CacheGroupOptOut_NeverServesFromCache is spec §2's
// per-group override: GroupConfig.Cache=false must opt the group out even
// though the global cache is enabled — every request goes to the
// upstream, and no X-Llmgw-Cache header is ever set.
func TestHandleChat_CacheGroupOptOut_NeverServesFromCache(t *testing.T) {
	const respBody = `{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	var upstreamCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	optOut := false
	gw := newCacheTestGateway(t, srv, &optOut, defaultCacheMaxBodyBytes)
	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	for i := 0; i < 2; i++ {
		req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, rec.Code)
		}
		if got := rec.Header().Get("X-Llmgw-Cache"); got != "" {
			t.Errorf("request %d: X-Llmgw-Cache = %q, want unset (group opted out)", i, got)
		}
	}
	if upstreamCalls != 2 {
		t.Errorf("upstreamCalls = %d, want 2 (an opted-out group must never be served from cache)", upstreamCalls)
	}
}

// TestHandleChat_CacheOversizeBody_SkipsStore_StillServesEveryRequest
// proves an upstream body larger than cache.maxBodyBytes is served
// normally but never cached: a second identical request still goes to
// the upstream, since store() silently skipped the first one.
func TestHandleChat_CacheOversizeBody_SkipsStore_StillServesEveryRequest(t *testing.T) {
	respBody := `{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"` +
		strings.Repeat("x", 200) + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	var upstreamCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	gw := newCacheTestGateway(t, srv, nil, 32) // maxBodyBytes far smaller than respBody
	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	for i := 0; i < 2; i++ {
		req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != respBody {
			t.Fatalf("request %d: status=%d body=%q, want 200 and the full upstream body regardless of the cache's maxBodyBytes", i, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("X-Llmgw-Cache"); got != "miss" {
			t.Errorf("request %d: X-Llmgw-Cache = %q, want %q (oversize is a store-time skip, not a lookup-time change)", i, got, "miss")
		}
	}
	if upstreamCalls != 2 {
		t.Errorf("upstreamCalls = %d, want 2 (an oversize response must never be served from cache on a later request)", upstreamCalls)
	}
}

// TestHandleChat_CacheRedisDown_MissesAndServesNormallyWithLog proves
// spec §2's "redis errors during cache ops → treat as miss ... never fail
// the request because the cache is down": cfg.Redis points at an address
// nothing listens on (a respClient dials lazily, so construction still
// succeeds), so every lookup/store call fails, yet the request is served
// normally every time, with a "miss" header and a rate-limited error log
// line. The rate-limit behavior itself (once per storeErrorLogEvery) is
// already pinned precisely at the responseCache level
// (TestResponseCache_Lookup_RedisDown_MissWithRateLimitedLog,
// cache_test.go); this test only proves the failure never surfaces to
// the client end to end.
func TestHandleChat_CacheRedisDown_MissesAndServesNormallyWithLog(t *testing.T) {
	deadLn := newFakeListener(t)
	deadAddr := deadLn.Addr().String()
	if err := deadLn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	const respBody = `{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
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
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	cfg.Redis = &RedisConfig{Address: deadAddr}
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m"}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	origStderr := os.Stderr
	pr, pw, _ := os.Pipe()
	os.Stderr = pw

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	_ = pw.Close()
	os.Stderr = origStderr
	var logBuf bytes.Buffer
	if _, err := io.Copy(&logBuf, pr); err != nil {
		t.Fatalf("io.Copy: %v", err)
	}

	if rec.Code != http.StatusOK || rec.Body.String() != respBody {
		t.Fatalf("status=%d body=%q, want 200 and the upstream body served despite redis being down", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Llmgw-Cache"); got != "miss" {
		t.Errorf("X-Llmgw-Cache = %q, want %q (redis down is treated as a miss)", got, "miss")
	}
	if !strings.Contains(logBuf.String(), "response cache: redis error") {
		t.Errorf("want a logged cache redis error, got %q", logBuf.String())
	}
}

// TestHandleChat_AnthropicTranslatedResponse_CachedAboveAdapter proves
// the cache sits above the adapter layer: an anthropic-type provider's
// translated, OpenAI-shaped response is what gets cached, and a second
// identical request is served that exact translated body from the cache
// — with the anthropic upstream called only once.
func TestHandleChat_AnthropicTranslatedResponse_CachedAboveAdapter(t *testing.T) {
	const anthResp = `{"id":"msg_01ABC","type":"message","role":"assistant","content":[{"type":"text","text":"hi there"}],"model":"claude-x","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":4}}`
	var upstreamCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthResp))
	}))
	defer srv.Close()

	redisLn := newBehavioralRedisServer(t)
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"anthropic": {Type: "anthropic", BaseURL: srv.URL, APIKey: "sk-ant", Models: []string{"claude-x"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	cfg.Redis = &RedisConfig{Address: redisLn.Addr().String()}
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m"}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "claude-x", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	req1 := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, body=%s", rec1.Code, rec1.Body.String())
	}
	if got := rec1.Header().Get("X-Llmgw-Cache"); got != "miss" {
		t.Errorf("first request X-Llmgw-Cache = %q, want %q", got, "miss")
	}
	var out1 map[string]any
	if err := json.Unmarshal(rec1.Body.Bytes(), &out1); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if _, ok := out1["choices"]; !ok {
		t.Fatalf("first response is not OpenAI-shaped (no \"choices\"): %s", rec1.Body.String())
	}

	req2 := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request: status = %d, body=%s", rec2.Code, rec2.Body.String())
	}
	if got := rec2.Header().Get("X-Llmgw-Cache"); got != "hit" {
		t.Errorf("second request X-Llmgw-Cache = %q, want %q", got, "hit")
	}
	if rec2.Body.String() != rec1.Body.String() {
		t.Errorf("second (cached) response body = %q, want identical to the first translated response %q", rec2.Body.String(), rec1.Body.String())
	}
	if upstreamCalls != 1 {
		t.Errorf("upstreamCalls = %d, want 1 (the anthropic upstream must not be called again on a cache hit)", upstreamCalls)
	}
}

// TestHandleChat_CacheMixedAliasForm_NeverHits_SameFormHits is the
// review-fix (finding 1, CRITICAL) end-to-end regression: a bare alias
// ("claude-x") and its provider-prefixed form ("anthropic/claude-x")
// resolve to the identical provider+upstreamModel, but a translating
// adapter bakes the client's own requested alias into the cached
// response body's "model" field — so a request using one form must never
// be served from an entry cached under the other form, or the client
// would receive a "model" value it never sent. The same form repeated
// must still hit.
func TestHandleChat_CacheMixedAliasForm_NeverHits_SameFormHits(t *testing.T) {
	const anthResp = `{"id":"msg_01ABC","type":"message","role":"assistant","content":[{"type":"text","text":"hi there"}],"model":"claude-x","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":4}}`
	var upstreamCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthResp))
	}))
	defer srv.Close()

	redisLn := newBehavioralRedisServer(t)
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"anthropic": {Type: "anthropic", BaseURL: srv.URL, APIKey: "sk-ant", Models: []string{"claude-x"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	cfg.Redis = &RedisConfig{Address: redisLn.Addr().String()}
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m"}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bareBody := map[string]any{"model": "claude-x", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	prefixedBody := map[string]any{"model": "anthropic/claude-x", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}

	// A: bare alias form. Miss, cached under the bare-alias key.
	reqA := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", bareBody)
	recA := httptest.NewRecorder()
	h.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusOK || recA.Header().Get("X-Llmgw-Cache") != "miss" {
		t.Fatalf("request A: status=%d cache=%q, want 200/miss", recA.Code, recA.Header().Get("X-Llmgw-Cache"))
	}
	var outA map[string]any
	if err := json.Unmarshal(recA.Body.Bytes(), &outA); err != nil {
		t.Fatalf("decode A: %v", err)
	}
	if outA["model"] != "claude-x" {
		t.Fatalf("request A model = %v, want %q", outA["model"], "claude-x")
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstreamCalls after A = %d, want 1", upstreamCalls)
	}

	// B: same upstream model, PREFIXED alias form. Must be a genuine
	// miss, never served A's cached "claude-x" body.
	reqB := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", prefixedBody)
	recB := httptest.NewRecorder()
	h.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("request B: status = %d, body=%s", recB.Code, recB.Body.String())
	}
	if got := recB.Header().Get("X-Llmgw-Cache"); got != "miss" {
		t.Errorf("request B X-Llmgw-Cache = %q, want %q (a different alias form must never hit A's entry)", got, "miss")
	}
	var outB map[string]any
	if err := json.Unmarshal(recB.Body.Bytes(), &outB); err != nil {
		t.Fatalf("decode B: %v", err)
	}
	if outB["model"] != "anthropic/claude-x" {
		t.Fatalf("request B model = %v, want its own requested alias %q, never A's cached %q", outB["model"], "anthropic/claude-x", "claude-x")
	}
	if upstreamCalls != 2 {
		t.Fatalf("upstreamCalls after B = %d, want 2 (a different alias form is a genuine miss)", upstreamCalls)
	}

	// C: bare form again, matching A exactly. Must hit A's entry.
	reqC := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", bareBody)
	recC := httptest.NewRecorder()
	h.ServeHTTP(recC, reqC)
	if got := recC.Header().Get("X-Llmgw-Cache"); got != "hit" {
		t.Errorf("request C X-Llmgw-Cache = %q, want %q (same alias form as A must hit)", got, "hit")
	}
	if recC.Body.String() != recA.Body.String() {
		t.Errorf("request C body = %q, want identical to A's cached body %q", recC.Body.String(), recA.Body.String())
	}
	if upstreamCalls != 2 {
		t.Errorf("upstreamCalls after C = %d, want still 2 (C is a real hit, no upstream call)", upstreamCalls)
	}
}

// TestHandleChat_CacheableRequest_UpstreamDown_502HasNoCacheHeader is the
// review-fix (finding 4, folded) regression: a cacheable request whose
// upstream is unreachable gets a 502 with NO X-Llmgw-Cache header — the
// pre-set "miss" header (set before call() runs, so headers precede a
// successful body) must be cleared once handleAdapterError decides the
// request actually failed, since nothing was served from or stored to
// the cache.
func TestHandleChat_CacheableRequest_UpstreamDown_502HasNoCacheHeader(t *testing.T) {
	deadLn := newFakeListener(t)
	deadAddr := deadLn.Addr().String()
	if err := deadLn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	redisLn := newBehavioralRedisServer(t)
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: "http://" + deadAddr, APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	cfg.Redis = &RedisConfig{Address: redisLn.Addr().String()}
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m"}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (connection refused), body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Llmgw-Cache"); got != "" {
		t.Errorf("X-Llmgw-Cache = %q, want unset on a 502 (nothing was served from or stored to the cache)", got)
	}
}

// --- per-group cache TTL: end-to-end through a real Gateway (item B) ---

// TestHandleChat_CachePerGroupTTL_EndToEnd_DifferentEXPerGroup is item B's
// e2e proof: two groups share one global cache.ttl ("1m" = EX 60), one
// ("override") overrides it to "30s", the other ("inherit") leaves it
// unset. A cache-miss request from each group's own user must produce a
// SET...EX command carrying that group's own resolved TTL — 30 for
// "override", 60 for "inherit" — proving effectiveTTL's per-group
// resolution (cache.go) actually reaches the wire through the full
// ServeHTTP path, not just the unit level
// (TestResponseCache_Store_EffectiveTTLDiffersPerGroup_SETCarriesGroupTTL,
// cache_test.go). The two requests carry deliberately different bodies —
// cacheKey has no group/user component (routes_unified.go), so identical
// bodies from different groups would collide into one cache entry and the
// second request would be a hit, never issuing a second SET at all.
func TestHandleChat_CachePerGroupTTL_EndToEnd_DifferentEXPerGroup(t *testing.T) {
	const respBody = `{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	redisLn, redisSrv := newBehavioralRedisServerAndHandle(t)
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{
		"override": {CacheTTL: "30s"},
		"inherit":  {},
	}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "bob", Group: "override", APIKey: "sk-bob", Limits: &LimitsConfig{}},
		{Name: "alice", Group: "inherit", APIKey: "sk-alice", Limits: &LimitsConfig{}},
	}}
	cfg.Redis = &RedisConfig{Address: redisLn.Addr().String()}
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m", MaxBodyBytes: defaultCacheMaxBodyBytes}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// "override" group's user (bob): expect EX 30.
	overrideBody := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "override-group"}}}
	reqOverride := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-bob", overrideBody)
	recOverride := httptest.NewRecorder()
	h.ServeHTTP(recOverride, reqOverride)
	if recOverride.Code != http.StatusOK {
		t.Fatalf("override-group request: status = %d, want 200", recOverride.Code)
	}
	if ex, ok := redisSrv.setEX(); !ok || ex != "30" {
		t.Errorf("override-group SET...EX = %q (ok=%v), want %q", ex, ok, "30")
	}

	// "inherit" group's user (alice), a genuinely different body so it
	// misses (and issues its own SET) rather than hitting bob's entry:
	// expect EX 60 (the global 1m).
	inheritBody := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "inherit-group"}}}
	reqInherit := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", inheritBody)
	recInherit := httptest.NewRecorder()
	h.ServeHTTP(recInherit, reqInherit)
	if recInherit.Code != http.StatusOK {
		t.Fatalf("inherit-group request: status = %d, want 200", recInherit.Code)
	}
	if ex, ok := redisSrv.setEX(); !ok || ex != "60" {
		t.Errorf("inherit-group SET...EX = %q (ok=%v), want %q", ex, ok, "60")
	}
}

// --- cacheCaptureWriter: bounded capture (review finding 2) ---

// TestCacheCaptureWriter_BoundsBufferAtMaxBodyBytes_StillWritesFullBodyThrough
// is the review-fix (finding 2, Important) regression: a single Write of
// a body larger than maxBodyBytes must not buffer the whole thing —
// capture stops at the cap — while the real client-visible write is
// never truncated.
func TestCacheCaptureWriter_BoundsBufferAtMaxBodyBytes_StillWritesFullBodyThrough(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusTrackingWriter{ResponseWriter: rec}
	const maxBody = 16
	cw := newCacheCaptureWriter(sw, maxBody)

	fullBody := []byte(strings.Repeat("x", maxBody+50)) // one write, well over the cap
	cw.Header().Set("Content-Type", "application/json")
	cw.WriteHeader(http.StatusOK)
	n, err := cw.Write(fullBody)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(fullBody) {
		t.Errorf("Write returned n=%d, want %d (the real client write must never be truncated)", n, len(fullBody))
	}

	if !cw.oversize {
		t.Error("oversize = false, want true")
	}
	// "buffer <= cap + one write": this implementation holds the capture
	// at exactly the cap, a tighter bound the looser one still accepts.
	if cw.buf.Len() > maxBody {
		t.Errorf("captured buffer = %d bytes, want <= maxBodyBytes (%d)", cw.buf.Len(), maxBody)
	}
	if rec.Body.String() != string(fullBody) {
		t.Errorf("client-visible body = %d bytes, want the full %d-byte body (capture must never truncate the real response)", rec.Body.Len(), len(fullBody))
	}
}

// TestCacheCaptureWriter_MultipleWrites_StopsCapturingOnceCapReached
// covers the multi-write case: capture stops appending once the cap is
// crossed on a later write, not just a single oversize one, while every
// write still reaches the real client in full.
func TestCacheCaptureWriter_MultipleWrites_StopsCapturingOnceCapReached(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusTrackingWriter{ResponseWriter: rec}
	const maxBody = 10
	cw := newCacheCaptureWriter(sw, maxBody)
	cw.WriteHeader(http.StatusOK)

	if _, err := cw.Write([]byte("12345")); err != nil { // 5 bytes, under the cap
		t.Fatalf("Write: %v", err)
	}
	if cw.oversize {
		t.Fatal("oversize = true after a write under the cap")
	}
	if _, err := cw.Write([]byte("67890ABCDE")); err != nil { // 10 more bytes, crosses the 10-byte cap
		t.Fatalf("Write: %v", err)
	}
	if !cw.oversize {
		t.Error("oversize = false, want true once the cap is crossed")
	}
	if cw.buf.Len() != maxBody {
		t.Errorf("captured buffer = %d bytes, want exactly maxBodyBytes (%d)", cw.buf.Len(), maxBody)
	}
	if rec.Body.String() != "1234567890ABCDE" {
		t.Errorf("client-visible body = %q, want the full, unsplit content across both writes", rec.Body.String())
	}
}

// TestCacheCaptureWriter_ImplicitStatus_CapturesContentType is the
// review-fix (finding 4, folded) regression: the implicit-200 Write path
// (no explicit WriteHeader call) must still snapshot Content-Type, not
// just status — mirroring WriteHeader's own capture.
func TestCacheCaptureWriter_ImplicitStatus_CapturesContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusTrackingWriter{ResponseWriter: rec}
	cw := newCacheCaptureWriter(sw, defaultCacheMaxBodyBytes)
	cw.Header().Set("Content-Type", "application/json") // set, but WriteHeader never explicitly called

	if _, err := cw.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if cw.status != http.StatusOK {
		t.Errorf("status = %d, want 200 (implicit)", cw.status)
	}
	if cw.contentType != "application/json" {
		t.Errorf("contentType = %q, want %q (must be captured on the implicit-200 path too)", cw.contentType, "application/json")
	}
}

// TestUnifiedCostMicros covers ruling (d)'s two-id price lookup order:
// canonical ("provider/model") first, falling back to bare — the upstream
// model id alone — only when canonical has no configured price at all, in
// neither overrides nor the built-in table.
func TestUnifiedCostMicros(t *testing.T) {
	u := usage{prompt: 1_000_000, completion: 1_000_000}

	cases := []struct {
		overrides map[string]*ModelPricing
		name      string
		canonical string
		bare      string
		want      int64
	}{
		{
			name:      "canonical override price wins over bare",
			canonical: "openai/gpt-5",
			bare:      "gpt-5",
			overrides: map[string]*ModelPricing{
				"openai/gpt-5": {InputPerM: 1, OutputPerM: 2},
			},
			want: 3_000_000, // (1M prompt * $1/M) + (1M completion * $2/M), in micros
		},
		{
			name:      "canonical unpriced falls back to bare's built-in price",
			canonical: "unknown-provider/gpt-5",
			bare:      "gpt-5",
			overrides: nil,
			// builtinPricing["gpt-5"] = {InputPerM: 1.25, OutputPerM: 10}:
			// (1M prompt * $1.25/M) + (1M completion * $10/M), in micros.
			want: 11_250_000,
		},
		{
			name:      "neither id priced returns zero",
			canonical: "unknown-provider/unknown-model",
			bare:      "unknown-model",
			overrides: nil,
			want:      0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, unifiedCostMicros(c.canonical, c.bare, u, c.overrides))
		})
	}
}
