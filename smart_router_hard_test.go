package traefikllmgateway

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasSmartTermBoundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		text, term string
		want       bool
	}{
		{"implement raft in go", "raft|", true},
		{"a draft of the doc", "raft|", false},
		{"raftlog is not raft", "raft|", true},
		{"raftlog only", "raft|", false},
		{"linearizability proof", "linearizab", true},
		{"我们用raft做复制", "raft|", true},
		{"o(n log n) please", "o(n log", true},
		{"embrace change", "race condition", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, hasSmartTerm(c.text, c.term), "%q in %q", c.term, c.text)
	}
}

func hardFeatures(prose string) smartFeatures {
	return extractSmartV2(map[string]any{"messages": []any{map[string]any{"role": "user", "content": prose}}}, cacheEndpointChat)
}

func TestHardnessSignals(t *testing.T) {
	t.Parallel()
	rt := evalRouter(t, evalLayouts[0].mins, smartClassifierV2)
	cases := []struct {
		name, prose string
		rank        int
	}{
		{"terse consensus question", "short q: can raft elect two leaders in the same term", 2},
		{"native script with latin token", "我们的分布式事务用 TCC，请给出幂等设计", 2},
		{"arabic distributed terms", "صمم لي حلا بدون معاملات موزعة يضمن المعالجة مرة واحدة", 2},
		{"complexity with no wording", "make it O(n log n)", 1},
		{"easy ask names a scary subject", "explain the difference between a mutex and a semaphore", 0},
		{"greeting in german", "Hallo! Wie geht es dir heute?", 0},
		{"polish greeting", "Cześć! Co polecasz na szybki obiad?", 0},
		{"proofread is not a proof", "Proofread this: their going to the libary", 0},
	}
	for _, c := range cases {
		f := hardFeatures(c.prose)
		v := scoreSmartV2(rt, &f, rt.weights, smartSessionHint{})
		assert.Equal(t, c.rank, v.rank, c.name)
	}
}

func TestHardnessEasyAskIgnoresLogScare(t *testing.T) {
	t.Parallel()
	rt := evalRouter(t, evalLayouts[0].mins, smartClassifierV2)
	f := hardFeatures("which line is it?\n\npanic: runtime error: index out of range [5] with length 3\ngoroutine 1 [running]:\nmain.main()\n\t/app/main.go:12 +0x1d")
	v := scoreSmartV2(rt, &f, rt.weights, smartSessionHint{})
	assert.Zero(t, v.rank)
	assert.Positive(t, f.logBytes)
}

func TestHardnessLargeImplementationOutputNeedsTop(t *testing.T) {
	t.Parallel()
	rt := evalRouter(t, evalLayouts[0].mins, smartClassifierV2)
	req := map[string]any{"max_tokens": 16000, "messages": []any{map[string]any{"role": "user", "content": "implement a small tokenizer for our config language"}}}
	f := extractSmartV2(req, cacheEndpointChat)
	v := scoreSmartV2(rt, &f, rt.weights, smartSessionHint{})
	assert.Equal(t, 2, v.rank)
	req["max_tokens"] = 4096
	f = extractSmartV2(req, cacheEndpointChat)
	assert.Less(t, scoreSmartV2(rt, &f, rt.weights, smartSessionHint{}).rank, 2)
}

func toolStepRequest(ask, toolOut string) map[string]any {
	req := chatMsgs(
		msg("user", ask),
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c1", "type": "function"}}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": toolOut},
	)
	req["tools"] = []any{map[string]any{"type": "function"}}
	return req
}

