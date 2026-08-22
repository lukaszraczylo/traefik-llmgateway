package traefikllmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// goldenTestdataDir is where every fixture this file walks lives.
const goldenTestdataDir = "testdata/anthropic"

// goldenGatewayModel and goldenCreated are the fixed model/created values
// every response- and stream-translation golden fixture's "_want.json"
// bakes in — openAIResponseFromAnthropic and newAnthropicStreamState both
// take these as caller-supplied parameters (Anthropic's own wire formats
// carry neither), so the golden tests must supply the same fixed values
// the "_want.json" files were written against.
const (
	goldenGatewayModel = "gw-anthropic-model"
	goldenCreated      = int64(1734000000)
)

// normalizeJSON round-trips b through json.Unmarshal into a generic
// map[string]any (or []any), so a comparison via reflect.DeepEqual is not
// tripped up by Go-native numeric types (int64, e.g.) that differ from
// the float64 every JSON number decodes to — both v and w.json.Unmarshal
// output use the same generic decode, so both land on float64 either way.
func normalizeJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("normalizeJSON: marshal: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("normalizeJSON: unmarshal: %v", err)
	}
	return out
}

// readGoldenJSON reads and json.Unmarshals name (relative to
// testdata/anthropic) into a fresh map[string]any.
func readGoldenJSON(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(goldenTestdataDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return m
}

// wantErrFixture is the shape a "{name}_wanterr.json" fixture decodes
// into: a substring the produced *translateError.Error() must contain,
// and whether it must set notSupported.
type wantErrFixture struct {
	ErrContains  string `json:"errContains"`
	NotSupported bool   `json:"notSupported"`
}

// assertTranslateError asserts err is a *translateError matching want.
func assertTranslateError(t *testing.T, err error, want wantErrFixture) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a *translateError containing %q, got nil", want.ErrContains)
	}
	var terr *translateError
	if !errors.As(err, &terr) {
		t.Fatalf("err = %v (%T), want *translateError", err, err)
	}
	if !strings.Contains(terr.Error(), want.ErrContains) {
		t.Errorf("error message = %q, want it to contain %q", terr.Error(), want.ErrContains)
	}
	if terr.notSupported != want.NotSupported {
		t.Errorf("notSupported = %v, want %v", terr.notSupported, want.NotSupported)
	}
}

// TestAnthropicRequestGolden walks every "chat_*_in.json" fixture under
// testdata/anthropic, translating it via anthropicRequestFromOpenAI, and
// asserts the result against the sibling "_want.json" fixture (a
// successful translation) or "_wanterr.json" fixture (a *translateError),
// whichever is present — exactly one of the two exists per fixture.
func TestAnthropicRequestGolden(t *testing.T) {
	entries, err := os.ReadDir(goldenTestdataDir)
	if err != nil {
		t.Fatalf("read testdata dir: %v", err)
	}

	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), "_in.json"); ok && strings.HasPrefix(n, "chat_") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no chat_*_in.json fixtures found under %s", goldenTestdataDir)
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			in := readGoldenJSON(t, name+"_in.json")

			got, err := anthropicRequestFromOpenAI(in)

			wantErrPath := filepath.Join(goldenTestdataDir, name+"_wanterr.json")
			if _, statErr := os.Stat(wantErrPath); statErr == nil {
				var want wantErrFixture
				b, rerr := os.ReadFile(wantErrPath)
				if rerr != nil {
					t.Fatalf("read %s: %v", wantErrPath, rerr)
				}
				if uerr := json.Unmarshal(b, &want); uerr != nil {
					t.Fatalf("decode %s: %v", wantErrPath, uerr)
				}
				assertTranslateError(t, err, want)
				return
			}

			if err != nil {
				t.Fatalf("anthropicRequestFromOpenAI: %v", err)
			}
			want := readGoldenJSON(t, name+"_want.json")
			gotN := normalizeJSON(t, got)
			wantN := normalizeJSON(t, want)
			if !reflect.DeepEqual(gotN, wantN) {
				gotB, _ := json.MarshalIndent(gotN, "", "  ")
				wantB, _ := json.MarshalIndent(wantN, "", "  ")
				t.Errorf("anthropicRequestFromOpenAI(%s) mismatch:\ngot:\n%s\nwant:\n%s", name, gotB, wantB)
			}
		})
	}
}

