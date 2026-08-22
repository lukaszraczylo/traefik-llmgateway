package traefikllmgateway

import (
	"strings"
	"testing"
)

// TestResolveModelMeta_Layering is the feature's core table-driven test:
// config override > discovered > builtin > absent, checked independently
// for context and for cost (discovery never reports cost, so cost only
// has three layers).
func TestResolveModelMeta_Layering(t *testing.T) {
	const provider = "openai"
	const model = "test-model"

	cases := []struct {
		cfg             map[string]*ModelMetaConfig
		name            string
		discoveredCtx   int
		wantCtx         int
		wantInput       int64
		wantOutput      int64
		discoveredKnown bool
		wantCtxKnown    bool
		wantCostKnown   bool
	}{
		{
			name:         "everything absent",
			wantCtxKnown: false,
		},
		{
			name:            "discovered context only, no cost anywhere",
			discoveredCtx:   4096,
			discoveredKnown: true,
			wantCtx:         4096,
			wantCtxKnown:    true,
		},
		{
			name: "config override wins over discovered",
			cfg: map[string]*ModelMetaConfig{
				model: {ContextTokens: 8192},
			},
			discoveredCtx:   4096,
			discoveredKnown: true,
			wantCtx:         8192,
			wantCtxKnown:    true,
		},
		{
			name: "config exact provider/model key wins over bare key",
			cfg: map[string]*ModelMetaConfig{
				model:                  {ContextTokens: 1000},
				provider + "/" + model: {ContextTokens: 2000},
			},
			wantCtx:      2000,
			wantCtxKnown: true,
		},
		{
			name: "config override sets cost, discovery ignored for cost",
			cfg: map[string]*ModelMetaConfig{
				model: {InputCostPerMTokMicroUSD: 100, OutputCostPerMTokMicroUSD: 200},
			},
			wantInput:     100,
			wantOutput:    200,
			wantCostKnown: true,
		},
		{
			name: "free config override zeroes cost explicitly, known",
			cfg: map[string]*ModelMetaConfig{
				model: {Free: true},
			},
			wantInput:     0,
			wantOutput:    0,
			wantCostKnown: true,
		},
		{
			name:          "builtin table used when nothing else known",
			wantCtx:       0, // "test-model" is not in builtinModelMetaTable
			wantCostKnown: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveModelMeta(provider, model, c.cfg, c.discoveredCtx, c.discoveredKnown)
			if got.ContextKnown != c.wantCtxKnown || got.ContextTokens != c.wantCtx {
				t.Errorf("context = (%d, known=%v), want (%d, known=%v)", got.ContextTokens, got.ContextKnown, c.wantCtx, c.wantCtxKnown)
			}
			if got.CostKnown != c.wantCostKnown || got.InputCostPerMTokMicroUSD != c.wantInput || got.OutputCostPerMTokMicroUSD != c.wantOutput {
				t.Errorf("cost = (%d, %d, known=%v), want (%d, %d, known=%v)",
					got.InputCostPerMTokMicroUSD, got.OutputCostPerMTokMicroUSD, got.CostKnown,
					c.wantInput, c.wantOutput, c.wantCostKnown)
			}
		})
	}
}

// TestResolveModelMeta_BuiltinTable exercises the real, generated
// builtinModelMetaTable directly (pricing_data_gen.go) for both context
// and cost, and confirms an operator's config override still wins over
// it.
func TestResolveModelMeta_BuiltinTable(t *testing.T) {
	const bareID = "deepseek-chat" // a stable, real LiteLLM entry (deepseek's own bare key)
	builtin, ok := builtinModelMetaTable[bareID]
	if !ok || builtin.ContextTokens <= 0 || builtin.InputCostPerMTokMicroUSD <= 0 {
		t.Fatalf("builtinModelMetaTable[%q] = %+v, ok=%v — test fixture assumption broke, pick another stable id", bareID, builtin, ok)
	}

	got := resolveModelMeta("acme", bareID, nil, 0, false)
	if !got.ContextKnown || got.ContextTokens != builtin.ContextTokens {
		t.Errorf("context = (%d, known=%v), want (%d, known=true)", got.ContextTokens, got.ContextKnown, builtin.ContextTokens)
	}
	if !got.CostKnown || got.InputCostPerMTokMicroUSD != builtin.InputCostPerMTokMicroUSD || got.OutputCostPerMTokMicroUSD != builtin.OutputCostPerMTokMicroUSD {
		t.Errorf("cost = (%d, %d, known=%v), want (%d, %d, known=true)",
			got.InputCostPerMTokMicroUSD, got.OutputCostPerMTokMicroUSD, got.CostKnown,
			builtin.InputCostPerMTokMicroUSD, builtin.OutputCostPerMTokMicroUSD)
	}

	overridden := resolveModelMeta("acme", bareID, map[string]*ModelMetaConfig{bareID: {ContextTokens: 1}}, 0, false)
	if overridden.ContextTokens != 1 {
		t.Errorf("config override over builtin: context = %d, want 1", overridden.ContextTokens)
	}
	// Cost is untouched by the context-only override — still falls
	// through to the builtin table (independent per-field layering).
	if overridden.InputCostPerMTokMicroUSD != builtin.InputCostPerMTokMicroUSD {
		t.Errorf("cost should still fall through to builtin when override sets only context, got %d, want %d", overridden.InputCostPerMTokMicroUSD, builtin.InputCostPerMTokMicroUSD)
	}
}

