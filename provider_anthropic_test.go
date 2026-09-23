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

// TestAnthropicAdapter_ChatCompletion_NonStreaming proves the adapter
// translates an OpenAI-shaped request to Anthropic's wire format, sends
// the required x-api-key/anthropic-version headers, and translates the
// Anthropic response back to an OpenAI-shaped chat.completion body on the
// wire, with usage extracted from Anthropic's input_tokens/output_tokens.
func TestAnthropicAdapter_ChatCompletion_NonStreaming(t *testing.T) {
	const anthResp = `{"id":"msg_01ABC","type":"message","role":"assistant","content":[{"type":"text","text":"hi there"}],"model":"claude-opus-5","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":4}}`

	var gotBody []byte
	var gotAPIKey, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		gotBody, _ = io.ReadAll(r.Body)
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthResp))
	}))
	defer srv.Close()

	a, err := newAnthropicAdapter("p1", srv.URL, "sk-ant-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}
	rec := httptest.NewRecorder()

	req := map[string]any{
		"model":    "claude-opus-5",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	u, err := a.chatCompletion(context.Background(), rec, req)
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}

	if u.prompt != 10 || u.completion != 4 {
		t.Errorf("usage = %+v, want {prompt:10 completion:4}", u)
	}
	if gotAPIKey != "sk-ant-test" { //nolint:gosec // test-only literal, not a real credential
		t.Errorf("x-api-key = %q, want %q", gotAPIKey, "sk-ant-test")
	}
	if gotVersion != anthropicAPIVersion {
		t.Errorf("anthropic-version = %q, want %q", gotVersion, anthropicAPIVersion)
	}

	sent := decodeJSONBody(t, gotBody)
	if sent["max_tokens"] != float64(anthropicDefaultMaxTokens) {
		t.Errorf("upstream max_tokens = %v, want %v (default)", sent["max_tokens"], anthropicDefaultMaxTokens)
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
	if out["id"] != "chatcmpl-msg_01ABC" {
		t.Errorf("id = %v, want %q", out["id"], "chatcmpl-msg_01ABC")
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

// anthropicStreamFrames are the raw SSE events a fake streaming upstream
// emits for the tests below: a message_start, one text delta, and a
// message_delta/message_stop pair.
var anthropicStreamFrames = []struct {
	event string
	data  string
}{
	{"message_start", `{"type":"message_start","message":{"id":"msg_01STREAM","usage":{"input_tokens":6,"output_tokens":1}}}`},
	{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
	{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi!"}}`},
	{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`},
	{"message_stop", `{"type":"message_stop"}`},
}

// TestAnthropicAdapter_ChatCompletion_Streaming proves the adapter
// forwards a streaming Anthropic response as OpenAI-shaped SSE chunks on
// the wire, flushing per event, terminated by "[DONE]" once the upstream
// stream ends (ruling (b): message_stop carries no output of its own —
// the adapter writes "[DONE]" after readSSE returns, not because of any
// particular event).
func TestAnthropicAdapter_ChatCompletion_Streaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("httptest ResponseWriter does not implement http.Flusher")
		}
		for _, frame := range anthropicStreamFrames {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", frame.event, frame.data)
			f.Flush()
		}
	}))
	defer srv.Close()

	a, err := newAnthropicAdapter("p1", srv.URL, "sk-ant-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}
	rec := newRecordingResponseWriter()

	req := map[string]any{
		"model":    "claude-opus-5",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	u, err := a.chatCompletion(context.Background(), rec, req)
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}

	if u.prompt != 6 || u.completion != 3 {
		t.Errorf("usage = %+v, want {prompt:6 completion:3}", u)
	}

	body := rec.body.String()
	if got := rec.header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	// 4 chunks are expected on the wire: role, content delta, finish — the
	// content_block_start/stop and message_stop events produce nothing —
	// plus the terminal [DONE].
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
	// 3 events translate to a chunk (message_start, the text delta,
	// message_delta) — content_block_start/stop produce nothing — plus one
	// more flush for the terminal [DONE]: 4 flushes, one per writeData/
	// writeDone call, proving the adapter flushes per event rather than
	// buffering the whole stream.
	const wantFlushes = 4
	if rec.flushed < wantFlushes {
		t.Errorf("flushed = %d, want >= %d", rec.flushed, wantFlushes)
	}
}

// sseDataChunks splits an SSE response body into its individual "data:
// ..." payloads, in wire order, stripping the "data: " prefix — a small
// shared parser for the M1 tests below, since forwardStream's per-event
// framing (sseWriter.writeData, sse.go) always separates events with a
// blank line.
func sseDataChunks(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		data, ok := strings.CutPrefix(frame, "data: ")
		if !ok {
			t.Fatalf("SSE frame missing %q prefix: %q", "data: ", frame)
		}
		out = append(out, data)
	}
	return out
}

// TestAnthropicAdapter_ChatCompletion_Streaming_IncludeUsage_EmitsFinalUsageChunk
// is the M1 regression test: a client that sets
// stream_options.include_usage on a request routed to an anthropic-type
// provider must still get a final usage-only chunk on the wire, exactly
// as an OpenAI-type provider's own forwardStream relays from a real
// upstream usage chunk (provider_openai.go) — Anthropic has no such
// upstream chunk, so the adapter must synthesize one from the usage it
// already accumulated translating message_start/message_delta.
func TestAnthropicAdapter_ChatCompletion_Streaming_IncludeUsage_EmitsFinalUsageChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("httptest ResponseWriter does not implement http.Flusher")
		}
		for _, frame := range anthropicStreamFrames {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", frame.event, frame.data)
			f.Flush()
		}
	}))
	defer srv.Close()

	a, err := newAnthropicAdapter("p1", srv.URL, "sk-ant-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}
	rec := newRecordingResponseWriter()

	req := map[string]any{
		"model":          "claude-opus-5",
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       []any{map[string]any{"role": "user", "content": "hello"}},
	}
	u, err := a.chatCompletion(context.Background(), rec, req)
	if err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}
	if u.prompt != 6 || u.completion != 3 {
		t.Fatalf("usage = %+v, want {prompt:6 completion:3}", u)
	}

	chunks := sseDataChunks(t, rec.body.String())
	if len(chunks) < 2 {
		t.Fatalf("got %d SSE frames, want at least a usage chunk followed by [DONE]", len(chunks))
	}
	if chunks[len(chunks)-1] != "[DONE]" {
		t.Fatalf("last SSE frame = %q, want [DONE]", chunks[len(chunks)-1])
	}
	usageFrame := chunks[len(chunks)-2]
	var got struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
		Choices []any `json:"choices"`
	}
	if err := json.Unmarshal([]byte(usageFrame), &got); err != nil {
		t.Fatalf("decode usage chunk %q: %v", usageFrame, err)
	}
	if len(got.Choices) != 0 {
		t.Errorf("usage chunk choices = %v, want empty", got.Choices)
	}
	if got.Usage == nil {
		t.Fatalf("usage chunk has no usage object: %q", usageFrame)
	}
	if got.Usage.PromptTokens != 6 || got.Usage.CompletionTokens != 3 || got.Usage.TotalTokens != 9 {
		t.Errorf("usage chunk = %+v, want {prompt_tokens:6 completion_tokens:3 total_tokens:9}", *got.Usage)
	}
}

// TestAnthropicAdapter_ChatCompletion_Streaming_NoIncludeUsage_NoFinalUsageChunk
// proves the M1 fix is opt-in: a client that never sets
// stream_options.include_usage gets the stream unchanged from before the
// fix — no extra usage-only chunk on the wire.
func TestAnthropicAdapter_ChatCompletion_Streaming_NoIncludeUsage_NoFinalUsageChunk(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("httptest ResponseWriter does not implement http.Flusher")
		}
		for _, frame := range anthropicStreamFrames {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", frame.event, frame.data)
			f.Flush()
		}
	}))
	defer srv.Close()

	a, err := newAnthropicAdapter("p1", srv.URL, "sk-ant-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}
	rec := newRecordingResponseWriter()

	req := map[string]any{
		"model":    "claude-opus-5",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	if _, err := a.chatCompletion(context.Background(), rec, req); err != nil {
		t.Fatalf("chatCompletion: %v", err)
	}

	for _, chunk := range sseDataChunks(t, rec.body.String()) {
		if strings.Contains(chunk, `"usage"`) {
			t.Errorf("SSE frame %q carries a usage field though the client never set stream_options.include_usage", chunk)
		}
	}
}

// TestAnthropicAdapter_ChatCompletion_Streaming_CaseInsensitiveContentType
// proves the streaming-vs-JSON-fallback gate matches an upstream's
// Content-Type case-insensitively — review fix, mirroring the same fix
// in the openai-type adapter. Distinguishing evidence: the streaming path
// extracts real usage from the SSE events and sets Content-Type to the
// sseWriter's own lowercase "text/event-stream"; the JSON fallback path
// would copy the upstream's mixed-case header verbatim and fail to
// decode a body of raw SSE text as a single Anthropic response.
func TestAnthropicAdapter_ChatCompletion_Streaming_CaseInsensitiveContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "Text/Event-Stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		f, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("httptest ResponseWriter does not implement http.Flusher")
		}
		for _, frame := range anthropicStreamFrames {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", frame.event, frame.data)
			f.Flush()
		}
	}))
	defer srv.Close()

	a, err := newAnthropicAdapter("p1", srv.URL, "sk-ant-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}
	rec := newRecordingResponseWriter()

	req := map[string]any{
		"model":    "claude-opus-5",
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

// TestAnthropicAdapter_ChatCompletion_Upstream429 proves a non-2xx
// upstream response returns a *providerHTTPError with nothing written to
// the client, mirroring the openai-type adapter's contract.
func TestAnthropicAdapter_ChatCompletion_Upstream429(t *testing.T) {
	const errBody = `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(errBody))
	}))
	defer srv.Close()

	a, err := newAnthropicAdapter("p1", srv.URL, "sk-ant-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}
	rec := httptest.NewRecorder()

	u, err := a.chatCompletion(context.Background(), rec, map[string]any{
		"model":    "claude-opus-5",
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

// TestAnthropicAdapter_ChatCompletion_TranslateErrorSkipsUpstream proves a
// request field anthropicRequestFromOpenAI rejects (n>1, here) never
// reaches the upstream at all — the adapter returns the *translateError
// before making any HTTP request.
func TestAnthropicAdapter_ChatCompletion_TranslateErrorSkipsUpstream(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a, err := newAnthropicAdapter("p1", srv.URL, "sk-ant-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}
	rec := httptest.NewRecorder()

	_, err = a.chatCompletion(context.Background(), rec, map[string]any{
		"model":    "claude-opus-5",
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

// TestAnthropicAdapter_ListModels proves listModels parses Anthropic's
// "data": [{"id": ...}] Models API response into a flat slice of ids.
func TestAnthropicAdapter_ListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		if got := r.Header.Get("x-api-key"); got != "sk-ant-test" {
			t.Errorf("x-api-key = %q, want %q", got, "sk-ant-test")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5"},{"id":"claude-sonnet-5"}]}`))
	}))
	defer srv.Close()

	a, err := newAnthropicAdapter("p1", srv.URL, "sk-ant-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}

	got, err := a.listModels(context.Background())
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	want := []string{"claude-opus-5", "claude-sonnet-5"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestAnthropicAdapter_ListModels_FollowsPagination is the M7 regression
// test: has_more:true plus a last_id must make listModels fetch a second
// page via ?after_id=<last_id> and return the union of both pages, not
// just the first page's 20 (Anthropic's own default page size).
func TestAnthropicAdapter_ListModels_FollowsPagination(t *testing.T) {
	var gotAfterIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAfterIDs = append(gotAfterIDs, r.URL.Query().Get("after_id"))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after_id") == "" {
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-opus-5"},{"id":"claude-sonnet-5"}],"has_more":true,"last_id":"claude-sonnet-5"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-haiku-5"}],"has_more":false,"last_id":"claude-haiku-5"}`))
	}))
	defer srv.Close()

	a, err := newAnthropicAdapter("p1", srv.URL, "sk-ant-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}

	got, err := a.listModels(context.Background())
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	want := []string{"claude-opus-5", "claude-sonnet-5", "claude-haiku-5"}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if len(gotAfterIDs) != 2 || gotAfterIDs[0] != "" || gotAfterIDs[1] != "claude-sonnet-5" {
		t.Errorf("after_id sequence = %#v, want [\"\", \"claude-sonnet-5\"] (first page unqualified, second page's after_id = first page's last_id)", gotAfterIDs)
	}
}

// TestAnthropicAdapter_ListModels_PageCapStopsRunawayPagination proves
// the M7 fix's other half: an upstream that always reports has_more:true
// with an ever-advancing last_id does not page forever — listModels
// stops after anthropicListModelsPageCap requests and returns
// successfully with whatever it accumulated, rather than looping until
// context cancellation or exhausting memory.
func TestAnthropicAdapter_ListModels_PageCapStopsRunawayPagination(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		id := fmt.Sprintf("model-%d", requests)
		fmt.Fprintf(w, `{"data":[{"id":%q}],"has_more":true,"last_id":%q}`, id, id)
	}))
	defer srv.Close()

	a, err := newAnthropicAdapter("p1", srv.URL, "sk-ant-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}

	got, err := a.listModels(context.Background())
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	if requests != anthropicListModelsPageCap {
		t.Errorf("upstream received %d requests, want exactly %d (the page cap)", requests, anthropicListModelsPageCap)
	}
	if len(got) != anthropicListModelsPageCap {
		t.Errorf("got %d ids, want %d (one per page up to the cap)", len(got), anthropicListModelsPageCap)
	}
}
