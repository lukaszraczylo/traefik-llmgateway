package traefikllmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// captureBodyHandler wraps fn, first draining and recording the request
// body (and, if capturedAuth is non-nil, the Authorization header) so a
// test can assert on what the adapter actually sent upstream.
func captureBodyHandler(capturedBody *[]byte, capturedAuth *string, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if capturedBody != nil {
			*capturedBody = b
		}
		if capturedAuth != nil {
			*capturedAuth = r.Header.Get("Authorization")
		}
		fn(w, r)
	}
}

// decodeJSONBody unmarshals b into a map for structural assertions,
// avoiding brittleness from json.Marshal's key ordering.
func decodeJSONBody(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode captured request body: %v (body=%s)", err, b)
	}
	return m
}

// TestOpenAIAdapter_ChatCompletion_NonStreaming proves the non-streaming
// path copies the upstream's Content-Type, status, and body verbatim to
// the client, extracts prompt/completion usage from the body, and sends no
// stream_options since the request never asked to stream.
func TestOpenAIAdapter_ChatCompletion_NonStreaming(t *testing.T) {
	const respBody = `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":20}}`

	var gotBody []byte
	var gotAuth string
	srv := httptest.NewServer(captureBodyHandler(&gotBody, &gotAuth, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	rec := httptest.NewRecorder()

	req := map[string]any{"model": "gpt-5", "messages": []any{}}
	u, err := a.chatCompletion(context.Background(), rec, req)
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}

	if u.prompt != 10 || u.completion != 20 {
		t.Errorf("usage = %+v, want {prompt:10 completion:20}", u)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want %q (verbatim passthrough)", rec.Body.String(), respBody)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sk-test")
	}

	sent := decodeJSONBody(t, gotBody)
	if _, present := sent["stream_options"]; present {
		t.Errorf("stream_options sent on a non-streaming request: %v", sent["stream_options"])
	}
}

