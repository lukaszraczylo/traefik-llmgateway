package traefikllmgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func cloneJSON(t *testing.T, v map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func lastBlockHasMarker(t *testing.T, content any) bool {
	t.Helper()
	blocks, ok := content.([]any)
	if !ok || len(blocks) == 0 {
		return false
	}
	m, _ := blocks[len(blocks)-1].(map[string]any)
	cc, _ := m["cache_control"].(map[string]any)
	return cc["type"] == "ephemeral"
}

func fiveMessageBody() map[string]any {
	return map[string]any{
		"model":  "claude-x",
		"system": "sys",
		"tools":  []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "u0"},
			map[string]any{"role": "assistant", "content": "a1"},
			map[string]any{"role": "user", "content": "u2"},
			map[string]any{"role": "assistant", "content": "a3"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "u4"}}},
		},
	}
}

func TestInjectCacheControlFourBreakpoints(t *testing.T) {
	out, n := injectCacheControl(fiveMessageBody())
	if n != 4 {
		t.Fatalf("added = %d, want 4", n)
	}
	tools := out["tools"].([]any)
	if _, ok := tools[0].(map[string]any)["cache_control"]; ok {
		t.Fatal("first tool marked")
	}
	if tools[1].(map[string]any)["cache_control"].(map[string]any)["type"] != "ephemeral" {
		t.Fatal("last tool not marked")
	}
	if !lastBlockHasMarker(t, out["system"]) {
		t.Fatalf("system not marked: %#v", out["system"])
	}
	msgs := out["messages"].([]any)
	for i, want := range []bool{false, false, true, false, true} {
		got := lastBlockHasMarker(t, msgs[i].(map[string]any)["content"])
		if got != want {
			t.Fatalf("message %d marked = %v, want %v", i, got, want)
		}
	}
}

func TestInjectCacheControlRollsForward(t *testing.T) {
	out1, _ := injectCacheControl(fiveMessageBody())
	_ = out1
	b := fiveMessageBody()
	msgs := b["messages"].([]any)
	b["messages"] = append(msgs,
		map[string]any{"role": "assistant", "content": "a5"},
		map[string]any{"role": "user", "content": "u6"})
	out2, n := injectCacheControl(b)
	if n != 4 {
		t.Fatalf("added = %d", n)
	}
	m2 := out2["messages"].([]any)
	if !lastBlockHasMarker(t, m2[4].(map[string]any)["content"]) {
		t.Fatal("marker 3 not on the former marker-4 message")
	}
	if !lastBlockHasMarker(t, m2[6].(map[string]any)["content"]) {
		t.Fatal("marker 4 not on the new last user message")
	}
	if lastBlockHasMarker(t, m2[2].(map[string]any)["content"]) {
		t.Fatal("old marker 3 still present")
	}
}

func TestInjectCacheControlSkipsWhenClientMarked(t *testing.T) {
	b := fiveMessageBody()
	b["messages"].([]any)[2] = map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "u2", "cache_control": map[string]any{"type": "ephemeral"}},
	}}
	want := cloneJSON(t, b)
	out, n := injectCacheControl(b)
	if n != 0 {
		t.Fatalf("added = %d, want 0", n)
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatal("body changed")
	}
}

func TestInjectCacheControlLeavesInputUntouched(t *testing.T) {
	b := fiveMessageBody()
	want := cloneJSON(t, b)
	out, n := injectCacheControl(b)
	if n == 0 {
		t.Fatal("expected markers")
	}
	if !reflect.DeepEqual(b, want) {
		t.Fatal("input mutated")
	}
	if reflect.DeepEqual(out, want) {
		t.Fatal("output equals input")
	}
}

func TestInjectCacheControlNoToolsNoSystem(t *testing.T) {
	b := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	out, n := injectCacheControl(b)
	if n != 1 {
		t.Fatalf("added = %d, want 1", n)
	}
	if !lastBlockHasMarker(t, out["messages"].([]any)[0].(map[string]any)["content"]) {
		t.Fatal("user message not marked")
	}
	if _, ok := out["system"]; ok {
		t.Fatal("system created")
	}
}

func TestPromptCacheConfigValidation(t *testing.T) {
	cases := map[string]*ProviderConfig{
		"inject-on-openai": {Type: providerTypeOpenAI, BaseURL: "http://x", PromptCache: &PromptCacheConfig{Inject: true}},
		"key-on-anthropic": {Type: providerTypeAnthropic, BaseURL: "http://x", APIKey: "k", PromptCache: &PromptCacheConfig{Key: true}},
	}
	for name, pc := range cases {
		cfg := CreateConfig()
		cfg.Providers = map[string]*ProviderConfig{"prov": pc}
		_, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
		if err == nil || !strings.Contains(err.Error(), `"prov"`) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestAnthropicAdapterInjectsOnlyWhenEnabled(t *testing.T) {
	run := func(inject bool) string {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		}))
		defer srv.Close()
		a, err := newAnthropicAdapter("anthropic", srv.URL, "sk-ant")
		if err != nil {
			t.Fatal(err)
		}
		a.cacheInject = inject
		req := map[string]any{"model": "claude-test", "messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "hi"},
		}}
		if _, err := a.chatCompletion(context.Background(), httptest.NewRecorder(), req); err != nil {
			t.Fatal(err)
		}
		return got
	}
	on, off := run(true), run(false)
	if strings.Contains(off, "cache_control") {
		t.Fatalf("disabled body has markers: %s", off)
	}
	if strings.Count(on, `"cache_control":{"type":"ephemeral"}`) != 2 {
		t.Fatalf("enabled body = %s", on)
	}
}

func TestMessagesPassthroughInjects(t *testing.T) {
	run := func(inject bool) string {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		}))
		defer srv.Close()
		gw := newMessagesTestGateway(t, srv.URL, "")
		if inject {
			gw.cfg.Providers["anthropic"].PromptCache = &PromptCacheConfig{Inject: true}
		}
		body := map[string]any{"model": "claude-test", "max_tokens": 10, "system": "sys", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, newMessagesRequest(t, body, "Authorization", "Bearer sk-alice"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		return got
	}
	on, off := run(true), run(false)
	if strings.Contains(off, "cache_control") {
		t.Fatalf("disabled body has markers: %s", off)
	}
	if strings.Count(on, `"cache_control":{"type":"ephemeral"}`) != 2 {
		t.Fatalf("enabled body = %s", on)
	}
}
