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
