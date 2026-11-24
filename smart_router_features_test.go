package traefikllmgateway

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const featGoSnippet = "func f(a int) (int, error) {\n\tif a > 0 {\n\t\treturn a * 2, nil\n\t}\n\treturn 0, fmt.Errorf(\"bad %d\", a)\n}"

func TestExtractV2ProsePasteSplit(t *testing.T) {
	t.Parallel()
	dense := strings.Repeat("if (a[i] == b[j]) { x = y | z; }\n", 4)
	tests := []struct {
		name      string
		text      string
		wantIn    []string
		wantOut   []string
		wantCode  bool
		wantIntnt int
	}{
		{
			name:     "fenced code leaves the prose",
			text:     "please prove the claim\n```go\n" + featGoSnippet + "\n```\nthanks",
			wantIn:   []string{"please prove the claim", "thanks"},
			wantOut:  []string{"func f", "fmt.Errorf"},
			wantCode: true, wantIntnt: smartIntentMath,
		},
		{
			name:     "dense unfenced paragraph is a paste",
			text:     "what language is this\n\n" + dense,
			wantIn:   []string{"what language is this"},
			wantOut:  []string{"a[i]"},
			wantCode: true, wantIntnt: smartIntentUnknown,
		},
		{
			name:    "markers inside a paste do not set the intent",
			text:    "how many lines is this\n\n```\nprove derive analyze evaluate justify theorem\n```",
			wantIn:  []string{"how many lines"},
			wantOut: []string{"theorem"},
			// The fence holds no log signature, so it counts as code.
			wantCode: true, wantIntnt: smartIntentUnknown,
		},
		{
			name:      "plain prose stays whole",
			text:      "Summarize the council vote. The council voted five to four.",
			wantIn:    []string{"Summarize the council vote", "five to four"},
			wantIntnt: smartIntentRewrite,
		},
		{
			name:      "short bullet list is prose not a paste",
			text:      "We need:\n\n- a\n- b\n- c\n- d\n- e\n- f\n- g\n\nshould we migrate?",
			wantIn:    []string{"- a", "should we migrate"},
			wantIntnt: smartIntentDesign,
		},
		{
			name:     "question trailing a paste without a blank line stays prose",
			text:     "```\n```\n" + dense + "why is this slow for large inputs?",
			wantIn:   []string{"why is this slow for large inputs?"},
			wantOut:  []string{"a[i]"},
			wantCode: true, wantIntnt: smartIntentDebug,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := extractSmartV2(chatMsgs(msg("user", tc.text)), cacheEndpointChat)
			require.True(t, f.found)
			for _, in := range tc.wantIn {
				assert.Contains(t, f.prose, in)
			}
			for _, out := range tc.wantOut {
				assert.NotContains(t, f.prose, out)
			}
			assert.Equal(t, tc.wantCode, f.codeBytes > 0, "codeBytes")
			assert.Equal(t, smartIntentNames[tc.wantIntnt], smartIntentNames[f.intent])
		})
	}
}

func TestExtractV2ProseCap(t *testing.T) {
	t.Parallel()
	f := extractSmartV2(chatMsgs(msg("user", strings.Repeat("word ", 5000))), cacheEndpointChat)
	assert.LessOrEqual(t, len(f.prose), smartTextCap+1)
	assert.Equal(t, strings.ToLower(f.prose), f.lower)
}

func TestExtractV2LogDetection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		text    string
		wantMsg string
		wantLog bool
	}{
		{"python traceback", "why?\n\nTraceback (most recent call last):\n  File \"a.py\", line 1\nKeyError: 'x'", "why?", true},
		{"go panic", "help\n\npanic: runtime error: index out of range\n\ngoroutine 1 [running]:", "help", true},
		{"java frame", "npe?\n\nException in thread main\n\tat com.acme.A.run(A.java:3)", "npe?", true},
		{"fenced build error", "fix\n```\nerror: cannot find module\n```", "fix", true},
		{"weak signature in prose is not a log", "I saw an error: yesterday and an Exception too, can you explain what these words mean in general", "explain what these words", false},
		{"npm log", "install fails\n\nnpm ERR! code ERESOLVE\nnpm ERR! could not resolve", "install fails", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := extractSmartV2(chatMsgs(msg("user", tc.text)), cacheEndpointChat)
			assert.Equal(t, tc.wantLog, f.logBytes > 0, "logBytes")
			assert.Contains(t, f.prose, tc.wantMsg)
			if tc.wantLog {
				assert.Equal(t, smartIntentDebug, f.intent)
			}
		})
	}
}