// TestAnthropicResponseGolden walks every "resp_*_in.json" fixture,
// translating it via openAIResponseFromAnthropic with the fixed
// goldenGatewayModel/goldenCreated, and asserts the result against the
// sibling "_want.json" fixture.
func TestAnthropicResponseGolden(t *testing.T) {
	entries, err := os.ReadDir(goldenTestdataDir)
	if err != nil {
		t.Fatalf("read testdata dir: %v", err)
	}

	var names []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), "_in.json"); ok && strings.HasPrefix(n, "resp_") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no resp_*_in.json fixtures found under %s", goldenTestdataDir)
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(goldenTestdataDir, name+"_in.json"))
			if err != nil {
				t.Fatalf("read %s_in.json: %v", name, err)
			}

			got, _, err := openAIResponseFromAnthropic(body, goldenGatewayModel, goldenCreated)
			if err != nil {
				t.Fatalf("openAIResponseFromAnthropic: %v", err)
			}

			want := readGoldenJSON(t, name+"_want.json")
			gotN := normalizeJSON(t, got)
			wantN := normalizeJSON(t, want)
			if !reflect.DeepEqual(gotN, wantN) {
				gotB, _ := json.MarshalIndent(gotN, "", "  ")
				wantB, _ := json.MarshalIndent(wantN, "", "  ")
				t.Errorf("openAIResponseFromAnthropic(%s) mismatch:\ngot:\n%s\nwant:\n%s", name, gotB, wantB)
			}
		})
	}
}

// streamUsageFixture is the shape a "{name}_usage.json" fixture decodes
// into: the prompt/completion totals anthropicStreamState.usage() must
// report once every event in the sibling ".sse" fixture has been
// translated.
type streamUsageFixture struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
}

// runStreamGolden translates every event in testdata/anthropic/{name}.sse
// through a fresh anthropicStreamState, in order, and asserts the
// flattened list of emitted chunks against "{name}_want.json" and the
// final usage() against "{name}_usage.json".
func runStreamGolden(t *testing.T, name string) {
	t.Helper()

	f, err := os.Open(filepath.Join(goldenTestdataDir, name+".sse"))
	if err != nil {
		t.Fatalf("open %s.sse: %v", name, err)
	}
	defer f.Close() //nolint:errcheck

	st := newAnthropicStreamState(goldenGatewayModel, goldenCreated)
	var got []any
	terminalErr := readSSE(f, func(ev sseEvent) error {
		chunks, terr := st.translate(ev)
		for _, c := range chunks {
			var v any
			if uerr := json.Unmarshal(c, &v); uerr != nil {
				t.Fatalf("translate(%s) produced invalid JSON: %v (%s)", ev.event, uerr, c)
			}
			got = append(got, v)
		}
		return terr
	})
	if terminalErr != nil {
		t.Fatalf("readSSE/translate(%s): %v", name, terminalErr)
	}

	wantBytes, err := os.ReadFile(filepath.Join(goldenTestdataDir, name+"_want.json"))
	if err != nil {
		t.Fatalf("read %s_want.json: %v", name, err)
	}
	var want []any
	if uerr := json.Unmarshal(wantBytes, &want); uerr != nil {
		t.Fatalf("decode %s_want.json: %v", name, uerr)
	}

	if !reflect.DeepEqual(got, want) {
		gotB, _ := json.MarshalIndent(got, "", "  ")
		wantB, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("translate(%s) chunk list mismatch:\ngot:\n%s\nwant:\n%s", name, gotB, wantB)
	}

	usageBytes, err := os.ReadFile(filepath.Join(goldenTestdataDir, name+"_usage.json"))
	if err != nil {
		t.Fatalf("read %s_usage.json: %v", name, err)
	}
	var wantUsage streamUsageFixture
	if err := json.Unmarshal(usageBytes, &wantUsage); err != nil {
		t.Fatalf("decode %s_usage.json: %v", name, err)
	}
	if gotUsage := st.usage(); gotUsage.prompt != wantUsage.Prompt || gotUsage.completion != wantUsage.Completion {
		t.Errorf("usage() = %+v, want {prompt:%d completion:%d}", gotUsage, wantUsage.Prompt, wantUsage.Completion)
	}
}

