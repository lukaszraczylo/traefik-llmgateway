package main

import (
	"go/parser"
	"go/token"
	"reflect"
	"strings"
	"testing"
)

// fixtureJSON is a small, hand-built stand-in for LiteLLM's real
// model_prices_and_context_window.json, covering every branch of the
// pruning rule documented on this package (main.go's own doc comment):
// the sample_spec schema-example entry, a disallowed provider, a
// disallowed mode, null cost fields, a bare-vs-provider-prefixed
// collision with identical values, a three-way collision with
// DIFFERING values across providers (deepseek-v4-flash, mirroring the
// real dashscope/deepseek disagreement this tool was built against), an
// xai entry with float-valued (not int) max_input_tokens/max_tokens, an
// embedding-mode entry with a genuine zero output cost, and an entry
// with no max_input_tokens/max_tokens at all.
const fixtureJSON = `{
  "sample_spec": {
    "litellm_provider": "one of https://docs.litellm.ai/docs/providers",
    "input_cost_per_token": 0.0,
    "output_cost_per_token": 0.0,
    "max_input_tokens": "max input tokens, if the provider specifies it."
  },
  "gpt-5": {
    "litellm_provider": "openai", "mode": "chat",
    "input_cost_per_token": 1.25e-06, "output_cost_per_token": 1.0e-05,
    "max_input_tokens": 400000, "max_tokens": 128000
  },
  "bedrock/some-model": {
    "litellm_provider": "bedrock", "mode": "chat",
    "input_cost_per_token": 1e-06, "output_cost_per_token": 1e-06,
    "max_input_tokens": 100000
  },
  "openai/dalle": {
    "litellm_provider": "openai", "mode": "image_generation",
    "output_cost_per_image": 0.04
  },
  "openai/null-cost": {
    "litellm_provider": "openai", "mode": "chat",
    "input_cost_per_token": null, "output_cost_per_token": null,
    "max_input_tokens": 1000
  },
  "deepseek-chat": {
    "litellm_provider": "deepseek", "mode": "chat",
    "input_cost_per_token": 2.8e-07, "output_cost_per_token": 4.2e-07,
    "max_input_tokens": 131072, "max_tokens": 8192
  },
  "deepseek/deepseek-chat": {
    "litellm_provider": "deepseek", "mode": "chat",
    "input_cost_per_token": 2.8e-07, "output_cost_per_token": 4.2e-07,
    "max_input_tokens": 131072, "max_tokens": 8192
  },
  "dashscope/deepseek-v4-flash": {
    "litellm_provider": "dashscope", "mode": "chat",
    "input_cost_per_token": 2e-07, "output_cost_per_token": 4e-07,
    "max_input_tokens": 1000000
  },
  "deepseek-v4-flash": {
    "litellm_provider": "deepseek", "mode": "chat",
    "input_cost_per_token": 4.4e-07, "output_cost_per_token": 1.32e-06,
    "max_input_tokens": 131072
  },
  "deepseek/deepseek-v4-flash": {
    "litellm_provider": "deepseek", "mode": "chat",
    "input_cost_per_token": 4.4e-07, "output_cost_per_token": 1.32e-06,
    "max_input_tokens": 131072
  },
  "xai/grok-4-fast-reasoning": {
    "litellm_provider": "xai", "mode": "chat",
    "input_cost_per_token": 2e-07, "output_cost_per_token": 5e-07,
    "max_input_tokens": 2000000.0, "max_tokens": 2000000.0
  },
  "openai/whisper-notmode": {
    "litellm_provider": "openai", "mode": "audio_transcription",
    "input_cost_per_token": 1e-06, "output_cost_per_token": 1e-06,
    "max_input_tokens": 16000
  },
  "text-embedding-3-large": {
    "litellm_provider": "openai", "mode": "embedding",
    "input_cost_per_token": 1.3e-07, "output_cost_per_token": 0.0,
    "max_input_tokens": 8191, "max_tokens": 8191
  },
  "openai/no-max-tokens-at-all": {
    "litellm_provider": "openai", "mode": "chat",
    "input_cost_per_token": 1e-06, "output_cost_per_token": 2e-06
  },
  "minimax/MiniMax-M2.1-lightning": {
    "litellm_provider": "minimax", "mode": "chat",
    "input_cost_per_token": 3e-07, "output_cost_per_token": 2.4e-06,
    "max_input_tokens": 1000000
  },
  "claude-haiku-4-5": {
    "litellm_provider": "anthropic", "mode": "chat",
    "input_cost_per_token": 1e-06, "output_cost_per_token": 5e-06,
    "max_input_tokens": 200000, "max_tokens": 64000
  },
  "moonshot/kimi-k2.5": {
    "litellm_provider": "moonshot", "mode": "chat",
    "input_cost_per_token": 6e-07, "output_cost_per_token": 2.5e-06,
    "max_input_tokens": 262144
  }
}`

// fixtureUpstreamKeyCount is fixtureJSON's total top-level key count
// (including every pruned-out entry and sample_spec) — parseAndPrune's
// second return value.
const fixtureUpstreamKeyCount = 17

