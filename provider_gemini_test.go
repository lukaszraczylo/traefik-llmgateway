package traefikllmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewGeminiAdapter_KeylessIsAConstructorError proves an empty resolved
// API key is a constructor error for gemini-type providers, mirroring
// ruling (c) and anthropic's equivalent test.
func TestNewGeminiAdapter_KeylessIsAConstructorError(t *testing.T) {
	if _, err := newGeminiAdapter("p1", "https://generativelanguage.googleapis.com", ""); err == nil {
		t.Fatalf("newGeminiAdapter: want error for empty API key, got nil")
	}
}

// TestGeminiAdapter_InjectAuth proves injectAuth sets the x-goog-api-key
// header unconditionally.
func TestGeminiAdapter_InjectAuth(t *testing.T) {
	a, err := newGeminiAdapter("p1", "https://generativelanguage.googleapis.com", "AIza-test")
	if err != nil {
		t.Fatalf("newGeminiAdapter: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	a.injectAuth(req)

	if got := req.Header.Get("x-goog-api-key"); got != "AIza-test" { //nolint:gosec // test-only literal, not a real credential
		t.Errorf("x-goog-api-key = %q, want %q", got, "AIza-test")
	}
}

// TestGeminiAdapter_ChatCompletion_NonStreaming proves the adapter builds
// the {base}/v1beta/models/{model}:generateContent URL from req["model"],
// sends the x-goog-api-key header, translates the OpenAI request to
// Gemini's wire format, and translates the Gemini response back to an
// OpenAI-shaped chat.completion body on the wire, with usage extracted
// from usageMetadata.
func TestGeminiAdapter_ChatCompletion_NonStreaming(t *testing.T) {
	const geminiResp = `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi there"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4},"responseId":"resp_01ABC"}`

	var gotBody []byte
	var gotAPIKey, gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		gotAPIKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(geminiResp))
	}))
	defer srv.Close()

	a, err := newGeminiAdapter("p1", srv.URL, "AIza-test")
	if err != nil {
		t.Fatalf("newGeminiAdapter: %v", err)
	}
	rec := httptest.NewRecorder()

	req := map[string]any{
		"model":    "gemini-2.5-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	u, err := a.chatCompletion(context.Background(), rec, req)
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}

	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Errorf("path = %q, want /v1beta/models/gemini-2.5-pro:generateContent", gotPath)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if u.prompt != 10 || u.completion != 4 {
		t.Errorf("usage = %+v, want {prompt:10 completion:4}", u)
	}
	if gotAPIKey != "AIza-test" { //nolint:gosec // test-only literal, not a real credential
		t.Errorf("x-goog-api-key = %q, want %q", gotAPIKey, "AIza-test")
	}

	sent := decodeJSONBody(t, gotBody)
	if _, hasModel := sent["model"]; hasModel {
		t.Errorf("upstream body carries a \"model\" field = %v, want none (Gemini's model is URL-only)", sent["model"])
	}
	contents, ok := sent["contents"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("upstream body contents = %v, want a single-element array", sent["contents"])
	}

	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("client body is not valid JSON: %v (%s)", err, rec.Body.String())
	}
	if out["object"] != "chat.completion" {
		t.Errorf("object = %v, want %q", out["object"], "chat.completion")
	}
	if out["id"] != "chatcmpl-resp_01ABC" {
		t.Errorf("id = %v, want %q", out["id"], "chatcmpl-resp_01ABC")
	}
	choices, ok := out["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices = %v, want a single-element array", out["choices"])
	}
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "hi there" {
		t.Errorf("message.content = %v, want %q", message["content"], "hi there")
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want %q", choice["finish_reason"], "stop")
	}
}

// TestGeminiAdapter_ChatCompletion_ModelsPrefixStripped proves a
// req["model"] that already carries a "models/" prefix is not doubled into
// the URL.
func TestGeminiAdapter_ChatCompletion_ModelsPrefixStripped(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`))
	}))
	defer srv.Close()

	a, err := newGeminiAdapter("p1", srv.URL, "AIza-test")
	if err != nil {
		t.Fatalf("newGeminiAdapter: %v", err)
	}
	rec := httptest.NewRecorder()

	_, err = a.chatCompletion(context.Background(), rec, map[string]any{
		"model":    "models/gemini-2.5-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Errorf("path = %q, want /v1beta/models/gemini-2.5-pro:generateContent (no doubled models/ prefix)", gotPath)
	}
}

// geminiStreamFrames are the raw SSE "data:" lines a fake streaming
// upstream emits for the tests below: a role/text chunk, a text-only
// continuation, then a finish chunk carrying usageMetadata. Gemini's SSE
// stream has no "event:" field to set, unlike Anthropic's.
var geminiStreamFrames = []string{
	`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hi!"}]}}],"responseId":"resp_STREAM"}`,
	`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":6,"candidatesTokenCount":3}}`,
}