// TestAnthropicStreamGolden_Text covers a text-only stream: role chunk,
// two content deltas, a finish chunk mapped from end_turn, with a ping
// and a content_block_start/stop interspersed that must produce nothing.
func TestAnthropicStreamGolden_Text(t *testing.T) {
	runStreamGolden(t, "stream_text")
}

// TestAnthropicStreamGolden_ToolUse covers a tool_use stream: role chunk,
// a tool_calls-start chunk, two input_json_delta argument fragments, and
// a finish chunk mapped from tool_use.
func TestAnthropicStreamGolden_ToolUse(t *testing.T) {
	runStreamGolden(t, "stream_tooluse")
}

// TestAnthropicStreamGolden_TextThenTool covers a text block followed by
// a tool_use block (Anthropic content-block index 0 then 1) and proves
// the emitted tool_calls[0].index is the 0-based tool-call ordinal (0),
// not the raw Anthropic content-block index (1) — review fix: OpenAI's
// contract requires tool_calls[].index to be contiguous from 0 over the
// tool-call array, regardless of how many non-tool_use blocks preceded it.
func TestAnthropicStreamGolden_TextThenTool(t *testing.T) {
	runStreamGolden(t, "stream_text_then_tool")
}

// TestAnthropicStreamState_ErrorEvent proves an "error" SSE event
// translates to one OpenAI-shaped {"error":{...}} chunk and a non-nil
// terminal error, per the stream mapping table's error row — built
// inline rather than as a golden fixture since it is the one case
// runStreamGolden's readSSE-drives-to-completion harness cannot express
// (the whole point is that translation stops here).
func TestAnthropicStreamState_ErrorEvent(t *testing.T) {
	st := newAnthropicStreamState(goldenGatewayModel, goldenCreated)
	ev := sseEvent{
		event: "error",
		data:  []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`),
	}

	chunks, err := st.translate(ev)
	if err == nil {
		t.Fatalf("translate(error event): want a terminal error, got nil")
	}
	if len(chunks) != 1 {
		t.Fatalf("translate(error event) returned %d chunks, want 1", len(chunks))
	}

	var payload map[string]any
	if uerr := json.Unmarshal(chunks[0], &payload); uerr != nil {
		t.Fatalf("emitted error chunk is not valid JSON: %v (%s)", uerr, chunks[0])
	}
	errObj, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("emitted error chunk missing \"error\" object: %s", chunks[0])
	}
	if errObj["message"] != "Overloaded" {
		t.Errorf("error.message = %v, want %q", errObj["message"], "Overloaded")
	}
	if errObj["type"] != "overloaded_error" {
		t.Errorf("error.type = %v, want %q", errObj["type"], "overloaded_error")
	}
}

// TestAnthropicStreamState_MalformedEventReturnsError proves a malformed
// (invalid-JSON) data payload on each recognized event type returns an
// error wrapping errUpstream, rather than being swallowed as (nil, nil) —
// review fix: a dropped input_json_delta would otherwise silently corrupt
// the client's reassembled tool-call arguments. The "error" event type
// itself already returned an error before this fix and is covered by
// TestAnthropicStreamState_ErrorEvent, not repeated here.
func TestAnthropicStreamState_MalformedEventReturnsError(t *testing.T) {
	const corrupt = `{"type":"message_start","message":{`

	for _, event := range []string{"message_start", "content_block_start", "content_block_delta", "message_delta"} {
		t.Run(event, func(t *testing.T) {
			st := newAnthropicStreamState(goldenGatewayModel, goldenCreated)
			chunks, err := st.translate(sseEvent{event: event, data: []byte(corrupt)})
			if err == nil {
				t.Fatalf("translate(%s, corrupt data): want error, got nil (chunks=%v)", event, chunks)
			}
			if !errors.Is(err, errUpstream) {
				t.Errorf("translate(%s, corrupt data): err = %v, want errors.Is(err, errUpstream)", event, err)
			}
			if len(chunks) != 0 {
				t.Errorf("translate(%s, corrupt data): chunks = %v, want none", event, chunks)
			}
		})
	}
}

// TestAnthropicListModels_404NotSpeciallyHandled proves listModels
// returns a plain *providerHTTPError on a 404 (an older Anthropic API
// version that predates the Models endpoint) rather than any special
// fallback — the registry, not this adapter, decides what to do with a
// provider's explicitly configured models on a listModels failure.
func TestAnthropicListModels_404NotSpeciallyHandled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
	}))
	defer srv.Close()

	a, err := newAnthropicAdapter("p1", srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}

	_, err = a.listModels(context.Background())
	if err == nil {
		t.Fatalf("listModels: want error on 404, got nil")
	}
	var httpErr *providerHTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v (%T), want *providerHTTPError", err, err)
	}
	if httpErr.status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", httpErr.status)
	}
}

// TestAnthropicAdapter_Embeddings_NotSupported proves embeddings never
// makes an upstream request and always returns a *translateError with
// notSupported set — the caller (Task 12) maps that to HTTP 501.
func TestAnthropicAdapter_Embeddings_NotSupported(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a, err := newAnthropicAdapter("p1", srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}

	_, err = a.embeddings(context.Background(), httptest.NewRecorder(), map[string]any{"model": "claude-opus-5", "input": "hi"})
	var terr *translateError
	if !errors.As(err, &terr) {
		t.Fatalf("err = %v (%T), want *translateError", err, err)
	}
	if !terr.notSupported {
		t.Errorf("notSupported = false, want true")
	}
	if called {
		t.Errorf("embeddings made an upstream request, want none")
	}
}

// TestNewAnthropicAdapter_KeylessIsAConstructorError proves ruling (c):
// an empty resolved API key is a constructor error for anthropic-type
// providers, unlike the openai-type keyless ruling.
func TestNewAnthropicAdapter_KeylessIsAConstructorError(t *testing.T) {
	if _, err := newAnthropicAdapter("p1", "https://api.anthropic.com", ""); err == nil {
		t.Fatalf("newAnthropicAdapter: want error for empty API key, got nil")
	}
}

// TestAnthropicAdapter_InjectAuth proves injectAuth sets both the
// x-api-key and anthropic-version headers unconditionally.
func TestAnthropicAdapter_InjectAuth(t *testing.T) {
	a, err := newAnthropicAdapter("p1", "https://api.anthropic.com", "sk-test")
	if err != nil {
		t.Fatalf("newAnthropicAdapter: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	a.injectAuth(req)

	if got := req.Header.Get("x-api-key"); got != "sk-test" {
		t.Errorf("x-api-key = %q, want %q", got, "sk-test")
	}
	if got := req.Header.Get("anthropic-version"); got != anthropicAPIVersion {
		t.Errorf("anthropic-version = %q, want %q", got, anthropicAPIVersion)
	}
}

// TestAnthropicRequestFromOpenAI_MaxCompletionTokensPrecedence proves
// max_completion_tokens takes precedence over max_tokens when an OpenAI
// request sends both — the newer field wins.
func TestAnthropicRequestFromOpenAI_MaxCompletionTokensPrecedence(t *testing.T) {
	req := map[string]any{
		"model":                 "claude-opus-5",
		"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens":            float64(100),
		"max_completion_tokens": float64(200),
	}
	got, err := anthropicRequestFromOpenAI(req)
	if err != nil {
		t.Fatalf("anthropicRequestFromOpenAI: %v", err)
	}
	if got["max_tokens"] != int64(200) {
		t.Errorf("max_tokens = %v, want 200 (max_completion_tokens takes precedence)", got["max_tokens"])
	}
}

// TestIsTruthy pins isTruthy's truthy-only semantics (per its own doc
// comment): only a present AND non-empty value counts as "the client
// actually asked for this" — nil, false, 0, "", and an empty map/slice
// must not trip a truthy-only unsupported-field check.
func TestIsTruthy(t *testing.T) {
	cases := []struct {
		v    any
		name string
		want bool
	}{
		{name: "nil is falsy", v: nil, want: false},
		{name: "bool true is truthy", v: true, want: true},
		{name: "bool false is falsy", v: false, want: false},
		{name: "nonzero float64 is truthy", v: float64(1.5), want: true},
		{name: "zero float64 is falsy", v: float64(0), want: false},
		{name: "nonempty string is truthy", v: "x", want: true},
		{name: "empty string is falsy", v: "", want: false},
		{name: "nonempty map is truthy", v: map[string]any{"a": 1}, want: true},
		{name: "empty map is falsy", v: map[string]any{}, want: false},
		{name: "nonempty slice is truthy", v: []any{1}, want: true},
		{name: "empty slice is falsy", v: []any{}, want: false},
		{name: "an unrecognized non-nil type is conservatively truthy", v: 42, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isTruthy(c.v))
		})
	}
}

// TestToFloat64 covers every numeric shape toFloat64 accepts — the normal
// post-json.Unmarshal float64, plus the Go-native int/int64/float32 shapes
// a test or hand-built request can supply — and the fallback for anything
// else.
func TestToFloat64(t *testing.T) {
	cases := []struct {
		v      any
		name   string
		want   float64
		wantOK bool
	}{
		{name: "float64", v: float64(3.5), want: 3.5, wantOK: true},
		{name: "float32", v: float32(2.5), want: 2.5, wantOK: true},
		{name: "int", v: int(7), want: 7, wantOK: true},
		{name: "int64", v: int64(9), want: 9, wantOK: true},
		{name: "unsupported type returns ok=false", v: "7", want: 0, wantOK: false},
		{name: "nil returns ok=false", v: nil, want: 0, wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := toFloat64(c.v)
			assert.Equal(t, c.wantOK, ok)
			assert.Equal(t, c.want, got)
		})
	}
}

// --- openAIRequestFromAnthropic / anthropicResponseFromOpenAI: the
// reverse-direction translators routes_messages.go uses (/v1/messages
// resolved to a non-anthropic-type provider) ---

// TestOpenAIRequestFromAnthropic_ToolResultBecomesToolMessage proves an
// assistant tool_use block becomes an OpenAI tool_calls[] entry (with its
// decoded "input" re-marshaled to a JSON string, matching OpenAI's
// arguments field) and a following user tool_result block becomes its
// own separate role:"tool" message — the mirror of
// anthropicRequestFromOpenAI's pendingToolResults collapse in the other
// direction.
func TestOpenAIRequestFromAnthropic_ToolResultBecomesToolMessage(t *testing.T) {
	req := map[string]any{
		"model": "claude-x",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": map[string]any{"city": "London"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "sunny"},
			}},
		},
	}

	out, _, err := openAIRequestFromAnthropic(req)
	if err != nil {
		t.Fatalf("openAIRequestFromAnthropic: %v", err)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2 (assistant tool_calls + tool result)", msgs)
	}
	assistant, _ := msgs[0].(map[string]any)
	toolCalls, _ := assistant["tool_calls"].([]any)
	if len(toolCalls) != 1 {
		t.Fatalf("assistant tool_calls = %v, want 1 entry", assistant["tool_calls"])
	}
	tc, _ := toolCalls[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	assert.Equal(t, "get_weather", fn["name"])
	assert.JSONEq(t, `{"city":"London"}`, fn["arguments"].(string))

	toolMsg, _ := msgs[1].(map[string]any)
	assert.Equal(t, "tool", toolMsg["role"])
	assert.Equal(t, "call_1", toolMsg["tool_call_id"])
	assert.Equal(t, "sunny", toolMsg["content"])
}

// TestOpenAIRequestFromAnthropic_SystemStringFoldedIntoMessages proves a
// plain-string Anthropic "system" field becomes a role:"system" message
// prepended ahead of the rest, and the Anthropic-only top-level "system"
// key never survives into the OpenAI-shaped output.
func TestOpenAIRequestFromAnthropic_SystemStringFoldedIntoMessages(t *testing.T) {
	req := map[string]any{
		"model":    "claude-x",
		"system":   "Be terse.",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}

	out, _, err := openAIRequestFromAnthropic(req)
	if err != nil {
		t.Fatalf("openAIRequestFromAnthropic: %v", err)
	}
	if _, ok := out["system"]; ok {
		t.Error(`out carries a "system" key; Anthropic's system field has no OpenAI equivalent and must be folded into messages[]`)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %v, want 2 (system + user)", msgs)
	}
	sysMsg, _ := msgs[0].(map[string]any)
	assert.Equal(t, "system", sysMsg["role"])
	assert.Equal(t, "Be terse.", sysMsg["content"])
}

// TestAnthropicResponseFromOpenAI_ToolCallsBecomeToolUseBlocks proves an
// OpenAI tool_calls[] entry becomes an Anthropic tool_use content block
// with its arguments STRING decoded back to a structured "input" value,
// stop_reason maps tool_calls -> tool_use, and the caller-supplied model
// (the client's own requested alias) is echoed into the response.
func TestAnthropicResponseFromOpenAI_ToolCallsBecomeToolUseBlocks(t *testing.T) {
	const body = `{"id":"chatcmpl-77","choices":[{"index":0,"message":{"content":null,"tool_calls":[{"id":"call_9","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`

	out, err := anthropicResponseFromOpenAI([]byte(body), "aliased/model")
	if err != nil {
		t.Fatalf("anthropicResponseFromOpenAI: %v", err)
	}
	assert.Equal(t, "aliased/model", out["model"])
	assert.Equal(t, "tool_use", out["stop_reason"])

	content, _ := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want one tool_use block", out["content"])
	}
	block, _ := content[0].(map[string]any)
	assert.Equal(t, "tool_use", block["type"])
	assert.Equal(t, "get_weather", block["name"])
	assert.Equal(t, "call_9", block["id"])
	input, _ := block["input"].(map[string]any)
	assert.Equal(t, "Paris", input["city"])

	usageMap, _ := out["usage"].(map[string]any)
	assert.Equal(t, int64(3), usageMap["input_tokens"])
	assert.Equal(t, int64(2), usageMap["output_tokens"])
}

// TestAnthropicResponseFromOpenAI_PlainTextResponse proves the common
// case: a plain-text OpenAI response becomes a single Anthropic text
// content block with stop_reason "stop" -> "end_turn".
func TestAnthropicResponseFromOpenAI_PlainTextResponse(t *testing.T) {
	const body = `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`

	out, err := anthropicResponseFromOpenAI([]byte(body), "claude-x")
	if err != nil {
		t.Fatalf("anthropicResponseFromOpenAI: %v", err)
	}
	assert.Equal(t, "message", out["type"])
	assert.Equal(t, "assistant", out["role"])
	assert.Equal(t, "end_turn", out["stop_reason"])
	content, _ := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want one text block", out["content"])
	}
	block, _ := content[0].(map[string]any)
	assert.Equal(t, "text", block["type"])
	assert.Equal(t, "hi there", block["text"])
}