// TestOpenAIAdapter_Embeddings proves embeddings uses the same
// verbatim-passthrough forwarding as chatCompletion's non-streaming path,
// against /v1/embeddings, extracting only prompt_tokens.
func TestOpenAIAdapter_Embeddings(t *testing.T) {
	const respBody = `{"data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":5}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %q, want /v1/embeddings", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	rec := httptest.NewRecorder()

	u, err := a.embeddings(context.Background(), rec, map[string]any{"model": "text-embedding-3", "input": "hello"})
	if err != nil {
		t.Fatalf("embeddings: %v", err)
	}
	if u.prompt != 5 || u.completion != 0 {
		t.Errorf("usage = %+v, want {prompt:5 completion:0}", u)
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want %q", rec.Body.String(), respBody)
	}
}

// streamFrames are the four SSE data payloads a fake streaming upstream
// emits for the tests below: three content chunks, then a final
// choices-empty usage chunk.
var streamFrames = []string{
	`{"id":"1","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
	`{"id":"1","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
	`{"id":"1","choices":[{"index":0,"delta":{"content":"!"}}]}`,
	`{"id":"1","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":9}}`,
}

// newStreamingFakeUpstream returns an httptest.Server whose
// /v1/chat/completions handler records the request body, then emits
// streamFrames as SSE, flushing and sleeping 50ms between each, and
// finally "[DONE]".
func newStreamingFakeUpstream(t *testing.T, capturedBody *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(captureBodyHandler(capturedBody, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("httptest ResponseWriter does not implement http.Flusher")
		}
		for _, frame := range streamFrames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
			f.Flush()
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}))
}

// TestOpenAIAdapter_ChatCompletion_Streaming_ClientDidNotAskUsage proves
// that when the client streams without setting stream_options,
// chatCompletion injects include_usage upstream on its own, forwards only
// the 3 content chunks plus [DONE] to the client (suppressing the
// usage-only chunk), still captures the usage it carried, and flushes at
// least once per forwarded event.
func TestOpenAIAdapter_ChatCompletion_Streaming_ClientDidNotAskUsage(t *testing.T) {
	var gotBody []byte
	srv := newStreamingFakeUpstream(t, &gotBody)
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	rec := newRecordingResponseWriter()

	req := map[string]any{"model": "gpt-5", "stream": true}
	u, err := a.chatCompletion(context.Background(), rec, req)
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}

	if u.prompt != 7 || u.completion != 9 {
		t.Errorf("usage = %+v, want {prompt:7 completion:9}", u)
	}

	want := "data: " + streamFrames[0] + "\n\n" +
		"data: " + streamFrames[1] + "\n\n" +
		"data: " + streamFrames[2] + "\n\n" +
		"data: [DONE]\n\n"
	if got := rec.body.String(); got != want {
		t.Errorf("forwarded body = %q, want %q (usage chunk must be suppressed)", got, want)
	}
	if rec.flushed < 4 {
		t.Errorf("flushed = %d, want >= 4", rec.flushed)
	}

	sent := decodeJSONBody(t, gotBody)
	so, ok := sent["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("upstream request missing injected stream_options: %v", sent)
	}
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v, want true", so["include_usage"])
	}
}

// TestOpenAIAdapter_ChatCompletion_Streaming_ClientAskedUsage proves that
// when the client already set stream_options.include_usage=true itself,
// chatCompletion preserves that value (forcing true on top of true is a
// no-op) and forwards the usage-only chunk through to the client instead
// of suppressing it.
func TestOpenAIAdapter_ChatCompletion_Streaming_ClientAskedUsage(t *testing.T) {
	var gotBody []byte
	srv := newStreamingFakeUpstream(t, &gotBody)
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	rec := newRecordingResponseWriter()

	req := map[string]any{
		"model":          "gpt-5",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	u, err := a.chatCompletion(context.Background(), rec, req)
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}

	if u.prompt != 7 || u.completion != 9 {
		t.Errorf("usage = %+v, want {prompt:7 completion:9}", u)
	}

	want := "data: " + streamFrames[0] + "\n\n" +
		"data: " + streamFrames[1] + "\n\n" +
		"data: " + streamFrames[2] + "\n\n" +
		"data: " + streamFrames[3] + "\n\n" +
		"data: [DONE]\n\n"
	if got := rec.body.String(); got != want {
		t.Errorf("forwarded body = %q, want %q (usage chunk must be forwarded)", got, want)
	}
	if rec.flushed < 5 {
		t.Errorf("flushed = %d, want >= 5", rec.flushed)
	}

	sent := decodeJSONBody(t, gotBody)
	so, ok := sent["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("upstream request lost the client's stream_options: %v", sent)
	}
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v, want true (must not be overwritten)", so["include_usage"])
	}
}

// TestOpenAIAdapter_ChatCompletion_Streaming_ClientSuppressesUsage proves
// the C1 fix: a client sending stream_options.include_usage=false must not
// be able to suppress upstream usage reporting and escape token/cost
// budgets. chatCompletion forces include_usage=true upstream regardless,
// still captures the resulting usage for accounting, but derives
// clientAskedUsage from the client's own (false) value — so the usage-only
// chunk is not forwarded to this client. It also proves the merge copies
// the client's stream_options map instead of mutating it in place.
func TestOpenAIAdapter_ChatCompletion_Streaming_ClientSuppressesUsage(t *testing.T) {
	var gotBody []byte
	srv := newStreamingFakeUpstream(t, &gotBody)
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	rec := newRecordingResponseWriter()

	clientStreamOptions := map[string]any{"include_usage": false}
	req := map[string]any{
		"model":          "gpt-5",
		"stream":         true,
		"stream_options": clientStreamOptions,
	}
	u, err := a.chatCompletion(context.Background(), rec, req)
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}

	if u.prompt != 7 || u.completion != 9 {
		t.Errorf("usage = %+v, want {prompt:7 completion:9} (must still be captured despite include_usage=false)", u)
	}

	want := "data: " + streamFrames[0] + "\n\n" +
		"data: " + streamFrames[1] + "\n\n" +
		"data: " + streamFrames[2] + "\n\n" +
		"data: [DONE]\n\n"
	if got := rec.body.String(); got != want {
		t.Errorf("forwarded body = %q, want %q (usage chunk must be suppressed, client never asked)", got, want)
	}

	sent := decodeJSONBody(t, gotBody)
	so, ok := sent["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("upstream request missing stream_options: %v", sent)
	}
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v, want true (must be forced regardless of client value — quota bypass otherwise)", so["include_usage"])
	}

	if clientStreamOptions["include_usage"] != false {
		t.Errorf("client's own stream_options map was mutated in place: %v, want include_usage still false", clientStreamOptions)
	}
}

// TestOpenAIAdapter_ChatCompletion_Streaming_UsageCapturedBeforeAbruptClose
// proves that when the upstream sends the usage-only chunk and then the
// connection drops (no [DONE], no clean EOF), chatCompletion still returns
// the usage it had already captured alongside the error — a dropped
// connection after usage arrived must not read as zero usage.
func TestOpenAIAdapter_ChatCompletion_Streaming_UsageCapturedBeforeAbruptClose(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", streamFrames[3]) // usage-only chunk: {prompt:7 completion:9}
		w.(http.Flusher).Flush()

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatalf("httptest ResponseWriter does not implement http.Hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		// Abrupt close: no [DONE], no terminating chunk — the client must
		// see a genuine read error, not a clean EOF.
		_ = conn.Close()
	}))
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	rec := newRecordingResponseWriter()

	req := map[string]any{"model": "gpt-5", "stream": true}
	u, err := a.chatCompletion(context.Background(), rec, req)
	if err == nil {
		t.Fatalf("chatCompletion: want error from abrupt close, got nil (usage=%+v)", u)
	}
	if !errors.Is(err, errUpstream) {
		t.Errorf("err = %v, want errors.Is(err, errUpstream) == true", err)
	}
	if u.prompt != 7 || u.completion != 9 {
		t.Errorf("usage = %+v, want {prompt:7 completion:9} (captured before the drop)", u)
	}
}

// TestOpenAIAdapter_ChatCompletion_ContextCanceledMidStream proves that
// canceling the caller's context while a streaming response is still being
// read surfaces as errors.Is(err, context.Canceled) at chatCompletion's
// caller — required so Task 12 can tell a client hang-up apart from a real
// upstream failure.
func TestOpenAIAdapter_ChatCompletion_ContextCanceledMidStream(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", streamFrames[0])
		w.(http.Flusher).Flush()
		<-block // hold the connection open past the context cancellation below
	}))
	defer srv.Close()
	defer close(block) // unblock the handler before srv.Close() waits on it

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	rec := newRecordingResponseWriter()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := a.chatCompletion(ctx, rec, map[string]any{"model": "gpt-5", "stream": true})
	if err == nil {
		t.Fatalf("chatCompletion: want error from context cancellation, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is(err, context.Canceled) == true", err)
	}
}

// TestOpenAIAdapter_ChatCompletion_Streaming_UpstreamIgnoresStreamFallsBackToJSON
// proves that when a streaming request's upstream answers with a plain
// JSON body instead of SSE (Content-Type not text/event-stream), and sends
// "Accept: text/event-stream" on the outgoing request, chatCompletion falls
// back to the non-streaming passthrough — the client gets the JSON body
// verbatim and usage is still extracted — instead of emitting an empty or
// broken SSE stream.
func TestOpenAIAdapter_ChatCompletion_Streaming_UpstreamIgnoresStreamFallsBackToJSON(t *testing.T) {
	const respBody = `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`

	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	rec := httptest.NewRecorder()

	u, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "stream": true})
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}
	if u.prompt != 3 || u.completion != 4 {
		t.Errorf("usage = %+v, want {prompt:3 completion:4}", u)
	}
	if rec.Body.String() != respBody {
		t.Errorf("body = %q, want %q (JSON fallback, verbatim passthrough)", rec.Body.String(), respBody)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if gotAccept != "text/event-stream" {
		t.Errorf("Accept header sent = %q, want %q", gotAccept, "text/event-stream")
	}
}

// TestOpenAIAdapter_ChatCompletion_Streaming_CaseInsensitiveContentType
// proves the streaming-vs-JSON-fallback gate matches an upstream's
// Content-Type case-insensitively — review fix: Content-Type values are
// case-insensitive per RFC 7231, but the gate previously did a
// case-sensitive substring match against "text/event-stream" and would
// wrongly fall back to the JSON path for an upstream that sent, e.g.,
// "Text/Event-Stream". Distinguishing evidence: the streaming path
// extracts real usage from the SSE chunks and sets Content-Type to the
// sseWriter's own lowercase "text/event-stream"; the JSON fallback path
// would copy the upstream's mixed-case header verbatim and fail to parse
// usage out of raw SSE text.
func TestOpenAIAdapter_ChatCompletion_Streaming_CaseInsensitiveContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "Text/Event-Stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("httptest ResponseWriter does not implement http.Flusher")
		}
		for _, frame := range streamFrames {
			fmt.Fprintf(w, "data: %s\n\n", frame)
			f.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	rec := newRecordingResponseWriter()

	u, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5", "stream": true})
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}
	if u.prompt != 7 || u.completion != 9 {
		t.Errorf("usage = %+v, want {prompt:7 completion:9} (proves the streaming path, not JSON fallback, was taken)", u)
	}
	if got := rec.header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type on client response = %q, want %q (sseWriter's own header; JSON fallback would copy the upstream's mixed-case value verbatim)", got, "text/event-stream")
	}
}

// TestOpenAIAdapter_ChatCompletion_Upstream429 proves a non-2xx upstream
// response returns a *providerHTTPError carrying the status and body,
// with nothing written to the client — the caller decides envelope vs
// passthrough.
func TestOpenAIAdapter_ChatCompletion_Upstream429(t *testing.T) {
	const errBody = `{"error":{"message":"rate limited","type":"rate_limit_error"}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(errBody))
	}))
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	rec := httptest.NewRecorder()

	u, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5"})
	if err == nil {
		t.Fatalf("chatCompletion: want error, got nil (usage=%+v)", u)
	}
	if u != (usage{}) {
		t.Errorf("usage = %+v, want zero value on error", u)
	}

	var httpErr *providerHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v (%T), want *providerHTTPError", err, err)
	}
	if httpErr.status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", httpErr.status, http.StatusTooManyRequests)
	}
	if string(httpErr.body) != errBody {
		t.Errorf("body = %q, want %q", httpErr.body, errBody)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("client response body = %q, want empty (caller decides passthrough)", rec.Body.String())
	}
}