func deepLoop(n int, anthropic bool) map[string]any {
	msgs := []any{msg("user", "Refactor the scheduler to remove the global lock")}
	for len(msgs) < n {
		id := "c" + strings.Repeat("x", len(msgs)%7)
		if anthropic {
			msgs = append(msgs,
				msg("assistant", []any{map[string]any{"type": "tool_use", "id": id, "name": "sh", "input": map[string]any{}}}),
				msg("user", []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"}}))
			continue
		}
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": id}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": "ok"})
	}
	return map[string]any{"messages": msgs[:n], "tools": []any{map[string]any{"type": "function"}}}
}

func TestExtractV2DeepAskScan400(t *testing.T) {
	t.Parallel()
	for _, anthropic := range []bool{false, true} {
		endpoint := cacheEndpointChat
		if anthropic {
			endpoint = cacheEndpointMessages
		}
		t.Run(endpoint, func(t *testing.T) {
			t.Parallel()
			f := extractSmartV2(deepLoop(399, anthropic), endpoint)
			assert.True(t, f.found, "ask 399 messages back")
			assert.Contains(t, f.prose, "scheduler")
			assert.True(t, f.toolLoopTurn)
			assert.True(t, f.toolHist)
			assert.False(t, f.truncated)

			f = extractSmartV2(deepLoop(401, anthropic), endpoint)
			assert.False(t, f.found, "ask beyond the cap")
			assert.True(t, f.truncated)
			assert.Equal(t, 401, f.msgCount)

			v1 := extractSmartRequest(deepLoop(60, anthropic), endpoint)
			assert.Empty(t, v1.userText, "v1 still stops at the 32 message window")
		})
	}
}

func TestExtractV2ToolTurns(t *testing.T) {
	t.Parallel()
	failed := chatMsgs(msg("user", "run the tests"), map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1"}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "--- FAIL: TestX\nexit status 1"})
	f := extractSmartV2(failed, cacheEndpointChat)
	assert.True(t, f.toolLoopTurn)
	assert.True(t, f.toolErr)

	okRes := chatMsgs(msg("user", "run the tests"), map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok  \tpkg\t0.3s"})
	assert.False(t, extractSmartV2(okRes, cacheEndpointChat).toolErr)

	isErr := chatMsgs(msg("user", "run it"), msg("assistant", []any{map[string]any{"type": "tool_use", "id": "t1"}}),
		msg("user", []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "is_error": true, "content": "denied"}}))
	f = extractSmartV2(isErr, cacheEndpointMessages)
	assert.True(t, f.toolLoopTurn)
	assert.True(t, f.toolErr)

	// A tool_result turn that also carries reminder text is still a tool turn, not a new ask.
	withText := chatMsgs(msg("user", "ship the release"), msg("assistant", []any{map[string]any{"type": "tool_use", "id": "t1"}}),
		msg("user", []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}, part("text", "text", "reminder: be brief")}))
	f = extractSmartV2(withText, cacheEndpointMessages)
	assert.True(t, f.toolLoopTurn)
	assert.Contains(t, f.prose, "ship the release")
	assert.NotContains(t, f.prose, "reminder")

	human := chatMsgs(msg("user", "first"), map[string]any{"role": "tool", "content": "ok"}, msg("user", "now explain the design"))
	f = extractSmartV2(human, cacheEndpointChat)
	assert.False(t, f.toolLoopTurn)
	assert.Contains(t, f.prose, "explain the design")
}

