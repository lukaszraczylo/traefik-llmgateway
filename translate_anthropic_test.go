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
