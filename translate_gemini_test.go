package traefikllmgateway

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// geminiGoldenTestdataDir is where every fixture this file walks lives.
const geminiGoldenTestdataDir = "testdata/gemini"

// geminiGoldenGatewayModel and geminiGoldenCreated are the fixed model/
// created values every response- and stream-translation golden fixture's
// "_want.json" bakes in — openAIResponseFromGemini and
// newGeminiStreamState both take these as caller-supplied parameters
// (Gemini's own wire formats carry neither), so the golden tests must
// supply the same fixed values the "_want.json" files were written
// against.
const (
	geminiGoldenGatewayModel = "gw-gemini-model"
	geminiGoldenCreated      = int64(1734000000)
)

// geminiReadGoldenJSON reads and json.Unmarshals name (relative to
// testdata/gemini) into a fresh map[string]any.
func geminiReadGoldenJSON(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(geminiGoldenTestdataDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return m
}

// geminiAssertTranslateError asserts err is a *translateError matching want.
func geminiAssertTranslateError(t *testing.T, err error, want wantErrFixture) {
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

// TestGeminiRequestGolden walks every "chat_*_in.json" fixture under
// testdata/gemini, translating it via geminiRequestFromOpenAI, and asserts
// the result against the sibling "_want.json" fixture (a successful
// translation) or "_wanterr.json" fixture (a *translateError), whichever is
// present — exactly one of the two exists per fixture.
func TestGeminiRequestGolden(t *testing.T) {
	entries, err := os.ReadDir(geminiGoldenTestdataDir)
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
		t.Fatalf("no chat_*_in.json fixtures found under %s", geminiGoldenTestdataDir)
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			in := geminiReadGoldenJSON(t, name+"_in.json")

			got, err := geminiRequestFromOpenAI(in)

			wantErrPath := filepath.Join(geminiGoldenTestdataDir, name+"_wanterr.json")
			if _, statErr := os.Stat(wantErrPath); statErr == nil {
				var want wantErrFixture
				b, rerr := os.ReadFile(wantErrPath)
				if rerr != nil {
					t.Fatalf("read %s: %v", wantErrPath, rerr)
				}
				if uerr := json.Unmarshal(b, &want); uerr != nil {
					t.Fatalf("decode %s: %v", wantErrPath, uerr)
				}
				geminiAssertTranslateError(t, err, want)
				return
			}

			if err != nil {
				t.Fatalf("geminiRequestFromOpenAI: %v", err)
			}
			want := geminiReadGoldenJSON(t, name+"_want.json")
			gotN := normalizeJSON(t, got)
			wantN := normalizeJSON(t, want)
			if !reflect.DeepEqual(gotN, wantN) {
				gotB, _ := json.MarshalIndent(gotN, "", "  ")
				wantB, _ := json.MarshalIndent(wantN, "", "  ")
				t.Errorf("geminiRequestFromOpenAI(%s) mismatch:\ngot:\n%s\nwant:\n%s", name, gotB, wantB)
			}
		})
	}
}

// TestGeminiResponseGolden walks every "resp_*_in.json" fixture,
// translating it via openAIResponseFromGemini with the fixed
// geminiGoldenGatewayModel/geminiGoldenCreated, and asserts the result
// against the sibling "_want.json" fixture.
func TestGeminiResponseGolden(t *testing.T) {
	entries, err := os.ReadDir(geminiGoldenTestdataDir)
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
		t.Fatalf("no resp_*_in.json fixtures found under %s", geminiGoldenTestdataDir)
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(geminiGoldenTestdataDir, name+"_in.json"))
			if err != nil {
				t.Fatalf("read %s_in.json: %v", name, err)
			}

			got, _, err := openAIResponseFromGemini(body, geminiGoldenGatewayModel, geminiGoldenCreated)
			if err != nil {
				t.Fatalf("openAIResponseFromGemini: %v", err)
			}

			want := geminiReadGoldenJSON(t, name+"_want.json")
			gotN := normalizeJSON(t, got)
			wantN := normalizeJSON(t, want)
			if !reflect.DeepEqual(gotN, wantN) {
				gotB, _ := json.MarshalIndent(gotN, "", "  ")
				wantB, _ := json.MarshalIndent(wantN, "", "  ")
				t.Errorf("openAIResponseFromGemini(%s) mismatch:\ngot:\n%s\nwant:\n%s", name, gotB, wantB)
			}
		})
	}
}