func TestExtractV2NonLatin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		text        string
		wantNonLat  bool
		wantForeign bool
	}{
		{"chinese", "请证明根号2是无理数，并说明每一步的理由", true, true},
		{"cyrillic", "Объясни разницу между блокировками в базе данных", true, true},
		{"arabic", "ما هي عاصمة مصر وكيف أصل إليها", true, true},
		{"polish is latin script but not english", "Zaprojektuj architekturę systemu płatności odpornego na awarie", false, true},
		{"german", "Warum stürzt mein Programm ab wenn ich die Liste sortiere", false, true},
		{"english has no flag", "What is the best way to store sessions for this service", false, false},
		{"too short is not foreign", "你好吗", true, false},
		{"known intent is not foreign", "Prove the lemma", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := extractSmartV2(chatMsgs(msg("user", tc.text)), cacheEndpointChat)
			assert.Equal(t, tc.wantNonLat, f.nonLatin, "nonLatin")
			assert.Equal(t, tc.wantForeign, f.foreign, "foreign")
		})
	}
}

func TestExtractV2Signals(t *testing.T) {
	t.Parallel()
	req := map[string]any{
		"max_tokens":      9000,
		"response_format": map[string]any{"type": "json_schema"},
		"tools":           []any{map[string]any{"type": "function"}},
		"messages":        []any{msg("system", "be terse"), msg("user", "Why? How?\n1. a\n2. b\n- c")},
	}
	f := extractSmartV2(req, cacheEndpointChat)
	assert.Equal(t, "be terse", f.system)
	assert.Equal(t, 2, f.questions)
	assert.Equal(t, 3, f.lists)
	assert.Equal(t, smartJSONSchema, f.jsonMode)
	assert.Equal(t, int64(9000), f.outTokens)
	assert.True(t, f.hasTools)
	assert.Equal(t, 2, f.msgCount)

	an := map[string]any{"system": "be terse", "messages": []any{msg("user", []any{part("text", "text", "hello"), map[string]any{"type": "image", "source": map[string]any{}}})}}
	f = extractSmartV2(an, cacheEndpointMessages)
	assert.Equal(t, "be terse", f.system)
	assert.True(t, f.hasImage)

	assert.Equal(t, 2, extractSmartV2(chatMsgs(msg("user", "你好吗？没问题？")), cacheEndpointChat).questions)
}

func TestExtractV2IntentSpread(t *testing.T) {
	t.Parallel()
	f := extractSmartV2(chatMsgs(msg("user", "Summarize the text and also prove the theorem")), cacheEndpointChat)
	assert.Equal(t, smartIntentMath, f.intent)
	assert.True(t, f.intentSpread)
	f = extractSmartV2(chatMsgs(msg("user", "Why does it crash? Is the design wrong?")), cacheEndpointChat)
	assert.False(t, f.intentSpread, "debug and design sit close together")
}

func TestExtractV2EmptyAndHistoryGaps(t *testing.T) {
	t.Parallel()
	f := extractSmartV2(map[string]any{"messages": []any{msg("assistant", "x"), "junk", 7}}, cacheEndpointChat)
	assert.False(t, f.found)
	assert.False(t, f.truncated)
	assert.Equal(t, 3, f.msgCount)
	assert.Equal(t, smartIntentUnknown, f.intent)
}

func hugePasteReq(pasteBytes int) map[string]any {
	paste := strings.Repeat("if (a[i] == b[j]) { x = y | z; }\n", pasteBytes/32+1)[:pasteBytes]
	return userReq("Please review this code and tell me why it fails.\n```\n" + paste + "\n```\nwhy does it fail on empty input?")
}

func TestSplitAskBoundsHugePaste(t *testing.T) {
	t.Parallel()
	huge := extractSmartV2(hugePasteReq(1<<20), cacheEndpointChat)
	assert.LessOrEqual(t, huge.codeBytes, smartAskTextCap, "a megabyte paste is cut before it is split")
	assert.Contains(t, huge.lower, "why does it fail on empty input", "the ask after the paste survives")
	assert.Contains(t, huge.lower, "please review this code", "the ask before the paste survives")

	big := extractSmartV2(hugePasteReq(2*smartAskTextCap), cacheEndpointChat)
	assert.Equal(t, smartBonusCodeCap*smartBonusCodeBytes <= huge.codeBytes, smartBonusCodeCap*smartBonusCodeBytes <= big.codeBytes, "both pastes saturate the code bonus")
}

func BenchmarkSplitAskHugePaste(b *testing.B) {
	req := hugePasteReq(1 << 20)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = extractSmartV2(req, cacheEndpointChat)
	}
}