// TestResolveModelMeta_FreeTierSuffix covers freeTierSuffix's layer
// (review addendum): a ":free"-suffixed id is free by naming convention
// alone, ranked above the built-in table but below an operator's own
// config override, and never invents a context window.
func TestResolveModelMeta_FreeTierSuffix(t *testing.T) {
	t.Run("free-by-suffix wins over a builtin paid entry under the identical key", func(t *testing.T) {
		const id = "review-fixture-model:free"
		t.Cleanup(func() { delete(builtinModelMetaTable, id) })
		builtinModelMetaTable[id] = builtinModelMeta{ContextTokens: 999, InputCostPerMTokMicroUSD: 5_000_000, OutputCostPerMTokMicroUSD: 5_000_000}

		got := resolveModelMeta("openrouter", id, nil, 0, false)
		if !got.CostKnown || got.InputCostPerMTokMicroUSD != 0 || got.OutputCostPerMTokMicroUSD != 0 {
			t.Errorf("cost = (%d, %d, known=%v), want (0, 0, true) — free-by-suffix must beat a builtin paid entry", got.InputCostPerMTokMicroUSD, got.OutputCostPerMTokMicroUSD, got.CostKnown)
		}
		// Context still resolves independently — the builtin entry's own
		// ContextTokens is a real, known value for this exact key, so it
		// is NOT suppressed by the free-tier cost rule (freeTierSuffix
		// affects cost only).
		if !got.ContextKnown || got.ContextTokens != 999 {
			t.Errorf("context = (%d, known=%v), want (999, true) — freeTierSuffix must not touch context resolution", got.ContextTokens, got.ContextKnown)
		}
	})

	t.Run("free-by-suffix never invents context when nothing else knows it", func(t *testing.T) {
		got := resolveModelMeta("openrouter", "some-model-nobody-prices:free", nil, 0, false)
		if got.ContextKnown {
			t.Errorf("context = %+v, want unknown — free-by-suffix must never invent a context window", got)
		}
		if !got.CostKnown || got.InputCostPerMTokMicroUSD != 0 || got.OutputCostPerMTokMicroUSD != 0 {
			t.Errorf("cost = %+v, want known zero", got)
		}
	})

	t.Run("an explicit operator override still beats free-by-suffix", func(t *testing.T) {
		const id = "review-fixture-model-2:free"
		cfg := map[string]*ModelMetaConfig{id: {InputCostPerMTokMicroUSD: 1, OutputCostPerMTokMicroUSD: 2}}
		got := resolveModelMeta("openrouter", id, cfg, 0, false)
		if !got.CostKnown || got.InputCostPerMTokMicroUSD != 1 || got.OutputCostPerMTokMicroUSD != 2 {
			t.Errorf("cost = (%d, %d, known=%v), want (1, 2, true) — an explicit operator override must win over the free-by-suffix rule", got.InputCostPerMTokMicroUSD, got.OutputCostPerMTokMicroUSD, got.CostKnown)
		}
	})

	t.Run("a non-free config override still beats free-by-suffix even at zero context", func(t *testing.T) {
		const id = "review-fixture-model-3:free"
		cfg := map[string]*ModelMetaConfig{id: {Free: false, InputCostPerMTokMicroUSD: 0, OutputCostPerMTokMicroUSD: 0}}
		// A present-but-all-zero, non-Free override entry sets nothing at
		// this layer (matches ModelMetaConfig's own "zero falls through"
		// contract) — the free-tier suffix rule still applies underneath
		// it, not the (absent) override.
		got := resolveModelMeta("openrouter", id, cfg, 0, false)
		if !got.CostKnown || got.InputCostPerMTokMicroUSD != 0 {
			t.Errorf("cost = %+v, want known zero via the free-by-suffix fallback", got)
		}
	})

	t.Run("an id without the suffix is unaffected", func(t *testing.T) {
		got := resolveModelMeta("openrouter", "plain-model-id", nil, 0, false)
		if got.CostKnown {
			t.Errorf("cost = %+v, want unknown for a plain id with no :free suffix and no other layer", got)
		}
	})
}