// TestParseAndPrune_Golden is the generator's golden test (task
// requirement): a small fixture JSON in, an exact expected pruned table
// out, covering every pruning-rule branch fixtureJSON's own doc comment
// lists.
func TestParseAndPrune_Golden(t *testing.T) {
	got, totalUpstream, err := parseAndPrune([]byte(fixtureJSON))
	if err != nil {
		t.Fatalf("parseAndPrune: %v", err)
	}
	if totalUpstream != fixtureUpstreamKeyCount {
		t.Errorf("totalUpstream = %d, want %d", totalUpstream, fixtureUpstreamKeyCount)
	}

	want := map[string]builtinEntry{
		"gpt-5": {ContextTokens: 400000, InputCostPerMTokMicroUSD: 1_250_000, OutputCostPerMTokMicroUSD: 10_000_000},
		// deepseek-chat: bare key wins over the identical-valued
		// "deepseek/deepseek-chat" prefixed key (rule: bare beats
		// prefixed regardless of value equality).
		"deepseek-chat": {ContextTokens: 131072, InputCostPerMTokMicroUSD: 280_000, OutputCostPerMTokMicroUSD: 420_000},
		// deepseek-v4-flash: three colliding keys, DIFFERING values
		// between "dashscope/..." and the deepseek-provider ones — the
		// bare "deepseek-v4-flash" key wins (rule 1: bare beats
		// prefixed), not the higher- or lower-priced alternative.
		"deepseek-v4-flash":     {ContextTokens: 131072, InputCostPerMTokMicroUSD: 440_000, OutputCostPerMTokMicroUSD: 1_320_000},
		"grok-4-fast-reasoning": {ContextTokens: 2_000_000, InputCostPerMTokMicroUSD: 200_000, OutputCostPerMTokMicroUSD: 500_000},
		// text-embedding-3-large: a genuine, known zero output cost
		// (embeddings bill input tokens only) must survive, not be
		// treated as "missing".
		"text-embedding-3-large": {ContextTokens: 8191, InputCostPerMTokMicroUSD: 130_000, OutputCostPerMTokMicroUSD: 0},
		// no-max-tokens-at-all: neither max_input_tokens nor max_tokens
		// present at all -> ContextTokens 0 (unknown), cost still kept.
		"no-max-tokens-at-all": {ContextTokens: 0, InputCostPerMTokMicroUSD: 1_000_000, OutputCostPerMTokMicroUSD: 2_000_000},
		// MiniMax-M2.1-lightning: LiteLLM's own entry, kept as-is.
		"MiniMax-M2.1-lightning": {ContextTokens: 1_000_000, InputCostPerMTokMicroUSD: 300_000, OutputCostPerMTokMicroUSD: 2_400_000},
		// MiniMax-M2.1-highspeed: the naming-bridge duplicate (generator
		// addendum) — identical values, no LiteLLM entry of its own in
		// this fixture.
		"MiniMax-M2.1-highspeed": {ContextTokens: 1_000_000, InputCostPerMTokMicroUSD: 300_000, OutputCostPerMTokMicroUSD: 2_400_000},
		// Coverage-audit sanity (addendum, 2026-08): openai-family and
		// anthropic bare ids resolve as LiteLLM's own canonical bare
		// keys, with no bridging or prefix-stripping needed — "gpt-5"
		// above already covers the openai family; this covers anthropic.
		"claude-haiku-4-5": {ContextTokens: 200_000, InputCostPerMTokMicroUSD: 1_000_000, OutputCostPerMTokMicroUSD: 5_000_000},
		// moonshot: Kimi models (coverage-audit addendum) — a
		// provider-prefixed key, bareModelID strips "moonshot/" the same
		// way it strips any other allowlisted provider's own prefix.
		"kimi-k2.5": {ContextTokens: 262144, InputCostPerMTokMicroUSD: 600_000, OutputCostPerMTokMicroUSD: 2_500_000},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseAndPrune(fixtureJSON) =\n%#v\nwant\n%#v", got, want)
	}
}

// TestRenderGoSource_ValidSortedGo asserts renderGoSource produces
// syntactically valid, gofmt-canonical Go source (parseable by go/
// parser) whose entries appear in sorted key order, and that its header
// documents the pruning provenance (source URL, content digest, counts).
func TestRenderGoSource_ValidSortedGo(t *testing.T) {
	table := map[string]builtinEntry{
		"zzz-model": {ContextTokens: 1, InputCostPerMTokMicroUSD: 1, OutputCostPerMTokMicroUSD: 1},
		"aaa-model": {ContextTokens: 2, InputCostPerMTokMicroUSD: 2, OutputCostPerMTokMicroUSD: 2},
	}
	src, err := renderGoSource(table, sourceMeta{sourceDigest: "deadbeefcafe", totalUpstream: 3111, kept: 2})
	if err != nil {
		t.Fatalf("renderGoSource: %v", err)
	}

	if _, err := parser.ParseFile(token.NewFileSet(), "pricing_data_gen.go", src, parser.AllErrors); err != nil {
		t.Fatalf("generated source does not parse as valid Go: %v\n%s", err, src)
	}
	if !strings.Contains(src, "package traefikllmgateway") {
		t.Error("generated source missing package declaration")
	}
	if !strings.Contains(src, litellmSourceURL) {
		t.Error("generated source missing source URL provenance comment")
	}
	if !strings.Contains(src, "deadbeefcafe") || !strings.Contains(src, "3111") {
		t.Error("generated source missing source-digest / upstream count provenance comment")
	}
	if got, want := strings.Index(src, `"aaa-model"`), strings.Index(src, `"zzz-model"`); got == -1 || want == -1 || got > want {
		t.Errorf(`entries not sorted: index("aaa-model")=%d, index("zzz-model")=%d, want aaa before zzz`, got, want)
	}
}