// TestOpenAIAdapter_ListModels proves listModels parses the OpenAI
// /v1/models "data": [{"id": ...}] fixture into a flat slice of ids.
func TestOpenAIAdapter_ListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5"},{"id":"gpt-5-mini"}]}`))
	}))
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "sk-test")
	got, err := a.listModels(context.Background())
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}

	want := []string{"gpt-5", "gpt-5-mini"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestOpenAIAdapter_FetchModelMetadata covers fetchModelMetadata (feature
// v0.23) against an httptest fixture shaped like LM Studio's real, live
// "/api/v0/models" response (task brief: llm/vlm models report
// max_context_length 262144, qwen2.5-0.5b 32768, embeddings 512-8192) —
// including the loaded_context_length-preferred-over-max_context_length
// rule, an entry with no loaded_context_length at all, and a
// metadataPath left unset (the default: a fast no-op, no HTTP call at
// all).
func TestOpenAIAdapter_FetchModelMetadata(t *testing.T) {
	t.Run("metadataPath unset is a fast no-op", func(t *testing.T) {
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		a := newOpenAIAdapter("p1", srv.URL, "sk-test")
		got, err := a.fetchModelMetadata(context.Background())
		if err != nil || got != nil {
			t.Errorf("fetchModelMetadata = (%v, %v), want (nil, nil)", got, err)
		}
		if called {
			t.Error("unset metadataPath must never issue an HTTP request")
		}
	})

	t.Run("LM Studio shaped fixture: prefers loaded over max, falls back when absent", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[
				{"id":"qwen/qwen3-vl-30b","max_context_length":262144,"loaded_context_length":262144},
				{"id":"qwen2.5-0.5b","max_context_length":32768,"loaded_context_length":4096},
				{"id":"text-embedding-nomic-embed-text-v1.5","max_context_length":2048,"loaded_context_length":512},
				{"id":"no-loaded-field","max_context_length":8192},
				{"id":"zero-everything","max_context_length":0,"loaded_context_length":0}
			]}`))
		}))
		defer srv.Close()

		a := newOpenAIAdapter("lmstudio", srv.URL, "")
		a.metadataPath = "/api/v0/models"
		got, err := a.fetchModelMetadata(context.Background())
		if err != nil {
			t.Fatalf("fetchModelMetadata: %v", err)
		}
		if gotPath != "/api/v0/models" {
			t.Errorf("request path = %q, want /api/v0/models", gotPath)
		}

		want := map[string]int{
			"qwen/qwen3-vl-30b":                    262144,
			"qwen2.5-0.5b":                         4096, // loaded_context_length preferred over max_context_length
			"text-embedding-nomic-embed-text-v1.5": 512,
			"no-loaded-field":                      8192, // falls back to max_context_length
			// "zero-everything" omitted entirely: both fields are 0.
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %#v, want %#v", got, want)
		}
	})

	t.Run("non-2xx response is an error, not a captured empty map", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		a := newOpenAIAdapter("p1", srv.URL, "")
		a.metadataPath = "/api/v0/models"
		if _, err := a.fetchModelMetadata(context.Background()); err == nil {
			t.Error("want an error for a non-2xx metadata response")
		}
	})

	t.Run("malformed JSON body is a decode error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{not json`))
		}))
		defer srv.Close()

		a := newOpenAIAdapter("p1", srv.URL, "")
		a.metadataPath = "/api/v0/models"
		if _, err := a.fetchModelMetadata(context.Background()); err == nil {
			t.Error("want a decode error for a malformed metadata response body")
		}
	})

	t.Run("unreachable upstream is an error", func(t *testing.T) {
		a := newOpenAIAdapter("p1", "http://127.0.0.1:1", "") // reserved, never listening
		a.metadataPath = "/api/v0/models"
		if _, err := a.fetchModelMetadata(context.Background()); err == nil {
			t.Error("want an error when the metadata endpoint is unreachable")
		}
	})
}