// TestOpenAIContentPartFromAnthropicImage_RejectsNonBase64Source proves an
// Anthropic image block whose source is not base64-encoded (a URL-sourced
// image, which this translator does not support) returns a
// *translateError rather than silently dropping or mistranslating it.
func TestOpenAIContentPartFromAnthropicImage_RejectsNonBase64Source(t *testing.T) {
	_, err := openAIContentPartFromAnthropicImage(map[string]any{"source": map[string]any{"type": "url", "url": "https://example.com/x.png"}})
	if err == nil {
		t.Fatal("want an error for a non-base64 image source, got nil")
	}
	if _, ok := err.(*translateError); !ok {
		t.Errorf("err = %T, want *translateError", err)
	}
}

// TestOpenAIMessagesFromAnthropic_PreservesToolResultThenTextOrder is the
// regression for item 3 (IMPORTANT, 2026-08-22 review): a legal Anthropic
// user turn shaped [tool_result, text] must translate preserving that
// order — tool message first, then the content message — since OpenAI
// requires a role:"tool" message to directly follow the assistant
// message carrying the tool_calls it answers, with nothing in between.
// The previous version of this function always emitted every tool_result
// AFTER the combined content/tool_calls message regardless of its
// original position, which turned this exact shape into
// [assistant(tool_calls), user(text), tool(...)] — an OpenAI 400, since
// user(text) sits between the tool_calls and its answer.
func TestOpenAIMessagesFromAnthropic_PreservesToolResultThenTextOrder(t *testing.T) {
	content := []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "sunny"},
		map[string]any{"type": "text", "text": "thanks"},
	}

	out, err := openAIMessagesFromAnthropic("user", content)
	if err != nil {
		t.Fatalf("openAIMessagesFromAnthropic: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("messages = %v, want 2 (tool result, then text)", out)
	}

	first, _ := out[0].(map[string]any)
	if first["role"] != "tool" || first["tool_call_id"] != "call_1" {
		t.Errorf("messages[0] = %v, want the tool_result message FIRST, matching its original position", first)
	}

	second, _ := out[1].(map[string]any)
	if second["role"] != "user" {
		t.Errorf("messages[1] = %v, want the text-content message SECOND", second)
	}
	parts, _ := second["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("messages[1].content = %v, want one text part", second["content"])
	}
}

