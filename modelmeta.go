package traefikllmgateway

import (
	"fmt"
	"sort"
	"strings"
)

// microUSDPerUSD scales a micro-USD integer (ModelMetaConfig/
// builtinModelMeta's storage unit — integer math avoids the float-drift
// pricing.go's costMicros already documents for the cost-ACCOUNTING path)
// to plain USD for exposure in a JSON API. Kept as its own named constant
// rather than reusing pricing.go's tokensPerMillion: the two are
// numerically identical (1_000_000) but represent different physical
// quantities — one a token count, this one a currency scale — and must
// not be conflated just because they share a value.
const microUSDPerUSD = 1_000_000

// freeTierSuffix marks a discovered model id as free by naming
// convention alone (review addendum, feature v0.23): a real provider
// (OpenRouter's own live API is the known case) reports free-tier model
// ids with this exact suffix, independent of whatever LiteLLM's static
// pricing file does or does not track for that provider. resolveModelMeta
// checks this AFTER an operator's own config override and BEFORE the
// built-in table, so a ":free"-suffixed id never falls through to a
// builtin entry's paid price for that same key, and an operator can
// still override it (e.g. to attach a context window) if they choose.
const freeTierSuffix = ":free"

// resolvedModelMeta is resolveModelMeta's result: a model's context
// window and per-token cost, each resolved independently through its own
// layering (feature v0.23). ContextKnown/CostKnown are authoritative — a
// caller must check them, never infer "unknown" from a zero numeric
// field, since a free model's cost fields are legitimately zero AND
// known (ModelMetaConfig.Free).
type resolvedModelMeta struct {
	ContextTokens             int
	InputCostPerMTokMicroUSD  int64
	OutputCostPerMTokMicroUSD int64
	ContextKnown              bool
	CostKnown                 bool
}

// resolveModelMeta layers per-model metadata resolution (feature v0.23):
//
//   - Context: an operator's modelMeta config override
//     (lookupModelMetaConfig: an exact "provider/model" key, then a bare
//     "model" key) wins when it sets ContextTokens > 0; otherwise
//     discoveredContext (only when discoveredKnown — a provider's own
//     discovery-captured context, registry.go's providerState.
//     discoveredContext, populated only for a provider whose adapter
//     implements modelMetadataFetcher and returned a value for this
//     model); otherwise the built-in table (builtinModelMetaTable,
//     pricing_data_gen.go), keyed by bare model id only — an operator's
//     own provider name never matches LiteLLM's canonical provider
//     names, so an exact "provider/model" form makes no sense for that
//     table the way it does for cfg; otherwise unknown.
//   - Cost: the config override wins when it sets Free (an explicit,
//     known zero) or either cost field > 0; otherwise a ":free"-suffixed
//     model id (freeTierSuffix — review addendum) wins next, ahead of
//     the built-in table, so a provider's own free-tier naming
//     convention is never shadowed by a paid built-in price under the
//     identical key; otherwise the built-in table. Discovery never
//     reports cost, so there is no discovery layer for it.
//
// Absent at every applicable layer is a genuinely unknown value and is
// never invented — ContextKnown/CostKnown both stay false in that case.
// The freeTierSuffix rule affects ONLY cost: a ":free" id's context
// still runs through the normal context layering above (config/
// discovery/builtin/absent), never inherited from some other, unrelated
// entry.
func resolveModelMeta(provider, model string, cfg map[string]*ModelMetaConfig, discoveredContext int, discoveredKnown bool) resolvedModelMeta {
	var out resolvedModelMeta
	override := lookupModelMetaConfig(provider, model, cfg)
	builtin, builtinOK := builtinModelMetaTable[model]

	switch {
	case override != nil && override.ContextTokens > 0:
		out.ContextTokens, out.ContextKnown = override.ContextTokens, true
	case discoveredKnown && discoveredContext > 0:
		out.ContextTokens, out.ContextKnown = discoveredContext, true
	case builtinOK && builtin.ContextTokens > 0:
		out.ContextTokens, out.ContextKnown = builtin.ContextTokens, true
	}

	switch {
	case override != nil && override.Free:
		out.InputCostPerMTokMicroUSD, out.OutputCostPerMTokMicroUSD, out.CostKnown = 0, 0, true
	case override != nil && (override.InputCostPerMTokMicroUSD > 0 || override.OutputCostPerMTokMicroUSD > 0):
		out.InputCostPerMTokMicroUSD = override.InputCostPerMTokMicroUSD
		out.OutputCostPerMTokMicroUSD = override.OutputCostPerMTokMicroUSD
		out.CostKnown = true
	case strings.HasSuffix(model, freeTierSuffix):
		out.InputCostPerMTokMicroUSD, out.OutputCostPerMTokMicroUSD, out.CostKnown = 0, 0, true
	case builtinOK && (builtin.InputCostPerMTokMicroUSD > 0 || builtin.OutputCostPerMTokMicroUSD > 0):
		out.InputCostPerMTokMicroUSD = builtin.InputCostPerMTokMicroUSD
		out.OutputCostPerMTokMicroUSD = builtin.OutputCostPerMTokMicroUSD
		out.CostKnown = true
	}
	return out
}