// TestOpenAIAdapter_Keyless proves a resolved-empty API key sends no
// Authorization header at all, not one with an empty token — the
// operator's own keyless gateway is a real deployment target.
func TestOpenAIAdapter_Keyless(t *testing.T) {
	var gotAuth string
	sawHeader := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, sawHeader = r.Header["Authorization"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`))
	}))
	defer srv.Close()

	a := newOpenAIAdapter("p1", srv.URL, "")
	rec := httptest.NewRecorder()

	u, err := a.chatCompletion(context.Background(), rec, map[string]any{"model": "gpt-5"})
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}
	if u.prompt != 1 || u.completion != 2 {
		t.Errorf("usage = %+v, want {prompt:1 completion:2}", u)
	}
	if sawHeader {
		t.Errorf("Authorization header present (%q), want absent for a keyless adapter", gotAuth)
	}
}

// TestBuildAdapters covers buildAdapters' config-resolution rules: secret
// resolution and propagation, base URL defaulting and trailing-slash
// trimming, keyless as valid (not an error), and the unknown/not-yet-
// implemented type errors.
func TestBuildAdapters(t *testing.T) {
	t.Run("good: openai with literal key defaults base URL", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"p1": {Type: "openai", APIKey: "sk-test"},
		}}
		adapters, err := buildAdapters(cfg)
		if err != nil {
			t.Fatalf("buildAdapters: %v", err)
		}
		a, ok := adapters["p1"]
		if !ok {
			t.Fatalf("adapters[%q] missing", "p1")
		}
		if a.name() != "p1" {
			t.Errorf("name() = %q, want %q", a.name(), "p1")
		}
		if a.typeName() != "openai" {
			t.Errorf("typeName() = %q, want %q", a.typeName(), "openai")
		}
		if a.base() != defaultBaseOpenAI {
			t.Errorf("base() = %q, want %q", a.base(), defaultBaseOpenAI)
		}

		req := httptest.NewRequest(http.MethodPost, "/", nil)
		a.injectAuth(req)
		if got := req.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer sk-test")
		}
	})

	t.Run("good: metadataPath is wired onto the openai adapter (feature v0.23)", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"lmstudio": {Type: "openai", MetadataPath: "/api/v0/models"},
		}}
		adapters, err := buildAdapters(cfg)
		if err != nil {
			t.Fatalf("buildAdapters: %v", err)
		}
		a, ok := adapters["lmstudio"].(*openaiAdapter)
		if !ok {
			t.Fatalf("adapters[%q] is not *openaiAdapter", "lmstudio")
		}
		if a.metadataPath != "/api/v0/models" {
			t.Errorf("metadataPath = %q, want %q", a.metadataPath, "/api/v0/models")
		}
	})

	t.Run("bad: metadataPath without a leading slash is a constructor error (review fix)", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"lmstudio": {Type: "openai", MetadataPath: "api/v0/models"},
		}}
		if _, err := buildAdapters(cfg); err == nil {
			t.Error("want an error for a metadataPath missing its leading slash")
		}
	})

	t.Run("good: empty metadataPath is valid (feature disabled)", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"p1": {Type: "openai", APIKey: "sk-test"},
		}}
		if _, err := buildAdapters(cfg); err != nil {
			t.Errorf("buildAdapters: %v, want no error for an empty metadataPath", err)
		}
	})

	t.Run("good: explicit base URL trailing slash trimmed", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"p1": {Type: "openai", BaseURL: "https://custom.example.com/", APIKey: "sk-test"},
		}}
		adapters, err := buildAdapters(cfg)
		if err != nil {
			t.Fatalf("buildAdapters: %v", err)
		}
		if got := adapters["p1"].base(); got != "https://custom.example.com" {
			t.Errorf("base() = %q, want %q", got, "https://custom.example.com")
		}
	})

	t.Run("good: empty apiKey is keyless, not an error", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"p1": {Type: "openai"},
		}}
		adapters, err := buildAdapters(cfg)
		if err != nil {
			t.Fatalf("buildAdapters: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		adapters["p1"].injectAuth(req)
		if _, present := req.Header["Authorization"]; present {
			t.Errorf("Authorization header present for a keyless provider")
		}
	})

	t.Run("good: env: apiKey resolves via resolveSecret", func(t *testing.T) {
		t.Setenv("LLMGW_T2", "sk-from-env")
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"p1": {Type: "openai", APIKey: "env:LLMGW_T2"}, //nolint:gosec // "env:NAME" is a resolveSecret indirection, not a credential
		}}
		adapters, err := buildAdapters(cfg)
		if err != nil {
			t.Fatalf("buildAdapters: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		adapters["p1"].injectAuth(req)
		if got := req.Header.Get("Authorization"); got != "Bearer sk-from-env" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer sk-from-env")
		}
	})

	t.Run("bad: env: apiKey unset propagates resolveSecret's error", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"p1": {Type: "openai", APIKey: "env:LLMGW_MISSING2"}, //nolint:gosec // "env:NAME" is a resolveSecret indirection, not a credential
		}}
		if _, err := buildAdapters(cfg); err == nil {
			t.Fatalf("buildAdapters: want error for unset env var, got nil")
		}
	})

	t.Run("bad: unknown provider type", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"p1": {Type: "not-a-real-type"},
		}}
		_, err := buildAdapters(cfg)
		if err == nil {
			t.Fatalf("buildAdapters: want error for unknown type, got nil")
		}
	})

	t.Run("good: anthropic with an API key builds successfully", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"p1": {Type: "anthropic", APIKey: "sk-test"},
		}}
		adapters, err := buildAdapters(cfg)
		if err != nil {
			t.Fatalf("buildAdapters: %v", err)
		}
		a, ok := adapters["p1"]
		if !ok {
			t.Fatalf("adapters[%q] missing", "p1")
		}
		if a.typeName() != "anthropic" {
			t.Errorf("typeName() = %q, want %q", a.typeName(), "anthropic")
		}
		if a.base() != defaultBaseAnthropic {
			t.Errorf("base() = %q, want %q", a.base(), defaultBaseAnthropic)
		}
	})

	t.Run("bad: anthropic without an API key is a constructor error", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"p1": {Type: "anthropic"},
		}}
		if _, err := buildAdapters(cfg); err == nil {
			t.Fatalf("buildAdapters: want error for keyless anthropic provider, got nil")
		}
	})

	t.Run("good: gemini with an API key builds successfully", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"p1": {Type: "gemini", APIKey: "sk-test"},
		}}
		adapters, err := buildAdapters(cfg)
		if err != nil {
			t.Fatalf("buildAdapters: %v", err)
		}
		a, ok := adapters["p1"]
		if !ok {
			t.Fatalf("adapters[%q] missing", "p1")
		}
		if a.typeName() != "gemini" {
			t.Errorf("typeName() = %q, want %q", a.typeName(), "gemini")
		}
		if a.base() != defaultBaseGemini {
			t.Errorf("base() = %q, want %q", a.base(), defaultBaseGemini)
		}
	})

	t.Run("bad: gemini without an API key is a constructor error", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{
			"p1": {Type: "gemini"},
		}}
		if _, err := buildAdapters(cfg); err == nil {
			t.Fatalf("buildAdapters: want error for keyless gemini provider, got nil")
		}
	})

	t.Run("bad: nil provider config value is a constructor error, not a panic", func(t *testing.T) {
		cfg := &Config{Providers: map[string]*ProviderConfig{"p1": nil}}
		if _, err := buildAdapters(cfg); err == nil {
			t.Fatalf("buildAdapters: want error for a nil ProviderConfig value, got nil")
		}
	})
}

// TestValidateConfigName covers validateConfigName's character-set,
// dot-only-name, and reserved-name rules, shared by buildAdapters
// (provider names) and validateTargetURLs (mcpServers/agents names,
// mcp_a2a_test.go) — a provider, MCP server, or agent named "v1", "mcp",
// or "a2a" would shadow one of the gateway's own fixed top-level routes,
// and one named "." or ".." reads as a directory-traversal segment once
// embedded as a path segment in the gateway's own routes.
func TestValidateConfigName(t *testing.T) {
	tests := []struct {
		kind    string
		name    string
		wantErr bool
	}{
		{"provider", "openai", false},
		{"provider", "my-provider_1.local", false},
		{"provider", "openai grok", true}, // space
		{"provider", "openai/grok", true}, // slash: would break passthroughRoute's own path splitting
		{"provider", "", true},            // empty never matches configNamePattern
		{"provider", "v1", true},          // reserved: unified API namespace
		{"provider", "mcp", true},         // reserved: MCP target-proxy prefix
		{"provider", "a2a", true},         // reserved: A2A target-proxy prefix
		{"mcpServers", "v1", true},        // same reserved set applies to every kind
		{"agents", "a2a", true},
		{"provider", ".", true},    // dot-only: matches configNamePattern's character class but rejected anyway
		{"provider", "..", true},   // dot-only: same
		{"mcpServers", ".", true},  // dot-only rule applies to every kind
		{"agents", "..", true},     // dot-only rule applies to every kind
		{"provider", "...", false}, // three dots is NOT one of dotOnlyConfigNames — the rule is exact-match on "." and "..", not "any dots-only string"
	}
	for _, tt := range tests {
		t.Run(tt.kind+"/"+tt.name, func(t *testing.T) {
			err := validateConfigName(tt.kind, tt.name)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateConfigName(%q, %q) error = %v, wantErr %v", tt.kind, tt.name, err, tt.wantErr)
			}
		})
	}
}

// TestBuildAdapters_RejectsReservedOrInvalidProviderName covers buildAdapters'
// own call to validateConfigName, end to end through the map key rather than
// validateConfigName in isolation.
func TestBuildAdapters_RejectsReservedOrInvalidProviderName(t *testing.T) {
	tests := []struct {
		name string
	}{
		{"v1"},
		{"mcp"},
		{"a2a"},
		{"has a space"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Providers: map[string]*ProviderConfig{
				tt.name: {Type: "openai", APIKey: "sk-test"},
			}}
			if _, err := buildAdapters(cfg); err == nil {
				t.Fatalf("buildAdapters: want error for provider name %q, got nil", tt.name)
			}
		})
	}
}