// TestOpenAIMessagesFromAnthropic_InterspersedToolResults proves the fix
// generalizes beyond one tool_result: [tool_result A, text, tool_result
// B] must translate to [tool(A), user(text), tool(B)] — each tool_result
// flushing whatever content/tool_calls had accumulated before it, not
// just the first one.
func TestOpenAIMessagesFromAnthropic_InterspersedToolResults(t *testing.T) {
	content := []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call_A", "content": "a"},
		map[string]any{"type": "text", "text": "middle"},
		map[string]any{"type": "tool_result", "tool_use_id": "call_B", "content": "b"},
	}

	out, err := openAIMessagesFromAnthropic("user", content)
	if err != nil {
		t.Fatalf("openAIMessagesFromAnthropic: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("messages = %v, want 3 (tool A, text, tool B)", out)
	}
	m0, _ := out[0].(map[string]any)
	m1, _ := out[1].(map[string]any)
	m2, _ := out[2].(map[string]any)
	if m0["tool_call_id"] != "call_A" {
		t.Errorf("messages[0].tool_call_id = %v, want call_A", m0["tool_call_id"])
	}
	if m1["role"] != "user" {
		t.Errorf("messages[1].role = %v, want user (the interspersed text)", m1["role"])
	}
	if m2["tool_call_id"] != "call_B" {
		t.Errorf("messages[2].tool_call_id = %v, want call_B", m2["tool_call_id"])
	}
}

