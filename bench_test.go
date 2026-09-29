package traefikllmgateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const benchStreamDeltas = 500

func benchOpenAIRequest() map[string]any {
	return map[string]any{
		"model":       "bench-model",
		"max_tokens":  float64(1024),
		"temperature": 0.7,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "What is the weather in Paris and Berlin?"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"Paris"}`}},
				map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"Berlin"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "18C, cloudy"},
			map[string]any{"role": "tool", "tool_call_id": "call_2", "content": "15C, rain"},
			map[string]any{"role": "user", "content": "Summarise both in one sentence."},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name":        "get_weather",
				"description": "Get the weather for a city",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string"}},
					"required":   []any{"city"},
				},
			}},
		},
		"tool_choice": "auto",
	}
}

func benchAnthropicStream() []byte {
	var sb strings.Builder
	sb.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01B\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n")
	sb.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	for i := 0; i < benchStreamDeltas; i++ {
		fmt.Fprintf(&sb, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" token%d\"}}\n\n", i)
	}
	sb.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	sb.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":500}}\n\n")
	sb.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return []byte(sb.String())
}

func benchGeminiStream() []byte {
	var sb strings.Builder
	for i := 0; i < benchStreamDeltas; i++ {
		fmt.Fprintf(&sb, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\" token%d\"}]}}],\"responseId\":\"resp_B\"}\n\n", i)
	}
	sb.WriteString("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":500}}\n\n")
	return []byte(sb.String())
}

func BenchmarkAnthropicRequestFromOpenAI(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := anthropicRequestFromOpenAI(benchOpenAIRequest()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGeminiRequestFromOpenAI(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := geminiRequestFromOpenAI(benchOpenAIRequest()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpenAIResponseFromAnthropic(b *testing.B) {
	body := []byte(`{"id":"msg_01XYZ","type":"message","role":"assistant","content":[{"type":"text","text":"The capital of France is Paris."},{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"Paris"}}],"model":"claude-opus-5","stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":8}}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if _, _, err := openAIResponseFromAnthropic(body, "gw-model", 1734000000); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOpenAIResponseFromGemini(b *testing.B) {
	body := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"The capital of France is Paris."},{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8},"responseId":"resp_1"}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if _, _, err := openAIResponseFromGemini(body, "gw-model", 1734000000); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadSSE(b *testing.B) {
	stream := benchAnthropicStream()
	b.ReportAllocs()
	b.SetBytes(int64(len(stream)))
	for i := 0; i < b.N; i++ {
		if err := readSSE(bytes.NewReader(stream), func(sseEvent) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAnthropicStreamTranslate(b *testing.B) {
	stream := benchAnthropicStream()
	b.ReportAllocs()
	b.SetBytes(int64(len(stream)))
	for i := 0; i < b.N; i++ {
		st := newAnthropicStreamState("gw-model", 1734000000)
		err := readSSE(bytes.NewReader(stream), func(ev sseEvent) error {
			_, terr := st.translate(ev)
			return terr
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGeminiStreamTranslate(b *testing.B) {
	stream := benchGeminiStream()
	b.ReportAllocs()
	b.SetBytes(int64(len(stream)))
	for i := 0; i < b.N; i++ {
		st := newGeminiStreamState("gw-model", 1734000000)
		err := readSSE(bytes.NewReader(stream), func(ev sseEvent) error {
			_, terr := st.translate(ev)
			return terr
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func newBenchGateway(b *testing.B, provider *ProviderConfig) http.Handler {
	b.Helper()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatalf("open %s: %v", os.DevNull, err)
	}
	origStderr := os.Stderr
	os.Stderr = devNull
	b.Cleanup(func() {
		os.Stderr = origStderr
		_ = devNull.Close()
	})
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"bench": provider}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	return h
}

func benchChatRequest(payload []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func BenchmarkUnifiedChatOpenAI(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"bench-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	b.Cleanup(upstream.Close)
	h := newBenchGateway(b, &ProviderConfig{Type: "openai", BaseURL: upstream.URL, APIKey: "k", Models: []string{"bench-model"}})
	payload := []byte(`{"model":"bench-model","messages":[{"role":"user","content":"hi"}]}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, benchChatRequest(payload))
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
}

func BenchmarkUnifiedChatAnthropicStream(b *testing.B) {
	stream := benchAnthropicStream()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(stream)
	}))
	b.Cleanup(upstream.Close)
	h := newBenchGateway(b, &ProviderConfig{Type: "anthropic", BaseURL: upstream.URL, APIKey: "k", Models: []string{"bench-model"}})
	payload := []byte(`{"model":"bench-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	b.ReportAllocs()
	b.SetBytes(int64(len(stream)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, benchChatRequest(payload))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
			b.Fatalf("status = %d, body tail = %.200s", rec.Code, rec.Body.String())
		}
	}
}
