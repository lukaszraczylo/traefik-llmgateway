package traefikllmgateway

import (
	"reflect"
	"testing"
)

func TestTranslateSystemKeepsCacheControl(t *testing.T) {
	cc := map[string]any{"type": "ephemeral"}
	req := map[string]any{
		"model": "claude-x",
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "A"},
				map[string]any{"type": "text", "text": "B", "cache_control": cc},
			}},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	out, err := anthropicRequestFromOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"type": "text", "text": "A"},
		map[string]any{"type": "text", "text": "B", "cache_control": map[string]any{"type": "ephemeral"}},
	}
	if !reflect.DeepEqual(out["system"], want) {
		t.Fatalf("system = %#v, want %#v", out["system"], want)
	}
	if _, ok := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)["cache_control"]; !ok {
		t.Fatal("input mutated")
	}
}

func TestTranslateSystemNoMarkersUnchanged(t *testing.T) {
	req := map[string]any{
		"model": "claude-x",
		"messages": []any{
			map[string]any{"role": "system", "content": "A"},
			map[string]any{"role": "developer", "content": "B"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	out, err := anthropicRequestFromOpenAI(req)
	if err != nil {
		t.Fatal(err)
	}
	if out["system"] != "A\n\nB" {
		t.Fatalf("system = %#v", out["system"])
	}
}

func systemReq(parts ...any) map[string]any {
	return map[string]any{
		"model": "claude-x",
		"messages": []any{
			map[string]any{"role": "system", "content": parts},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
}

func TestTranslateSystemEmptyMarkedPartKeepsString(t *testing.T) {
	cc := map[string]any{"type": "ephemeral"}
	out, err := anthropicRequestFromOpenAI(systemReq(
		map[string]any{"type": "text", "text": "", "cache_control": cc}))
	if err != nil {
		t.Fatal(err)
	}
	if out["system"] != "" {
		t.Fatalf("system = %#v, want empty string", out["system"])
	}
}

func TestTranslateSystemEmptyMarkedPartWithOthers(t *testing.T) {
	cc := map[string]any{"type": "ephemeral"}
	out, err := anthropicRequestFromOpenAI(systemReq(
		map[string]any{"type": "text", "text": "A"},
		map[string]any{"type": "text", "text": "", "cache_control": cc},
		map[string]any{"type": "text", "text": "B"}))
	if err != nil {
		t.Fatal(err)
	}
	if out["system"] != "A\n\nB" {
		t.Fatalf("system = %#v", out["system"])
	}
}