// geminiRunStreamGolden translates every event in
// testdata/gemini/{name}.sse through a fresh geminiStreamState, in order,
// and asserts the flattened list of emitted chunks against
// "{name}_want.json" and the final usage() against "{name}_usage.json".
func geminiRunStreamGolden(t *testing.T, name string) {
	t.Helper()

	f, err := os.Open(filepath.Join(geminiGoldenTestdataDir, name+".sse"))
	if err != nil {
		t.Fatalf("open %s.sse: %v", name, err)
	}
	defer f.Close() //nolint:errcheck

	st := newGeminiStreamState(geminiGoldenGatewayModel, geminiGoldenCreated)
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

	wantBytes, err := os.ReadFile(filepath.Join(geminiGoldenTestdataDir, name+"_want.json"))
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

	usageBytes, err := os.ReadFile(filepath.Join(geminiGoldenTestdataDir, name+"_usage.json"))
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

// TestGeminiStreamGolden_Text covers a text-only stream: a role chunk, two
// content deltas across two chunks, and a finish chunk on a third,
// parts-less chunk that also carries the final usageMetadata.
func TestGeminiStreamGolden_Text(t *testing.T) {
	geminiRunStreamGolden(t, "stream_text")
}

// TestGeminiStreamGolden_TextThenFuncall covers a text delta, then a
// functionCall part with no upstream "id" (proving the ordinal-synthesized
// "call_0" id, per lesson (a)), then a finish chunk on its own later,
// parts-less chunk — proving st.sawFunctionCall correctly latches across
// chunks so the finish_reason still maps to "tool_calls" rather than
// "stop" even though the finish chunk itself carries no functionCall part.
func TestGeminiStreamGolden_TextThenFuncall(t *testing.T) {
	geminiRunStreamGolden(t, "stream_text_then_funcall")
}

// TestGeminiStreamGolden_PromptBlocked covers a prompt-level safety block:
// a single chunk with no "candidates" at all, only promptFeedback.
// blockReason set — review fix (item 3). Proves the role chunk still
// fires, followed by a finish chunk mapped to "content_filter" rather than
// the stream silently ending with no finish signal at all.
func TestGeminiStreamGolden_PromptBlocked(t *testing.T) {
	geminiRunStreamGolden(t, "stream_prompt_blocked")
}

// TestGeminiStreamState_MalformedEventReturnsError proves a malformed
// (invalid-JSON) SSE data payload returns an error wrapping errUpstream,
// rather than being swallowed as (nil, nil) — lesson (c).
func TestGeminiStreamState_MalformedEventReturnsError(t *testing.T) {
	st := newGeminiStreamState(geminiGoldenGatewayModel, geminiGoldenCreated)
	chunks, err := st.translate(sseEvent{data: []byte(`{"candidates":[`)})
	if err == nil {
		t.Fatalf("translate(corrupt data): want error, got nil (chunks=%v)", chunks)
	}
	if !errors.Is(err, errUpstream) {
		t.Errorf("translate(corrupt data): err = %v, want errors.Is(err, errUpstream)", err)
	}
}

// TestGeminiStreamState_UsageMetadataOnlyOverwritesWhenPresent proves "last
// usageMetadata wins" only among chunks that actually carry the field — a
// later chunk with no usageMetadata at all must not reset a usage total an
// earlier chunk already captured.
func TestGeminiStreamState_UsageMetadataOnlyOverwritesWhenPresent(t *testing.T) {
	st := newGeminiStreamState(geminiGoldenGatewayModel, geminiGoldenCreated)

	if _, err := st.translate(sseEvent{data: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2}}`)}); err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := st.usage(); got.prompt != 7 || got.completion != 2 {
		t.Fatalf("usage() after first chunk = %+v, want {prompt:7 completion:2}", got)
	}

	if _, err := st.translate(sseEvent{data: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":" there"}]}}]}`)}); err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := st.usage(); got.prompt != 7 || got.completion != 2 {
		t.Errorf("usage() after a usageMetadata-less chunk = %+v, want unchanged {prompt:7 completion:2}", got)
	}
}

