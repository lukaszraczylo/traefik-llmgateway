package traefikllmgateway

import (
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
