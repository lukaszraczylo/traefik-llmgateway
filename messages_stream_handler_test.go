package traefikllmgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sseUpstream answers every request with frames, flushing after each one,
// and records the decoded request body.
func sseUpstream(t *testing.T, gotBody *map[string]any, calls *int32, frames ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		if gotBody != nil {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, gotBody)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		for _, fr := range frames {
			_, _ = io.WriteString(w, fr)
			if f != nil {
				f.Flush()
			}
		}
	}))
}

func streamMessagesBody(model string) map[string]any {
	return map[string]any{
		"model": model, "max_tokens": 64, "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "weather in Paris?"}},
	}
}

func serveMessages(t *testing.T, gw *Gateway, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, newMessagesRequest(t, body, "x-api-key", "sk-alice"))
	return rec
}

func userTokens(t *testing.T, gw *Gateway) (in, out int64) {
	t.Helper()
	in, _ = gw.limiter.getCounter("user", "alice", metricTokIn, windowDay, time.Now())
	out, _ = gw.limiter.getCounter("user", "alice", metricTokOut, windowDay, time.Now())
	return in, out
}

var openAITextAndToolFrames = []string{
	"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n",
	"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Let me \"}}]}\n\n",
	"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"check.\"}}]}\n\n",
	"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_7\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\n",
	"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}]}}]}\n\n",
	"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
	"data: {\"id\":\"chatcmpl-9\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\n",
	"data: [DONE]\n\n",
}

func TestHandleMessages_Stream_OpenAIProvider_Translates(t *testing.T) {
	var got map[string]any
	srv := sseUpstream(t, &got, nil, openAITextAndToolFrames...)
	defer srv.Close()
	gw := newMessagesTestGateway(t, "", srv.URL)

	rec := serveMessages(t, gw, streamMessagesBody("gpt-test"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if got["stream"] != true {
		t.Errorf("upstream stream = %v, want true", got["stream"])
	}
	if so, _ := got["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("upstream stream_options = %v, want include_usage true", got["stream_options"])
	}

	evs := parseAnthropicEvents(t, rec.Body.String())
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if names := eventNames(evs); !equalStrings(names, want) {
		t.Fatalf("events = %v, want %v", names, want)
	}
	msg := sub(evs[0].data, "message")
	if msg["model"] != "gpt-test" || msg["role"] != "assistant" {
		t.Errorf("message_start message = %v, want model gpt-test role assistant", msg)
	}
	text := sub(evs[2].data, "delta")["text"].(string) + sub(evs[3].data, "delta")["text"].(string)
	if text != "Let me check." {
		t.Errorf("text = %q", text)
	}
	tool := sub(evs[5].data, "content_block")
	if tool["type"] != "tool_use" || tool["id"] != "call_7" || tool["name"] != "get_weather" {
		t.Errorf("tool block = %v", tool)
	}
	if pj := sub(evs[6].data, "delta")["partial_json"]; pj != `{"city":"Paris"}` {
		t.Errorf("partial_json = %v", pj)
	}
	if d := sub(evs[8].data, "delta"); d["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", d["stop_reason"])
	}
	if u := sub(evs[8].data, "usage"); u["output_tokens"] != float64(7) || u["input_tokens"] != float64(11) {
		t.Errorf("message_delta usage = %v, want input 11 output 7", u)
	}
	if in, out := userTokens(t, gw); in != 11 || out != 7 {
		t.Errorf("billed tokens in/out = %d/%d, want 11/7", in, out)
	}
}

const anthropicStreamUpstream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-real-20260101","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"cache_creation_input_tokens":100,"cache_read_input_tokens":50,"output_tokens":1}}}` + "\n\n" +
	"event: ping\n" + `data: {"type":"ping"}` + "\n\n" +
	"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi there"}}` + "\n\n" +
	"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":6}}` + "\n\n" +
	"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"

func TestHandleMessages_Stream_AnthropicProvider_Relays(t *testing.T) {
	var got map[string]any
	srv := sseUpstream(t, &got, nil, anthropicStreamUpstream)
	defer srv.Close()
	gw := newMessagesTestGateway(t, srv.URL, "")

	rec := serveMessages(t, gw, streamMessagesBody("claude-test"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got["stream"] != true {
		t.Errorf("upstream stream = %v, want true", got["stream"])
	}
	wantBody := strings.Replace(anthropicStreamUpstream, `"model":"claude-real-20260101"`, `"model":"claude-test"`, 1)
	if rec.Body.String() != wantBody {
		t.Fatalf("relayed body differs from upstream with only the model rewritten:\n got: %q\nwant: %q", rec.Body.String(), wantBody)
	}
	if in, out := userTokens(t, gw); in != 162 || out != 6 {
		t.Errorf("billed tokens in/out = %d/%d, want 162/6 (12 + 100 cache write + 50 cache read; output 6)", in, out)
	}
}

func TestHandleMessages_Stream_NeverCached(t *testing.T) {
	var calls int32
	srv := sseUpstream(t, nil, &calls, openAITextAndToolFrames...)
	defer srv.Close()

	cfg := CreateConfig()
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m"}
	cacheOn := true
	cfg.Providers = map[string]*ProviderConfig{"openai": {Type: providerTypeOpenAI, BaseURL: srv.URL, APIKey: "sk-oai-up", Models: []string{"gpt-test"}}}
	cfg.Groups = map[string]*GroupConfig{"default": {Cache: &cacheOn}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice"}}}
	h, err := New(context.Background(), http.NotFoundHandler(), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	gw := h.(*Gateway)

	for i := 0; i < 2; i++ {
		rec := serveMessages(t, gw, streamMessagesBody("gpt-test"))
		if rec.Code != http.StatusOK || rec.Header().Get("X-Llmgw-Cache") == "hit" {
			t.Fatalf("request %d: status %d cache %q", i, rec.Code, rec.Header().Get("X-Llmgw-Cache"))
		}
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("upstream calls = %d, want 2: a stream must never be served from cache", n)
	}
}

func TestHandleMessages_Stream_PreStreamErrorIsJSON(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad prompt","type":"invalid_request_error"}}`)
	}))
	defer srv.Close()
	gw := newMessagesTestGateway(t, "", srv.URL)

	rec := serveMessages(t, gw, streamMessagesBody("gpt-test"))
	if rec.Code < 400 {
		t.Fatalf("status = %d, want an error status", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json before the stream starts", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["type"] != "error" {
		t.Fatalf("body = %s, want an Anthropic error object", rec.Body.String())
	}
	if atomic.LoadInt32(&calls) == 0 || !strings.Contains(rec.Body.String(), "bad prompt") {
		t.Fatalf("body = %s, want the upstream's own error after a real upstream call", rec.Body.String())
	}
}