// TestLookupModelMetaConfig covers lookupModelMetaConfig's own
// precedence and nil-safety directly.
func TestLookupModelMetaConfig(t *testing.T) {
	t.Run("nil cfg", func(t *testing.T) {
		if got := lookupModelMetaConfig("p", "m", nil); got != nil {
			t.Errorf("got %+v, want nil", got)
		}
	})
	t.Run("nil entry treated as absent", func(t *testing.T) {
		cfg := map[string]*ModelMetaConfig{"m": nil}
		if got := lookupModelMetaConfig("p", "m", cfg); got != nil {
			t.Errorf("got %+v, want nil (nil map value must not be dereferenced)", got)
		}
	})
	t.Run("bare key match", func(t *testing.T) {
		want := &ModelMetaConfig{ContextTokens: 5}
		cfg := map[string]*ModelMetaConfig{"m": want}
		if got := lookupModelMetaConfig("p", "m", cfg); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
	t.Run("exact provider/model beats bare", func(t *testing.T) {
		bare := &ModelMetaConfig{ContextTokens: 1}
		exact := &ModelMetaConfig{ContextTokens: 2}
		cfg := map[string]*ModelMetaConfig{"m": bare, "p/m": exact}
		if got := lookupModelMetaConfig("p", "m", cfg); got != exact {
			t.Errorf("got %+v, want the exact-key entry %+v", got, exact)
		}
	})
}

// TestResolveAliasModelMeta_Inheritance asserts an alias inherits its
// target's fully resolved metadata, and that an alias-level modelMeta
// entry overrides the inherited values independently per field.
func TestResolveAliasModelMeta_Inheritance(t *testing.T) {
	const alias = "aliased/coding"
	const targetProvider = "anthropic"
	const targetModel = "claude-x"

	t.Run("no alias entry: inherits target fully", func(t *testing.T) {
		cfg := map[string]*ModelMetaConfig{
			targetProvider + "/" + targetModel: {ContextTokens: 200000, InputCostPerMTokMicroUSD: 3_000_000, OutputCostPerMTokMicroUSD: 15_000_000},
		}
		got := resolveAliasModelMeta(alias, targetProvider, targetModel, cfg, 0, false)
		if got.ContextTokens != 200000 || !got.ContextKnown {
			t.Errorf("context = (%d, %v), want (200000, true)", got.ContextTokens, got.ContextKnown)
		}
		if got.InputCostPerMTokMicroUSD != 3_000_000 || got.OutputCostPerMTokMicroUSD != 15_000_000 || !got.CostKnown {
			t.Errorf("cost = (%d, %d, %v), want (3000000, 15000000, true)", got.InputCostPerMTokMicroUSD, got.OutputCostPerMTokMicroUSD, got.CostKnown)
		}
	})

	t.Run("alias entry overrides context only, cost still inherited", func(t *testing.T) {
		cfg := map[string]*ModelMetaConfig{
			targetProvider + "/" + targetModel: {ContextTokens: 200000, InputCostPerMTokMicroUSD: 3_000_000, OutputCostPerMTokMicroUSD: 15_000_000},
			alias:                              {ContextTokens: 999},
		}
		got := resolveAliasModelMeta(alias, targetProvider, targetModel, cfg, 0, false)
		if got.ContextTokens != 999 {
			t.Errorf("context = %d, want 999 (alias override)", got.ContextTokens)
		}
		if got.InputCostPerMTokMicroUSD != 3_000_000 || !got.CostKnown {
			t.Errorf("cost should still be inherited from target: got (%d, known=%v)", got.InputCostPerMTokMicroUSD, got.CostKnown)
		}
	})

	t.Run("alias entry sets free, overrides inherited cost", func(t *testing.T) {
		cfg := map[string]*ModelMetaConfig{
			targetProvider + "/" + targetModel: {InputCostPerMTokMicroUSD: 3_000_000, OutputCostPerMTokMicroUSD: 15_000_000},
			alias:                              {Free: true},
		}
		got := resolveAliasModelMeta(alias, targetProvider, targetModel, cfg, 0, false)
		if !got.CostKnown || got.InputCostPerMTokMicroUSD != 0 || got.OutputCostPerMTokMicroUSD != 0 {
			t.Errorf("cost = (%d, %d, known=%v), want (0, 0, true)", got.InputCostPerMTokMicroUSD, got.OutputCostPerMTokMicroUSD, got.CostKnown)
		}
	})

	t.Run("nil cfg never panics", func(t *testing.T) {
		got := resolveAliasModelMeta(alias, targetProvider, targetModel, nil, 0, false)
		if got.ContextKnown || got.CostKnown {
			t.Errorf("got %+v, want everything unknown", got)
		}
	})

	t.Run("nil alias entry value treated as absent, still inherits", func(t *testing.T) {
		cfg := map[string]*ModelMetaConfig{
			targetProvider + "/" + targetModel: {ContextTokens: 111},
			alias:                              nil,
		}
		got := resolveAliasModelMeta(alias, targetProvider, targetModel, cfg, 0, false)
		if got.ContextTokens != 111 || !got.ContextKnown {
			t.Errorf("got %+v, want inherited ContextTokens=111 (nil alias entry must not panic or override)", got)
		}
	})

	t.Run("alias entry sets non-free cost directly, overrides inherited cost", func(t *testing.T) {
		cfg := map[string]*ModelMetaConfig{
			targetProvider + "/" + targetModel: {InputCostPerMTokMicroUSD: 1, OutputCostPerMTokMicroUSD: 2},
			alias:                              {InputCostPerMTokMicroUSD: 100, OutputCostPerMTokMicroUSD: 200},
		}
		got := resolveAliasModelMeta(alias, targetProvider, targetModel, cfg, 0, false)
		if !got.CostKnown || got.InputCostPerMTokMicroUSD != 100 || got.OutputCostPerMTokMicroUSD != 200 {
			t.Errorf("got cost=(%d, %d, known=%v), want (100, 200, true) — alias's own non-free cost override", got.InputCostPerMTokMicroUSD, got.OutputCostPerMTokMicroUSD, got.CostKnown)
		}
	})
}

// TestMicroUSDPerMTokToUSD pins the currency conversion.
func TestMicroUSDPerMTokToUSD(t *testing.T) {
	cases := []struct {
		micro int64
		want  float64
	}{
		{0, 0},
		{1_250_000, 1.25},
		{10_000_000, 10},
	}
	for _, c := range cases {
		if got := microUSDPerMTokToUSD(c.micro); got != c.want {
			t.Errorf("microUSDPerMTokToUSD(%d) = %v, want %v", c.micro, got, c.want)
		}
	}
}

// TestValidateModelMeta covers construction-time validation: the
// zero-behavior-change nil/empty case, every rejection rule, and a
// deterministic first-error-wins ordering across multiple bad entries.
func TestValidateModelMeta(t *testing.T) {
	t.Run("nil/empty preserves prior behavior", func(t *testing.T) {
		got, err := validateModelMeta(nil)
		if err != nil || got != nil {
			t.Errorf("validateModelMeta(nil) = (%v, %v), want (nil, nil)", got, err)
		}
		got, err = validateModelMeta(map[string]*ModelMetaConfig{})
		if err != nil || got != nil {
			t.Errorf("validateModelMeta({}) = (%v, %v), want (nil, nil)", got, err)
		}
	})

	t.Run("valid entries pass through unchanged", func(t *testing.T) {
		raw := map[string]*ModelMetaConfig{
			"m1": {ContextTokens: 100, InputCostPerMTokMicroUSD: 1, OutputCostPerMTokMicroUSD: 2},
			"m2": {Free: true},
		}
		got, err := validateModelMeta(raw)
		if err != nil {
			t.Fatalf("validateModelMeta: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d entries, want 2", len(got))
		}
	})

	t.Run("nil entry rejected", func(t *testing.T) {
		_, err := validateModelMeta(map[string]*ModelMetaConfig{"m": nil})
		if err == nil {
			t.Fatal("want error for nil entry")
		}
	})

	t.Run("negative context rejected", func(t *testing.T) {
		_, err := validateModelMeta(map[string]*ModelMetaConfig{"m": {ContextTokens: -1}})
		if err == nil {
			t.Fatal("want error for negative contextTokens")
		}
	})

	t.Run("negative cost rejected", func(t *testing.T) {
		_, err := validateModelMeta(map[string]*ModelMetaConfig{"m": {InputCostPerMTokMicroUSD: -1}})
		if err == nil {
			t.Fatal("want error for negative input cost")
		}
		_, err = validateModelMeta(map[string]*ModelMetaConfig{"m": {OutputCostPerMTokMicroUSD: -1}})
		if err == nil {
			t.Fatal("want error for negative output cost")
		}
	})

	t.Run("free with non-zero cost rejected as contradictory", func(t *testing.T) {
		_, err := validateModelMeta(map[string]*ModelMetaConfig{"m": {Free: true, InputCostPerMTokMicroUSD: 1}})
		if err == nil {
			t.Fatal("want error for free:true with a non-zero cost field")
		}
	})

	t.Run("multiple invalid entries report the first in sorted key order", func(t *testing.T) {
		raw := map[string]*ModelMetaConfig{
			"zzz": {ContextTokens: -1},
			"aaa": {ContextTokens: -1},
		}
		_, err := validateModelMeta(raw)
		if err == nil {
			t.Fatal("want error")
		}
		if got := err.Error(); !strings.Contains(got, `"aaa"`) {
			t.Errorf("error = %q, want it to name the sorted-first key %q", got, "aaa")
		}
	})
}