// TestOpenAIToolMessageFromAnthropic_PreservesIsError is the regression
// for item 11a (IMPORTANT, 2026-08-22 review): Anthropic's
// tool_result.is_error has no native OpenAI tool-message field, so the
// previous version of this function silently dropped it. A string
// content now gets an "Error: " prefix so the failure signal survives
// into the one shape OpenAI's tool message actually carries.
func TestOpenAIToolMessageFromAnthropic_PreservesIsError(t *testing.T) {
	cases := []struct {
		content     any
		wantContent any
		name        string
		isError     bool
	}{
		{name: "string content with is_error prefixes Error:", content: "boom", isError: true, wantContent: "Error: boom"},
		{name: "string content without is_error is unchanged", content: "ok", isError: false, wantContent: "ok"},
		{name: "non-string content is left as-is even with is_error", content: []any{"x"}, isError: true, wantContent: []any{"x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bm := map[string]any{"tool_use_id": "call_1", "content": tc.content, "is_error": tc.isError}
			got := openAIToolMessageFromAnthropic(bm)
			assert.Equal(t, tc.wantContent, got["content"])
		})
	}
}

// TestOpenAIRequestFromAnthropic_ThinkingFieldReportedAsDropped is the
// regression for item 11b (IMPORTANT, 2026-08-22 review): Anthropic's
// "thinking" (extended-thinking config) has no OpenAI equivalent.
// openAIRequestFromAnthropic must report it in its dropped-fields return
// rather than silently discarding it, so the caller (routes_messages.go's
// callTranslatedMessages) can log a warning instead of the drop being
// invisible.
func TestOpenAIRequestFromAnthropic_ThinkingFieldReportedAsDropped(t *testing.T) {
	req := map[string]any{
		"model":    "claude-x",
		"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(1024)},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}

	_, dropped, err := openAIRequestFromAnthropic(req)
	if err != nil {
		t.Fatalf("openAIRequestFromAnthropic: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "thinking" {
		t.Errorf("dropped = %v, want [\"thinking\"]", dropped)
	}
}

// TestOpenAIRequestFromAnthropic_ThinkingAbsent_NothingDropped proves the
// dropped-fields list stays empty for a request that never set
// "thinking" in the first place — the previous test's absence is not
// itself evidence of a bug.
func TestOpenAIRequestFromAnthropic_ThinkingAbsent_NothingDropped(t *testing.T) {
	req := map[string]any{
		"model":    "claude-x",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}

	_, dropped, err := openAIRequestFromAnthropic(req)
	if err != nil {
		t.Fatalf("openAIRequestFromAnthropic: %v", err)
	}
	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none", dropped)
	}
}

// TestOpenAIRequestFromAnthropic_ReasoningModel_UsesMaxCompletionTokens is
// the regression for item 10 (IMPORTANT, 2026-08-22 review): OpenAI's
// o-series and gpt-5-family reasoning models reject the "max_tokens"
// request field with a 400 and require "max_completion_tokens" instead.
// Every other model keeps using "max_tokens" as before.
func TestOpenAIRequestFromAnthropic_ReasoningModel_UsesMaxCompletionTokens(t *testing.T) {
	cases := []struct {
		model      string
		wantField  string
		wantOthers []string
	}{
		{"o3-mini", "max_completion_tokens", []string{"max_tokens"}},
		{"o1", "max_completion_tokens", []string{"max_tokens"}},
		{"o4-mini-2025-04-16", "max_completion_tokens", []string{"max_tokens"}},
		{"gpt-5", "max_completion_tokens", []string{"max_tokens"}},
		{"gpt-5.1-chat-latest", "max_completion_tokens", []string{"max_tokens"}},
		{"gpt-4o", "max_tokens", []string{"max_completion_tokens"}},
		{"gpt-4o-mini", "max_tokens", []string{"max_completion_tokens"}},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			req := map[string]any{
				"model":      tc.model,
				"max_tokens": float64(256),
				"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
			}
			out, _, err := openAIRequestFromAnthropic(req)
			if err != nil {
				t.Fatalf("openAIRequestFromAnthropic: %v", err)
			}
			if _, ok := out[tc.wantField]; !ok {
				t.Errorf("out[%q] missing, want it set to 256", tc.wantField)
			}
			for _, other := range tc.wantOthers {
				if _, ok := out[other]; ok {
					t.Errorf("out[%q] present, want only %q set", other, tc.wantField)
				}
			}
		})
	}
}

// TestAnthropicUsagePayload_TotalInputTokens_FoldsCacheCounters is the
// unit-level regression for item 6 (IMPORTANT, 2026-08-22 review):
// cache_creation_input_tokens and cache_read_input_tokens must fold into
// the billed prompt count alongside fresh input_tokens, not be dropped.
func TestAnthropicUsagePayload_TotalInputTokens_FoldsCacheCounters(t *testing.T) {
	p := anthropicUsagePayload{InputTokens: 4, CacheCreationInputTokens: 180000, CacheReadInputTokens: 20000}
	if got, want := p.totalInputTokens(), int64(200004); got != want {
		t.Errorf("totalInputTokens() = %d, want %d", got, want)
	}
}