func TestRoutineToolStepStaysLow(t *testing.T) {
	t.Parallel()
	for _, l := range evalLayouts {
		rt := evalRouter(t, l.mins, smartClassifierV2)
		for _, ask := range []string{"format the new files", "did the tests pass?", "update the changelog with the fix for #231", "delete the unused import in main.go"} {
			c := classifySmart(rt, toolStepRequest(ask, "ok"), cacheEndpointChat)
			assert.Zero(t, c.rank, "%s on %s", ask, l.name)
		}
	}
	routine := func(ask, out string) bool {
		f := extractSmartV2(toolStepRequest(ask, out), cacheEndpointChat)
		return f.routineToolStep()
	}
	assert.True(t, routine("format the new files", "ok"))
	assert.False(t, routine("format the new files", "panic: runtime error\ngoroutine 1 [running]:"), "an error result is not routine")
	assert.False(t, routine("check whether the raft log can diverge", "ok"), "a hard subject is not routine")
	assert.False(t, routine(strings.Repeat("port the sync script to go and keep the rate limiting ", 2), "ok"), "a long ask is not routine")
	f := extractSmartV2(chatMsgs(msg("user", "format the new files")), cacheEndpointChat)
	assert.False(t, f.routineToolStep(), "not a tool-loop turn")
}

func TestRewriteAskStaysLowWhateverTheMaterial(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("The design review approach changed and the new strategy was agreed after a long meeting about the schema of the report. ", 6)
	for _, l := range evalLayouts {
		rt := evalRouter(t, l.mins, smartClassifierV2)
		rank := func(text string) int { return classifySmart(rt, chatMsgs(msg("user", text)), cacheEndpointChat).rank }
		assert.Zero(t, rank("Summarize this in 3 bullets:\n\n"+body), l.name)
		assert.Zero(t, rank("make this sound less angry: 'your team broke prod again and nobody told us'"), l.name)
		assert.Zero(t, rank("fix grammar only, dont change meaning: 'me and him goes to the store yesterday'"), l.name)
		assert.Positive(t, rank("Summarize the options below and recommend one:\n\n"+body), "a judgement ask is not a plain rewrite on "+l.name)
		assert.Positive(t, rank("Rewrite this legal clause in plain english but keep every obligation exactly: 'Licensee shall not sublicense'"), "fidelity-critical text on "+l.name)
		assert.Positive(t, rank("Summarize this and prove that the bound holds:\n\n"+body), "a math ask is not a plain rewrite on "+l.name)
	}
}

func TestShortForeignQuestionIsChat(t *testing.T) {
	t.Parallel()
	for _, l := range evalLayouts {
		rt := evalRouter(t, l.mins, smartClassifierV2)
		rank := func(text string) int { return classifySmart(rt, chatMsgs(msg("user", text)), cacheEndpointChat).rank }
		assert.Zero(t, rank("ei, como faço pra pegar o ultimo item de uma lista em python? e o penultimo"), l.name)
		assert.Zero(t, rank("מה ההבדל בין TCP ל-UDP בקצרה?"), l.name)
		assert.Positive(t, rank("Warum stürzt mein Programm ab?"), "a failure question is not chat on "+l.name)
		assert.Positive(t, rank("Dlaczego mój program w Go się zawiesza? Dwie gorutyny, jedna trzyma mutex A i czeka na B"), l.name)
	}
	f := hardFeatures("what is the capital of france? and what time is it there")
	assert.False(t, f.shortForeignQuestion(), "English words keep the English path")
	long := hardFeatures(strings.Repeat("¿Cómo se hace esto bien? ", 5))
	assert.False(t, long.shortForeignQuestion(), "a long question is not short chat")
	greeting := func(text string) bool {
		g := hardFeatures(text)
		return g.isGreeting()
	}
	assert.True(t, greeting("你好吗？"))
	assert.True(t, greeting("thanks a lot"))
	assert.False(t, greeting("and the leader?"))
}

// toolRun builds a chat tool loop whose tool results come in the given order, oldest first.
func toolRun(ask string, outs ...string) map[string]any {
	msgs := []any{msg("user", ask)}
	for _, out := range outs {
		msgs = append(msgs,
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "c", "type": "function"}}},
			map[string]any{"role": "tool", "tool_call_id": "c", "content": out})
	}
	req := chatMsgs(msgs...)
	req["tools"] = []any{map[string]any{"type": "function"}}
	return req
}