// TestGeminiAdapter_ChatCompletion_Streaming proves the adapter requests
// the :streamGenerateContent?alt=sse URL, forwards a streaming Gemini
// response as OpenAI-shaped SSE chunks on the wire, flushing per event, and
// terminates with "[DONE]" once the upstream stream ends — mirroring
// ruling (b): there is no distinct terminal event of Gemini's own to key
// off of.
func TestGeminiAdapter_ChatCompletion_Streaming(t *testing.T) {
	var gotPath, gotQuery, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("httptest ResponseWriter does not implement http.Flusher")
			return
		}
		for _, data := range geminiStreamFrames {
			fmt.Fprintf(w, "data: %s\n\n", data)
			f.Flush()
		}
	}))
	defer srv.Close()

	a, err := newGeminiAdapter("p1", srv.URL, "AIza-test")
	if err != nil {
		t.Fatalf("newGeminiAdapter: %v", err)
	}
	rec := newRecordingResponseWriter()

	req := map[string]any{
		"model":    "gemini-2.5-pro",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	u, err := a.chatCompletion(context.Background(), rec, req)
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}

	if gotPath != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
		t.Errorf("path = %q, want /v1beta/models/gemini-2.5-pro:streamGenerateContent", gotPath)
	}
	if gotQuery != "alt=sse" {
		t.Errorf("query = %q, want %q", gotQuery, "alt=sse")
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", gotAccept)
	}
	if u.prompt != 6 || u.completion != 3 {
		t.Errorf("usage = %+v, want {prompt:6 completion:3}", u)
	}

	body := rec.body.String()
	if got := rec.header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	wantSubstrings := []string{
		`"delta":{"role":"assistant"}`,
		`"delta":{"content":"Hi!"}`,
		`"finish_reason":"stop"`,
		"data: [DONE]\n\n",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(body, want) {
			t.Errorf("streamed body missing %q; got:\n%s", want, body)
		}
	}
	// 3 chunks (role, content delta, finish) plus one more flush for the
	// terminal [DONE]: 4 flushes, proving the adapter flushes per event
	// rather than buffering the whole stream.
	const wantFlushes = 4
	if rec.flushed < wantFlushes {
		t.Errorf("flushed = %d, want >= %d", rec.flushed, wantFlushes)
	}
}

// TestGeminiAdapter_ChatCompletion_Streaming_CaseInsensitiveContentType
// proves the streaming-vs-JSON-fallback gate matches an upstream's
// Content-Type case-insensitively, per lesson (g) — tested with
// "Text/Event-Stream; charset=utf-8".
func TestGeminiAdapter_ChatCompletion_Streaming_CaseInsensitiveContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "Text/Event-Stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		f, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("httptest ResponseWriter does not implement http.Flusher")
			return
		}
		for _, data := range geminiStreamFrames {
			fmt.Fprintf(w, "data: %s\n\n", data)
			f.Flush()
		}
	}))
	defer srv.Close()

	a, err := newGeminiAdapter("p1", srv.URL, "AIza-test")
	if err != nil {
		t.Fatalf("newGeminiAdapter: %v", err)
	}
	rec := newRecordingResponseWriter()

	req := map[string]any{
		"model":    "gemini-2.5-pro",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	u, err := a.chatCompletion(context.Background(), rec, req)
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}
	if u.prompt != 6 || u.completion != 3 {
		t.Errorf("usage = %+v, want {prompt:6 completion:3} (proves the streaming path, not JSON fallback, was taken)", u)
	}
	if got := rec.header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type on client response = %q, want %q (sseWriter's own header; JSON fallback would copy the upstream's mixed-case value verbatim)", got, "text/event-stream")
	}
}