// TestGeminiStreamState_IDLockedOnFirstChunk proves the id resolves once,
// on the first translated chunk, and never changes after — review fix
// (item 5, folded minor). The first chunk here carries no responseId, so
// every emitted chunk (including one after a later chunk arrives with a
// real responseId) must share the same construction-time fallback id.
func TestGeminiStreamState_IDLockedOnFirstChunk(t *testing.T) {
	const created = int64(1734000000)
	st := newGeminiStreamState(geminiGoldenGatewayModel, created)
	wantID := "chatcmpl-gemini-1734000000"

	chunks, err := st.translate(sseEvent{data: []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}]}`)})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	for _, c := range chunks {
		assertGeminiChunkID(t, c, wantID)
	}

	// A later chunk carries a real responseId — it must NOT override the
	// fallback already locked in on the first chunk.
	chunks, err = st.translate(sseEvent{data: []byte(`{"responseId":"resp_LATE","candidates":[{"content":{"role":"model","parts":[{"text":" there"}]},"finishReason":"STOP"}]}`)})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatalf("translate: want at least one chunk, got none")
	}
	for _, c := range chunks {
		assertGeminiChunkID(t, c, wantID)
	}
}

// TestGeminiStreamState_IDAdoptsFirstChunkResponseID proves the opposite
// side of the same rule: when the very first chunk DOES carry a
// responseId, that id — not the construction-time fallback — is what
// locks in for the rest of the stream.
func TestGeminiStreamState_IDAdoptsFirstChunkResponseID(t *testing.T) {
	st := newGeminiStreamState(geminiGoldenGatewayModel, geminiGoldenCreated)
	chunks, err := st.translate(sseEvent{data: []byte(`{"responseId":"resp_FIRST","candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}]}`)})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	for _, c := range chunks {
		assertGeminiChunkID(t, c, "chatcmpl-resp_FIRST")
	}
}

// assertGeminiChunkID decodes chunk and asserts its "id" field equals want.
func assertGeminiChunkID(t *testing.T, chunk []byte, want string) {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(chunk, &v); err != nil {
		t.Fatalf("chunk is not valid JSON: %v (%s)", err, chunk)
	}
	if v["id"] != want {
		t.Errorf("chunk id = %v, want %q (chunk: %s)", v["id"], want, chunk)
	}
}

// TestGeminiRequestFromOpenAI_MaxCompletionTokensPrecedence proves
// max_completion_tokens takes precedence over max_tokens when an OpenAI
// request sends both — the newer field wins, mirroring the anthropic
// translator's rule.
func TestGeminiRequestFromOpenAI_MaxCompletionTokensPrecedence(t *testing.T) {
	req := map[string]any{
		"model":                 "gemini-2.5-pro",
		"messages":              []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens":            float64(100),
		"max_completion_tokens": float64(200),
	}
	got, err := geminiRequestFromOpenAI(req)
	if err != nil {
		t.Fatalf("geminiRequestFromOpenAI: %v", err)
	}
	genConfig, ok := got["generationConfig"].(map[string]any)
	if !ok {
		t.Fatalf("generationConfig = %v, want a map", got["generationConfig"])
	}
	if genConfig["maxOutputTokens"] != int64(200) {
		t.Errorf("maxOutputTokens = %v, want 200 (max_completion_tokens takes precedence)", genConfig["maxOutputTokens"])
	}
}