func TestRecentToolFailureIsADebugSignal(t *testing.T) {
	t.Parallel()
	fails := []string{
		"WARNING: DATA RACE\nWrite at 0x00c0001 by goroutine 9", "--- FAIL: TestPool (0.01s)", "FAIL\texample.com/pool\t0.1s", "./a.go:3:2: undefined: Foo",
		"cannot use x (variable of type int) as string value", "src/repo.ts(22,30): error TS2322: bad", "context deadline exceeded",
		"goroutine 5 [chan receive, 10 minutes]:", "goroutine 7 [semacquire]:", "AssertionError: 1 != 2", "test timed out after 10m0s",
	}
	for _, l := range evalLayouts {
		rt := evalRouter(t, l.mins, smartClassifierV2)
		for _, out := range fails {
			// The failure sits two results back; the newest result passes.
			c := classifySmart(rt, toolRun("ok go", out, "read ok", "read ok"), cacheEndpointChat)
			assert.GreaterOrEqual(t, c.rank, 1, "%q on %s", out, l.name)
			assert.Less(t, c.rank, 2, "%q on %s", out, l.name)
		}
		c := classifySmart(rt, toolRun("ok go", "ok", "read ok", "read ok"), cacheEndpointChat)
		assert.Zero(t, c.rank, "passing output stays routine on %s", l.name)
		c = classifySmart(rt, toolRun("ok go", fails[0], "read ok", "read ok", "read ok"), cacheEndpointChat)
		assert.Zero(t, c.rank, "a failure older than the newest three results is ignored on %s", l.name)
	}
	f := extractSmartV2(toolRun("ok go", fails[1], "read ok"), cacheEndpointChat)
	assert.True(t, f.toolFail)
	assert.False(t, f.toolErr, "toolErr still reads only the newest result")
	assert.False(t, f.routineToolStep())
}

func TestTerseFailureAndReviewCues(t *testing.T) {
	t.Parallel()
	for _, ask := range []string{"go test -race keeps flagging this", "build is red after the generics change", "integration tests hang on ci",
		"the job is stuck again", "this test is flaky", "can you sort it out", "find out why", "review this PR for auth issues", "review the security of the login"} {
		assert.True(t, isSmartFailureAsk(ask), ask)
	}
	for _, ask := range []string{"change the color", "review this poem", "the author wrote a book", "the red house", "review the pricing page"} {
		assert.False(t, isSmartFailureAsk(ask), ask)
	}
}

func TestDesignDepthTermsRaiseTheTier(t *testing.T) {
	t.Parallel()
	rt := evalRouter(t, evalLayouts[0].mins, smartClassifierV2)
	for ask, want := range map[string]int{
		"threat model the device flow for our cli":           2,
		"leader election for a job scheduler with postgres":  2,
		"how to version a public REST api for 300 customers": 1,
	} {
		c := classifySmart(rt, chatMsgs(msg("user", ask)), cacheEndpointChat)
		assert.GreaterOrEqual(t, c.rank, want, ask)
	}
	c := classifySmart(rt, chatMsgs(msg("user", "what is a lease agreement")), cacheEndpointChat)
	assert.LessOrEqual(t, c.rank, 1, "one weak term raises at most to the middle tier")
}

func TestRewriteWithFormConstraintNeedsMiddleTier(t *testing.T) {
	t.Parallel()
	for _, l := range evalLayouts {
		rt := evalRouter(t, l.mins, smartClassifierV2)
		c := classifySmart(rt, chatMsgs(msg("user", "translate this poem to spanish keeping the rhyme and meter:\n\nThe evening settles soft and slow,\nthe river hums a quiet tune.")), cacheEndpointChat)
		assert.GreaterOrEqual(t, c.rank, 1, l.name)
		c = classifySmart(rt, chatMsgs(msg("user", "translate this to spanish: the parameter list is empty")), cacheEndpointChat)
		assert.Zero(t, c.rank, "parameter is not meter on %s", l.name)
	}
}