// TestChooseWinner_Deterministic asserts chooseWinner's tiebreak order
// (bare > provider priority > lexicographic full key) directly, beyond
// what the golden test above exercises indirectly.
func TestChooseWinner_Deterministic(t *testing.T) {
	cands := []candidate{
		{fullKey: "dashscope/x", provider: "dashscope", bare: false, entry: builtinEntry{ContextTokens: 1}},
		{fullKey: "deepseek/x", provider: "deepseek", bare: false, entry: builtinEntry{ContextTokens: 2}},
		{fullKey: "x", provider: "deepseek", bare: true, entry: builtinEntry{ContextTokens: 3}},
	}
	if got := chooseWinner(cands); got.entry.ContextTokens != 3 {
		t.Errorf("chooseWinner picked ContextTokens=%d, want 3 (the bare candidate)", got.entry.ContextTokens)
	}

	// With no bare candidate, provider priority decides: "deepseek"
	// precedes "dashscope" in providerPriority.
	noBare := []candidate{
		{fullKey: "dashscope/x", provider: "dashscope", bare: false, entry: builtinEntry{ContextTokens: 1}},
		{fullKey: "deepseek/x", provider: "deepseek", bare: false, entry: builtinEntry{ContextTokens: 2}},
	}
	if got := chooseWinner(noBare); got.entry.ContextTokens != 2 {
		t.Errorf("chooseWinner picked ContextTokens=%d, want 2 (deepseek outranks dashscope)", got.entry.ContextTokens)
	}
}

// TestApplyNamingBridges_DoesNotOverwriteExistingEntry asserts a bridged
// id that LiteLLM already prices directly (its own independent winning
// candidate) is left untouched by the bridge — an entry LiteLLM prices
// directly always wins over a bridged duplicate (generator addendum).
func TestApplyNamingBridges_DoesNotOverwriteExistingEntry(t *testing.T) {
	winners := map[string]candidate{
		"MiniMax-M2.1-lightning": {provider: "minimax", entry: builtinEntry{ContextTokens: 1_000_000, InputCostPerMTokMicroUSD: 300_000, OutputCostPerMTokMicroUSD: 2_400_000}},
		// A directly-priced, independent "-highspeed" entry with a
		// DIFFERENT value than the lightning entry would bridge to.
		"MiniMax-M2.1-highspeed": {provider: "minimax", entry: builtinEntry{ContextTokens: 500_000, InputCostPerMTokMicroUSD: 999, OutputCostPerMTokMicroUSD: 999}},
	}
	table := make(map[string]builtinEntry, len(winners))
	for bare, w := range winners {
		table[bare] = w.entry
	}
	applyNamingBridges(table, winners)

	want := builtinEntry{ContextTokens: 500_000, InputCostPerMTokMicroUSD: 999, OutputCostPerMTokMicroUSD: 999}
	if got := table["MiniMax-M2.1-highspeed"]; got != want {
		t.Errorf(`table["MiniMax-M2.1-highspeed"] = %+v, want %+v (its own direct LiteLLM entry, not the bridged duplicate)`, got, want)
	}
}

// TestContentDigest covers the review-fix replacement for a wall-clock
// snapshot date: identical bytes must always produce an identical
// digest (the whole point — a `make pricing-sync` re-run against
// unchanged upstream data must reproduce a byte-identical file, with no
// spurious diff), different bytes must produce a different digest, and
// the digest is always exactly contentDigestLen hex characters.
func TestContentDigest(t *testing.T) {
	a := contentDigest([]byte(`{"gpt-5":{}}`))
	b := contentDigest([]byte(`{"gpt-5":{}}`))
	c := contentDigest([]byte(`{"gpt-5":{"changed":true}}`))

	if a != b {
		t.Errorf("contentDigest is not deterministic: %q != %q for identical input", a, b)
	}
	if a == c {
		t.Errorf("contentDigest(%q) == contentDigest(%q) = %q, want different digests for different input", `{"gpt-5":{}}`, `{"gpt-5":{"changed":true}}`, a)
	}
	if len(a) != contentDigestLen {
		t.Errorf("len(contentDigest(...)) = %d, want %d", len(a), contentDigestLen)
	}
}