func TestHandleMessages_Stream_MidStreamErrorEvent_Translated(t *testing.T) {
	srv := sseUpstream(t, nil, nil,
		"data: {\"id\":\"chatcmpl-9\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n",
		"data: {not json\n\n",
	)
	defer srv.Close()
	gw := newMessagesTestGateway(t, "", srv.URL)

	rec := serveMessages(t, gw, streamMessagesBody("gpt-test"))
	evs := parseAnthropicEvents(t, rec.Body.String())
	names := eventNames(evs)
	if len(names) == 0 || names[len(names)-1] != "error" {
		t.Fatalf("events = %v, want the stream to end with an error event", names)
	}
	if e := sub(evs[len(evs)-1].data, "error"); e["type"] != anthropicStreamErrorType {
		t.Errorf("error = %v, want type %s", e, anthropicStreamErrorType)
	}
	for _, n := range names {
		if n == "message_stop" {
			t.Fatal("a failed stream must not send message_stop")
		}
	}
}

func TestHandleMessages_Stream_MidStreamErrorEvent_AnthropicRelayedOnce(t *testing.T) {
	srv := sseUpstream(t, nil, nil,
		"event: message_start\n"+`data: {"type":"message_start","message":{"id":"msg_02","type":"message","role":"assistant","model":"claude-real","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}`+"\n\n",
		"event: error\n"+`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`+"\n\n",
	)
	defer srv.Close()
	gw := newMessagesTestGateway(t, srv.URL, "")

	rec := serveMessages(t, gw, streamMessagesBody("claude-test"))
	evs := parseAnthropicEvents(t, rec.Body.String())
	if names := eventNames(evs); !equalStrings(names, []string{"message_start", "error"}) {
		t.Fatalf("events = %v, want [message_start error]", names)
	}
	if e := sub(evs[1].data, "error"); e["type"] != "overloaded_error" {
		t.Errorf("error = %v, want the upstream overloaded_error relayed as is", e)
	}
}

func TestHandleMessages_StreamingShapesAllStream(t *testing.T) {
	for _, stream := range []any{true, "true", float64(1)} {
		var got map[string]any
		srv := sseUpstream(t, &got, nil, anthropicStreamUpstream)
		gw := newMessagesTestGateway(t, srv.URL, "")
		body := streamMessagesBody("claude-test")
		body["stream"] = stream
		rec := serveMessages(t, gw, body)
		srv.Close()
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
			t.Errorf("stream=%v: status %d Content-Type %q, want a 200 event stream", stream, rec.Code, rec.Header().Get("Content-Type"))
		}
		if got["stream"] != true {
			t.Errorf("stream=%v: upstream stream = %v, want true", stream, got["stream"])
		}
	}
}
