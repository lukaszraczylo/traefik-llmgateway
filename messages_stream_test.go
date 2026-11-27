package traefikllmgateway

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// anthEvent is one decoded Anthropic SSE event: its name and JSON payload.
type anthEvent struct {
	data map[string]any
	name string
}

func parseAnthropicEvents(t *testing.T, body string) []anthEvent {
	t.Helper()
	var out []anthEvent
	err := readSSE(strings.NewReader(body), func(ev sseEvent) error {
		var m map[string]any
		if err := json.Unmarshal(ev.data, &m); err != nil {
			t.Fatalf("event %q data is not JSON: %v (%s)", ev.event, err, ev.data)
		}
		if m["type"] != ev.event {
			t.Errorf("event name %q != data.type %v", ev.event, m["type"])
		}
		out = append(out, anthEvent{name: ev.event, data: m})
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	return out
}

func eventNames(evs []anthEvent) []string {
	names := make([]string, len(evs))
	for i, e := range evs {
		names[i] = e.name
	}
	return names
}

func sub(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func oaiFrame(payload string) []byte { return []byte("data: " + payload + "\n\n") }

// runStreamWriter feeds frames into a fresh translator and finishes it.
func runStreamWriter(t *testing.T, u usage, frames ...string) (*httptest.ResponseRecorder, []anthEvent) {
	t.Helper()
	rec := httptest.NewRecorder()
	sw := newAnthropicStreamWriter(rec, "my-alias")
	for _, f := range frames {
		if _, err := sw.Write(oaiFrame(f)); err != nil {
			t.Fatalf("Write(%s): %v", f, err)
		}
	}
	if err := sw.finish(u); err != nil {
		t.Fatalf("finish: %v", err)
	}
	return rec, parseAnthropicEvents(t, rec.Body.String())
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAnthropicStreamWriter_TextOnly(t *testing.T) {
	rec, evs := runStreamWriter(t, usage{prompt: 11, completion: 3},
		`{"id":"chatcmpl-abc","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"id":"chatcmpl-abc","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`{"id":"chatcmpl-abc","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"id":"chatcmpl-abc","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl-abc","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3}}`,
	)

	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := eventNames(evs); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	msg := sub(evs[0].data, "message")
	if msg["id"] != "msg_abc" || msg["type"] != "message" || msg["role"] != "assistant" || msg["model"] != "my-alias" {
		t.Errorf("message_start.message = %v", msg)
	}
	if c, ok := msg["content"].([]any); !ok || len(c) != 0 {
		t.Errorf("message_start content = %v, want empty array", msg["content"])
	}
	if _, ok := sub(msg, "usage")["input_tokens"]; !ok {
		t.Errorf("message_start usage lacks input_tokens: %v", msg["usage"])
	}

	if cb := sub(evs[1].data, "content_block"); cb["type"] != "text" || cb["text"] != "" || evs[1].data["index"] != float64(0) {
		t.Errorf("content_block_start = %v", evs[1].data)
	}
	if d := sub(evs[2].data, "delta"); d["type"] != "text_delta" || d["text"] != "Hel" {
		t.Errorf("first delta = %v", evs[2].data)
	}
	if d := sub(evs[3].data, "delta"); d["text"] != "lo" {
		t.Errorf("second delta = %v", evs[3].data)
	}
	if evs[4].data["index"] != float64(0) {
		t.Errorf("content_block_stop index = %v", evs[4].data["index"])
	}
	md := evs[5].data
	if d := sub(md, "delta"); d["stop_reason"] != "end_turn" {
		t.Errorf("message_delta delta = %v", d)
	}
	if u := sub(md, "usage"); u["output_tokens"] != float64(3) || u["input_tokens"] != float64(11) {
		t.Errorf("message_delta usage = %v", u)
	}
}

func TestAnthropicStreamWriter_ToolUse(t *testing.T) {
	_, evs := runStreamWriter(t, usage{prompt: 20, completion: 9},
		`{"id":"chatcmpl-t","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"id":"chatcmpl-t","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		`{"id":"chatcmpl-t","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]}}]}`,
		`{"id":"chatcmpl-t","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := eventNames(evs); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	cb := sub(evs[1].data, "content_block")
	if cb["type"] != "tool_use" || cb["id"] != "call_1" || cb["name"] != "get_weather" {
		t.Errorf("content_block = %v", cb)
	}
	if in, ok := cb["input"].(map[string]any); !ok || len(in) != 0 {
		t.Errorf("tool_use start input = %v, want {}", cb["input"])
	}
	var args string
	for _, e := range evs[2:4] {
		d := sub(e.data, "delta")
		if d["type"] != "input_json_delta" {
			t.Errorf("delta type = %v", d["type"])
		}
		args += d["partial_json"].(string)
	}
	if args != `{"city":"Paris"}` {
		t.Errorf("reassembled arguments = %q", args)
	}
	if d := sub(evs[5].data, "delta"); d["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", d["stop_reason"])
	}
}

func TestAnthropicStreamWriter_TwoToolCalls(t *testing.T) {
	_, evs := runStreamWriter(t, usage{},
		`{"id":"c","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"a","arguments":""}}]}}]}`,
		`{"id":"c","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`{"id":"c","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"b","arguments":""}}]}}]}`,
		`{"id":"c","choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"x\":1}"}}]}}]}`,
		`{"id":"c","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if got := eventNames(evs); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if evs[1].data["index"] != float64(0) || evs[2].data["index"] != float64(0) || evs[3].data["index"] != float64(0) {
		t.Errorf("first block indexes wrong: %v %v %v", evs[1].data, evs[2].data, evs[3].data)
	}
	if evs[4].data["index"] != float64(1) || evs[5].data["index"] != float64(1) || evs[6].data["index"] != float64(1) {
		t.Errorf("second block indexes wrong: %v %v %v", evs[4].data, evs[5].data, evs[6].data)
	}
	if cb := sub(evs[4].data, "content_block"); cb["id"] != "call_b" || cb["name"] != "b" {
		t.Errorf("second tool block = %v", cb)
	}
}

func TestAnthropicStreamWriter_TextThenToolCall(t *testing.T) {
	_, evs := runStreamWriter(t, usage{},
		`{"id":"c","choices":[{"delta":{"content":"Checking."}}]}`,
		`{"id":"c","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"lookup","arguments":"{}"}}]}}]}`,
		`{"id":"c","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if got := eventNames(evs); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if cb := sub(evs[1].data, "content_block"); cb["type"] != "text" {
		t.Errorf("block 0 = %v, want text", cb)
	}
	if cb := sub(evs[4].data, "content_block"); cb["type"] != "tool_use" || evs[4].data["index"] != float64(1) {
		t.Errorf("block 1 = %v (index %v), want tool_use at 1", cb, evs[4].data["index"])
	}
}

func TestAnthropicStreamWriter_StopReasonMatchesNonStreaming(t *testing.T) {
	for _, finish := range []string{"stop", "length", "tool_calls", "content_filter", "function_call", ""} {
		frames := []string{`{"id":"c","choices":[{"delta":{"content":"x"}}]}`}
		if finish != "" {
			frames = append(frames, `{"id":"c","choices":[{"delta":{},"finish_reason":"`+finish+`"}]}`)
		}
		_, evs := runStreamWriter(t, usage{}, frames...)
		got := sub(evs[len(evs)-2].data, "delta")["stop_reason"]
		if want := anthropicStopReasonFromOpenAI(finish); got != want {
			t.Errorf("finish_reason %q: stop_reason = %v, want %v", finish, got, want)
		}
	}
}

func TestAnthropicStreamWriter_UsageFromFinish(t *testing.T) {
	_, evs := runStreamWriter(t, usage{prompt: 123, completion: 45},
		`{"id":"c","choices":[{"delta":{"content":"x"}}]}`,
	)
	u := sub(evs[len(evs)-2].data, "usage")
	if u["input_tokens"] != float64(123) || u["output_tokens"] != float64(45) {
		t.Errorf("message_delta usage = %v, want input 123 output 45", u)
	}
}

func TestAnthropicStreamWriter_EmptyStreamStillWellFormed(t *testing.T) {
	_, evs := runStreamWriter(t, usage{})
	want := []string{"message_start", "message_delta", "message_stop"}
	if got := eventNames(evs); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestAnthropicStreamWriter_DoneAndKeepAliveIgnored(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newAnthropicStreamWriter(rec, "a")
	if _, err := sw.Write([]byte(": keepalive\n\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Write(oaiFrame(`{"id":"c","choices":[{"delta":{"content":"x"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Write(oaiFrame("[DONE]")); err != nil {
		t.Fatal(err)
	}
	if err := sw.finish(usage{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.Body.String(), "DONE") {
		t.Errorf("[DONE] leaked into the Anthropic stream: %s", rec.Body.String())
	}
}

func TestAnthropicStreamWriter_UpstreamErrorChunkFailsStream(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newAnthropicStreamWriter(rec, "a")
	_, err := sw.Write(oaiFrame(`{"error":{"message":"boom","type":"server_error"}}`))
	if err == nil {
		t.Fatal("Write returned nil for an upstream error chunk, want an error")
	}
}

func TestAnthropicStreamWriter_MalformedChunkFailsStream(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newAnthropicStreamWriter(rec, "a")
	if _, err := sw.Write(oaiFrame(`{not json`)); err == nil {
		t.Fatal("Write returned nil for malformed JSON, want an error")
	}
}

func TestAnthropicStreamWriter_JSONFallbackWhenUpstreamIgnoresStream(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newAnthropicStreamWriter(rec, "my-alias")
	sw.Header().Set("Content-Type", "application/json")
	sw.WriteHeader(200)
	body := `{"id":"chatcmpl-j","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2}}`
	if _, err := sw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := sw.finish(usage{}); err != nil {
		t.Fatal(err)
	}
	evs := parseAnthropicEvents(t, rec.Body.String())
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if got := eventNames(evs); !equalStrings(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if d := sub(evs[2].data, "delta"); d["text"] != "hi" {
		t.Errorf("text delta = %v", d)
	}
	if u := sub(evs[4].data, "usage"); u["output_tokens"] != float64(2) {
		t.Errorf("usage = %v", u)
	}
}

func TestRewriteMessageStartModel_PreservesOtherBytes(t *testing.T) {
	in := []byte(`{"type":"message_start","message":{"id":"msg_1","model":"claude-real","big":12345678901234567890,"usage":{"input_tokens":3}}}`)
	got, err := rewriteMessageStartModel(in, "alias")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"message_start","message":{"id":"msg_1","model":"alias","big":12345678901234567890,"usage":{"input_tokens":3}}}`
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if _, err := rewriteMessageStartModel([]byte(`{"type":"message_start"}`), "a"); err == nil {
		t.Error("want error when message object is missing")
	}
}