// lookupModelMetaConfig resolves cfg's config-override layer for
// (provider, model): an exact "provider/model" key wins over a bare
// "model" key, matching this feature's own spec precedence (a key names
// either an exact provider/model pair or a bare id that applies wherever
// that id resolves). Returns nil (no override at this layer) when cfg is
// nil or neither key is present, or when the matched entry is itself a
// nil pointer (a malformed-but-decoded config value, treated as absent
// rather than dereferenced).
func lookupModelMetaConfig(provider, model string, cfg map[string]*ModelMetaConfig) *ModelMetaConfig {
	if cfg == nil {
		return nil
	}
	if v, ok := cfg[provider+"/"+model]; ok && v != nil {
		return v
	}
	if v, ok := cfg[model]; ok && v != nil {
		return v
	}
	return nil
}

// resolveAliasModelMeta applies modelMeta's alias-inheritance rule
// (feature v0.23): an alias inherits its target's fully resolved
// metadata (resolveModelMeta against the alias's resolved provider/
// bare-model target) UNLESS cfg carries a modelMeta entry keyed by the
// alias id itself — that entry's own fields win over the inherited ones,
// independently per field (an alias-level entry that sets only context
// still inherits the target's cost, and vice versa), the same
// override-wins-per-field shape resolveModelMeta itself applies between
// its own layers.
func resolveAliasModelMeta(alias, targetProvider, targetModel string, cfg map[string]*ModelMetaConfig, discoveredContext int, discoveredKnown bool) resolvedModelMeta {
	out := resolveModelMeta(targetProvider, targetModel, cfg, discoveredContext, discoveredKnown)
	if cfg == nil {
		return out
	}
	override, ok := cfg[alias]
	if !ok || override == nil {
		return out
	}
	if override.ContextTokens > 0 {
		out.ContextTokens, out.ContextKnown = override.ContextTokens, true
	}
	switch {
	case override.Free:
		out.InputCostPerMTokMicroUSD, out.OutputCostPerMTokMicroUSD, out.CostKnown = 0, 0, true
	case override.InputCostPerMTokMicroUSD > 0 || override.OutputCostPerMTokMicroUSD > 0:
		out.InputCostPerMTokMicroUSD = override.InputCostPerMTokMicroUSD
		out.OutputCostPerMTokMicroUSD = override.OutputCostPerMTokMicroUSD
		out.CostKnown = true
	}
	return out
}

// microUSDPerMTokToUSD converts a micro-USD-per-million-tokens integer
// (ModelMetaConfig/builtinModelMeta's storage unit) to plain
// USD-per-million-tokens for exposure in a JSON API (GET /v1/models'
// "pricing" extension field: "floats in USD per MTok").
func microUSDPerMTokToUSD(micro int64) float64 {
	return float64(micro) / microUSDPerUSD
}

// validateModelMeta validates cfg.ModelMeta (feature v0.23) and returns
// it unchanged (nil when raw is empty — the zero-behavior-change
// default, matching validateModelAliases' own convention, registry.go).
// raw is iterated in sorted key order, not Go's randomized map order, so
// a config with more than one invalid entry always reports the same one
// first, deterministically, across repeated runs.
func validateModelMeta(raw map[string]*ModelMetaConfig) (map[string]*ModelMetaConfig, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := raw[k]
		if v == nil {
			return nil, fmt.Errorf("llmgateway: modelMeta: entry %q must not be nil", k)
		}
		if v.ContextTokens < 0 {
			return nil, fmt.Errorf("llmgateway: modelMeta: entry %q: contextTokens must not be negative", k)
		}
		if v.InputCostPerMTokMicroUSD < 0 || v.OutputCostPerMTokMicroUSD < 0 {
			return nil, fmt.Errorf("llmgateway: modelMeta: entry %q: cost fields must not be negative", k)
		}
		if v.Free && (v.InputCostPerMTokMicroUSD != 0 || v.OutputCostPerMTokMicroUSD != 0) {
			return nil, fmt.Errorf("llmgateway: modelMeta: entry %q: free=true is contradictory with a non-zero cost field", k)
		}
	}
	return raw, nil
}