// TestGeminiRequestFromOpenAI_LogitBiasEmptyMapAllowed proves a
// present-but-empty logit_bias does not 400 — lesson (i): reject only when
// truthy/non-empty.
func TestGeminiRequestFromOpenAI_LogitBiasEmptyMapAllowed(t *testing.T) {
	req := map[string]any{
		"model":      "gemini-2.5-pro",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"logit_bias": map[string]any{},
	}
	if _, err := geminiRequestFromOpenAI(req); err != nil {
		t.Errorf("geminiRequestFromOpenAI with empty logit_bias: %v, want no error", err)
	}
}

// TestGeminiRequestFromOpenAI_UnknownToolCallIDErrors proves a role:"tool"
// message whose tool_call_id was never seen on a preceding assistant
// message's tool_calls[] is a *translateError, per the request mapping
// table's role:tool row — Gemini's functionResponse needs the name that
// map resolves, and an unresolvable id must not silently produce an empty
// name.
func TestGeminiRequestFromOpenAI_UnknownToolCallIDErrors(t *testing.T) {
	req := map[string]any{
		"model": "gemini-2.5-pro",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "tool", "tool_call_id": "call_never_seen", "content": "42"},
		},
	}
	_, err := geminiRequestFromOpenAI(req)
	var terr *translateError
	if !errors.As(err, &terr) {
		t.Fatalf("err = %v (%T), want *translateError", err, err)
	}
	if !strings.Contains(terr.Error(), "call_never_seen") {
		t.Errorf("error message = %q, want it to mention the unknown id", terr.Error())
	}
}

// TestGeminiEmbeddings_Single proves a single-string OpenAI embeddings
// input builds Gemini's :embedContent request body and translates its
// response back to an OpenAI embeddings list response, with usage
// estimated (ceil(chars/4)) and marked estimated — per the embeddings
// mapping table.
func TestGeminiEmbeddings_Single(t *testing.T) {
	in := geminiReadGoldenJSON(t, "embed_single_in.json")

	texts, single, err := geminiEmbeddingInputTexts(in["input"])
	if err != nil {
		t.Fatalf("geminiEmbeddingInputTexts: %v", err)
	}
	if !single {
		t.Fatalf("single = false, want true for a string input")
	}

	gotReq := geminiEmbedContentRequest(texts[0])
	wantReq := geminiReadGoldenJSON(t, "embed_single_want.json")
	if !reflect.DeepEqual(normalizeJSON(t, gotReq), normalizeJSON(t, wantReq)) {
		t.Errorf("geminiEmbedContentRequest mismatch: got %#v, want %#v", gotReq, wantReq)
	}

	respBody, err := os.ReadFile(filepath.Join(geminiGoldenTestdataDir, "embed_single_resp_in.json"))
	if err != nil {
		t.Fatalf("read embed_single_resp_in.json: %v", err)
	}
	estTokens := estimatedEmbeddingTokens(texts)
	if estTokens != 3 {
		t.Fatalf("estimatedEmbeddingTokens(%q) = %d, want 3", texts, estTokens)
	}

	gotResp, u, err := openAIEmbeddingResponseFromGemini(respBody, single, "gw-embed-model", estTokens)
	if err != nil {
		t.Fatalf("openAIEmbeddingResponseFromGemini: %v", err)
	}
	if !u.estimated {
		t.Errorf("usage.estimated = false, want true")
	}
	if u.prompt != 3 {
		t.Errorf("usage.prompt = %d, want 3", u.prompt)
	}
	wantResp := geminiReadGoldenJSON(t, "embed_single_resp_want.json")
	if !reflect.DeepEqual(normalizeJSON(t, gotResp), normalizeJSON(t, wantResp)) {
		gotB, _ := json.MarshalIndent(gotResp, "", "  ")
		wantB, _ := json.MarshalIndent(wantResp, "", "  ")
		t.Errorf("openAIEmbeddingResponseFromGemini mismatch:\ngot:\n%s\nwant:\n%s", gotB, wantB)
	}
}

