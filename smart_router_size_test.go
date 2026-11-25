package traefikllmgateway

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSmartPromptSize_CountsToolPayloads(t *testing.T) {
	t.Parallel()
	blob := strings.Repeat("x", 4000)
	tests := []struct {
		req      map[string]any
		name     string
		endpoint string
		minChars int
		maxChars int
	}{
		{
			name:     "openai tool_calls string arguments",
			req:      chatMsgs(map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "f", "arguments": blob}}}}),
			endpoint: cacheEndpointChat,
			minChars: 4000, maxChars: 4100,
		},
		{
			name:     "openai tool_calls object arguments",
			req:      chatMsgs(map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{"arguments": map[string]any{"q": blob}}}}}),
			endpoint: cacheEndpointChat,
			minChars: 4000, maxChars: 4100,
		},
		{
			name:     "anthropic tool_use input",
			req:      chatMsgs(msg("assistant", []any{map[string]any{"type": "tool_use", "id": "t1", "name": "f", "input": map[string]any{"code": blob}}})),
			endpoint: cacheEndpointMessages,
			minChars: 4000, maxChars: 4100,
		},
		{
			name:     "tools schema on chat",
			req:      map[string]any{"messages": []any{}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "f", "description": blob, "parameters": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}}}}}},
			endpoint: cacheEndpointChat,
			minChars: 4000, maxChars: 4300,
		},
		{
			name:     "tools schema on messages",
			req:      map[string]any{"messages": []any{}, "tools": []any{map[string]any{"name": "f", "description": blob, "input_schema": map[string]any{"type": "object"}}}},
			endpoint: cacheEndpointMessages,
			minChars: 4000, maxChars: 4200,
		},
		{
			name:     "no tool payloads add nothing",
			req:      chatMsgs(msg("user", "hello")),
			endpoint: cacheEndpointChat,
			minChars: 5, maxChars: 5,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			chars, _ := smartPromptSize(tc.req, tc.endpoint)
			if chars < tc.minChars || chars > tc.maxChars {
				t.Errorf("chars = %d, want in [%d, %d]", chars, tc.minChars, tc.maxChars)
			}
		})
	}
}

func TestSmartPromptSize_ToolSizeTracksSerializedLength(t *testing.T) {
	t.Parallel()
	input := map[string]any{"path": "/etc/hosts", "opts": map[string]any{"deep": []any{"a", "bb", true, nil, 12.5}}}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	got := smartValueSize(input)
	if got < len(raw)*7/10 || got > len(raw)*13/10 {
		t.Errorf("smartValueSize = %d, serialized length = %d: want within 30%%", got, len(raw))
	}
}

func TestSmartValueSize_BoundedWork(t *testing.T) {
	t.Parallel()
	deep := any("leaf")
	for i := 0; i < 1000; i++ {
		deep = []any{deep}
	}
	if got := smartValueSize(deep); got <= 0 || got > 2*smartSizeMaxDepth+16 {
		t.Errorf("deep nesting size = %d, want a small bounded value", got)
	}
	wide := make([]any, 100000)
	for i := range wide {
		wide[i] = "abcd"
	}
	if got := smartValueSize(wide); got > (smartSizeMaxNodes+1)*8 {
		t.Errorf("wide array size = %d, want capped by the node budget", got)
	}
}

func TestSmartPromptBytes_IncludesToolsSchema(t *testing.T) {
	t.Parallel()
	base := chatMsgs(msg("user", "hello"))
	withTools := map[string]any{
		"messages": base["messages"],
		"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "f", "description": strings.Repeat("d", 8000)}}},
	}
	baseBytes, _ := smartPromptBytes(base, cacheEndpointChat)
	toolBytes, _ := smartPromptBytes(withTools, cacheEndpointChat)
	if diff := toolBytes - baseBytes; diff < 8000 {
		t.Errorf("tools schema added %d bytes, want >= 8000", diff)
	}
}