// TestGeminiAdapter_ChatCompletion_Upstream429 proves a non-2xx upstream
// response returns a *providerHTTPError with nothing written to the
// client.
func TestGeminiAdapter_ChatCompletion_Upstream429(t *testing.T) {
	const errBody = `{"error":{"code":429,"message":"rate limited","status":"RESOURCE_EXHAUSTED"}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(errBody))
	}))
	defer srv.Close()

	a, err := newGeminiAdapter("p1", srv.URL, "AIza-test")
	if err != nil {
		t.Fatalf("newGeminiAdapter: %v", err)
	}
	rec := httptest.NewRecorder()

	u, err := a.chatCompletion(context.Background(), rec, map[string]any{
		"model":    "gemini-2.5-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if err == nil {
		t.Fatalf("chatCompletion: want error, got nil (usage=%+v)", u)
	}
	var httpErr *providerHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v (%T), want *providerHTTPError", err, err)
	}
	if httpErr.status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", httpErr.status, http.StatusTooManyRequests)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("client response body = %q, want empty", rec.Body.String())
	}
}

// TestGeminiAdapter_ChatCompletion_TranslateErrorSkipsUpstream proves a
// request field geminiRequestFromOpenAI rejects (n>1, here) never reaches
// the upstream at all.
func TestGeminiAdapter_ChatCompletion_TranslateErrorSkipsUpstream(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a, err := newGeminiAdapter("p1", srv.URL, "AIza-test")
	if err != nil {
		t.Fatalf("newGeminiAdapter: %v", err)
	}
	rec := httptest.NewRecorder()

	_, err = a.chatCompletion(context.Background(), rec, map[string]any{
		"model":    "gemini-2.5-pro",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"n":        float64(2),
	})
	var terr *translateError
	if !errors.As(err, &terr) {
		t.Fatalf("err = %v (%T), want *translateError", err, err)
	}
	if called {
		t.Errorf("upstream was called, want the translate error to short-circuit before any request")
	}
}

// TestGeminiAdapter_Embeddings_Single proves embeddings builds the
// :embedContent URL and translates the response to an OpenAI embeddings
// list body for a single-string "input".
func TestGeminiAdapter_Embeddings_Single(t *testing.T) {
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"embedding":{"values":[0.1,0.2,0.3]}}`))
	}))
	defer srv.Close()

	a, err := newGeminiAdapter("p1", srv.URL, "AIza-test")
	if err != nil {
		t.Fatalf("newGeminiAdapter: %v", err)
	}
	rec := httptest.NewRecorder()

	u, err := a.embeddings(context.Background(), rec, map[string]any{
		"model": "text-embedding-004",
		"input": "hello world",
	})
	if err != nil {
		t.Fatalf("embeddings: %v", err)
	}
	if gotPath != "/v1beta/models/text-embedding-004:embedContent" {
		t.Errorf("path = %q, want /v1beta/models/text-embedding-004:embedContent", gotPath)
	}
	sent := decodeJSONBody(t, gotBody)
	if _, hasRequests := sent["requests"]; hasRequests {
		t.Errorf("single-input body carries \"requests\", want the :embedContent shape")
	}
	if !u.estimated {
		t.Errorf("usage.estimated = false, want true (Gemini reports no embeddings usage)")
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("client body is not valid JSON: %v (%s)", err, rec.Body.String())
	}
	if out["object"] != "list" {
		t.Errorf("object = %v, want %q", out["object"], "list")
	}
	data, ok := out["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("data = %v, want a single-element array", out["data"])
	}
}

// TestGeminiAdapter_Embeddings_Batch proves an array "input" selects
// Gemini's :batchEmbedContents endpoint and translates its response to an
// OpenAI embeddings list body with one entry per input string.
func TestGeminiAdapter_Embeddings_Batch(t *testing.T) {
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"embeddings":[{"values":[0.1,0.2]},{"values":[0.3,0.4]}]}`))
	}))
	defer srv.Close()

	a, err := newGeminiAdapter("p1", srv.URL, "AIza-test")
	if err != nil {
		t.Fatalf("newGeminiAdapter: %v", err)
	}
	rec := httptest.NewRecorder()

	_, err = a.embeddings(context.Background(), rec, map[string]any{
		"model": "text-embedding-004",
		"input": []any{"hello", "world!!"},
	})
	if err != nil {
		t.Fatalf("embeddings: %v", err)
	}
	if gotPath != "/v1beta/models/text-embedding-004:batchEmbedContents" {
		t.Errorf("path = %q, want /v1beta/models/text-embedding-004:batchEmbedContents", gotPath)
	}
	sent := decodeJSONBody(t, gotBody)
	requests, ok := sent["requests"].([]any)
	if !ok || len(requests) != 2 {
		t.Fatalf("requests = %v, want a 2-element array", sent["requests"])
	}
	first := requests[0].(map[string]any)
	if first["model"] != "models/text-embedding-004" {
		t.Errorf("requests[0].model = %v, want %q", first["model"], "models/text-embedding-004")
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("client body is not valid JSON: %v (%s)", err, rec.Body.String())
	}
	data, ok := out["data"].([]any)
	if !ok || len(data) != 2 {
		t.Fatalf("data = %v, want a 2-element array", out["data"])
	}
}

// TestGeminiAdapter_ListModels proves listModels parses Gemini's
// "models": [{"name": "models/..."}] Models API response into a flat
// slice of bare ids, stripping the "models/" prefix.
func TestGeminiAdapter_ListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models" {
			t.Errorf("path = %q, want /v1beta/models", r.URL.Path)
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
			return
		}
		if got := r.Header.Get("x-goog-api-key"); got != "AIza-test" {
			t.Errorf("x-goog-api-key = %q, want %q", got, "AIza-test")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-2.5-pro"},{"name":"models/gemini-2.5-flash"}]}`))
	}))
	defer srv.Close()

	a, err := newGeminiAdapter("p1", srv.URL, "AIza-test")
	if err != nil {
		t.Fatalf("newGeminiAdapter: %v", err)
	}

	got, err := a.listModels(context.Background())
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	want := []string{"gemini-2.5-pro", "gemini-2.5-flash"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %#v, want %#v", got, want)
	}
}