// TestGeminiEmbeddings_Batch mirrors TestGeminiEmbeddings_Single for an
// array-of-strings OpenAI embeddings input, which selects Gemini's
// :batchEmbedContents endpoint and restores the "models/" prefix on each
// request entry's own "model" field, per the embeddings mapping table.
func TestGeminiEmbeddings_Batch(t *testing.T) {
	in := geminiReadGoldenJSON(t, "embed_batch_in.json")
	rawModel, _ := in["model"].(string)

	texts, single, err := geminiEmbeddingInputTexts(in["input"])
	if err != nil {
		t.Fatalf("geminiEmbeddingInputTexts: %v", err)
	}
	if single {
		t.Fatalf("single = true, want false for an array input")
	}

	gotReq := geminiBatchEmbedContentsRequest(rawModel, texts)
	wantReq := geminiReadGoldenJSON(t, "embed_batch_want.json")
	if !reflect.DeepEqual(normalizeJSON(t, gotReq), normalizeJSON(t, wantReq)) {
		gotB, _ := json.MarshalIndent(gotReq, "", "  ")
		wantB, _ := json.MarshalIndent(wantReq, "", "  ")
		t.Errorf("geminiBatchEmbedContentsRequest mismatch:\ngot:\n%s\nwant:\n%s", gotB, wantB)
	}

	respBody, err := os.ReadFile(filepath.Join(geminiGoldenTestdataDir, "embed_batch_resp_in.json"))
	if err != nil {
		t.Fatalf("read embed_batch_resp_in.json: %v", err)
	}
	estTokens := estimatedEmbeddingTokens(texts)
	if estTokens != 3 {
		t.Fatalf("estimatedEmbeddingTokens(%q) = %d, want 3", texts, estTokens)
	}

	gotResp, u, err := openAIEmbeddingResponseFromGemini(respBody, single, "gw-embed-model", estTokens)
	if err != nil {
		t.Fatalf("openAIEmbeddingResponseFromGemini: %v", err)
	}
	if !u.estimated {
		t.Errorf("usage.estimated = false, want true")
	}
	wantResp := geminiReadGoldenJSON(t, "embed_batch_resp_want.json")
	if !reflect.DeepEqual(normalizeJSON(t, gotResp), normalizeJSON(t, wantResp)) {
		gotB, _ := json.MarshalIndent(gotResp, "", "  ")
		wantB, _ := json.MarshalIndent(wantResp, "", "  ")
		t.Errorf("openAIEmbeddingResponseFromGemini mismatch:\ngot:\n%s\nwant:\n%s", gotB, wantB)
	}
}

// TestGeminiEmbeddingInputTexts_RejectsNonStringArrayEntry proves an
// input[] array with a non-string entry is a *translateError.
func TestGeminiEmbeddingInputTexts_RejectsNonStringArrayEntry(t *testing.T) {
	_, _, err := geminiEmbeddingInputTexts([]any{"ok", float64(5)})
	var terr *translateError
	if !errors.As(err, &terr) {
		t.Fatalf("err = %v (%T), want *translateError", err, err)
	}
}

// TestGeminiToolConfigFromOpenAI covers every tool_choice shape the request
// mapping table lists: the three recognized string modes, a function-name
// object, and the two "no mapping" cases (an unrecognized string, and an
// object without a valid function.name) that must return nil so the caller
// omits toolConfig entirely rather than sending a malformed one.
func TestGeminiToolConfigFromOpenAI(t *testing.T) {
	cases := []struct {
		tc   any
		want map[string]any
		name string
	}{
		{
			name: "none",
			tc:   "none",
			want: map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}},
		},
		{
			name: "auto",
			tc:   "auto",
			want: map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}},
		},
		{
			name: "required",
			tc:   "required",
			want: map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}},
		},
		{
			name: "function object names the allowed function",
			tc:   map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
			want: map[string]any{"functionCallingConfig": map[string]any{
				"mode":                 "ANY",
				"allowedFunctionNames": []any{"get_weather"},
			}},
		},
		{
			name: "unrecognized string has no mapping",
			tc:   "bogus",
			want: nil,
		},
		{
			name: "object without a function.name has no mapping",
			tc:   map[string]any{"type": "function"},
			want: nil,
		},
		{
			name: "an unrecognized type has no mapping",
			tc:   42,
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, geminiToolConfigFromOpenAI(c.tc))
		})
	}
}
