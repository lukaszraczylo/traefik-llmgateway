package traefikllmgateway

import (
	"fmt"
	"math"
	"sort"
	"sync"
)

// tokensPerMillion is the denominator for the providers' per-1M-token
// pricing unit.
const tokensPerMillion = 1_000_000

// builtinPricing is the built-in per-model price table, in USD per 1M
// tokens (InputPerM, OutputPerM). Source: each provider's own published
// list prices, as recorded 2026-08. Approximate: providers change prices
// without notice, and this table is not refreshed automatically — set the
// middleware config's "pricing" overrides for billing-grade accuracy.
var builtinPricing = map[string]ModelPricing{
	"gpt-5":             {InputPerM: 1.25, OutputPerM: 10},
	"gpt-5-mini":        {InputPerM: 0.25, OutputPerM: 2},
	"gpt-4o":            {InputPerM: 2.5, OutputPerM: 10},
	"o3":                {InputPerM: 2, OutputPerM: 8},
	"claude-opus-4-5":   {InputPerM: 5, OutputPerM: 25},
	"claude-sonnet-4-5": {InputPerM: 3, OutputPerM: 15},
	"claude-haiku-4-5":  {InputPerM: 1, OutputPerM: 5},
	"gemini-2.5-pro":    {InputPerM: 1.25, OutputPerM: 10},
	"gemini-2.5-flash":  {InputPerM: 0.3, OutputPerM: 2.5},
	"grok-4":            {InputPerM: 3, OutputPerM: 15},
}

// unknownModelWarnCap bounds warnedModels. The model id in
// costMicrosKnownFor's calls comes from client-supplied request bodies (a
// group with an empty Models allowlist accepts any model string), so
// without a cap an attacker or a buggy client cycling through model names
// could grow warnedModels without bound. Once the cap is reached,
// warnUnknownModelFor stops recording new names and fires its warn func
// once more with a summary message instead of a per-model one.
const unknownModelWarnCap = 128

// warnCapMessage is the single summary warning fired once the dedup set
// in warnedModels reaches unknownModelWarnCap.
const warnCapMessage = "pricing: unknown-model warning cap reached; further unknown models unwarned"

// warnedModels tracks which unknown model ids have already triggered
// a warn func, so a gateway processing many requests for the same
// unpriced model logs the warning once per process lifetime rather than
// once per request. warnCapNotified tracks whether the one-time summary
// warning has already fired once the cap is reached. Package-level and
// mutex-guarded by design: costMicrosKnownFor is called from the request path
// with no natural place to thread a longer-lived cache through, and the
// dedup set must survive across requests and across limiter instances,
// not reset per call.
var (
	warnedModelsMu  sync.Mutex
	warnedModels    = map[string]bool{}
	warnCapNotified bool
)

// costMicrosKnownFor computes the micro-USD cost of u's token usage for
// model, and whether a price was actually FOUND. An unknown model returns
// (0, false) — a zero that means "cannot be priced", which is NOT the
// same value a genuinely free model produces, even though both are 0.
// Looks up model's price via lookupPricing (overrides, then
// builtinPricing, then builtinModelMetaTable); an unknown model triggers
// one warn call per model id, up to unknownModelWarnCap distinct names.
//
// Security audit run-1, finding F-1 (high): an earlier single-int64-
// returning costMicros conflated "unpriced" with "genuinely zero cost",
// and limiter.account's own `if costMicros != 0` guard then skipped every
// cost-counter write for an unpriced model. checkAndCount reads exactly
// those never-written counters, so `used < limit` stayed permanently true
// and a configured costPerDayUSD/costPerMonthUSD could never fire. Any
// caller gating a SPEND control must handle ok == false.
//
// warn is the caller's own per-instance sink (a *Gateway's g.pricingWarn,
// logger.go), never a package global: Traefik constructs one Gateway per
// middleware instance, and a shared global made the most recently
// constructed instance log every other instance's warnings (finding F11 /
// review-auth F8, 2026-09 review). The unknown-model dedup (warnedModels)
// stays process-global, so a model already warned about under one
// instance is not warned about again under another.
func costMicrosKnownFor(model string, u usage, overrides map[string]*ModelPricing, warn func(string)) (int64, bool) {
	price, ok := lookupPricing(model, overrides)
	if !ok {
		warnUnknownModelFor(model, warn)
		return 0, false
	}
	return computeCostMicros(price, u), true
}

// computeCostMicros is costMicrosKnownFor's arithmetic: price's two per-1M-token rates, each scaled to
// micro-USD and rounded to the nearest micro-USD (a bare truncating cast
// can undershoot a price like 8.2 by one micro-USD due to float64
// representation), multiplied by their own token count, SUMMED, and only
// THEN divided back down and rounded to the nearest micro-USD (findings
// F9/F10, 2026-09 review).
//
// F9: the previous version divided input and output separately
// (tokens*priceMicrosPerM/tokensPerMillion, truncating each), which
// silently billed 0 for any single request under ~50 tokens at a
// realistic per-token price — up to ~2 micro-USD of truncation error per
// request (one per direction), enough that account (limits.go) skipped
// the cost-counter write entirely (`if costMicros != 0`) for genuinely
// billable-but-cheap traffic. Rounding the COMBINED total once instead
// halves the worst-case error and stops a nonzero combined cost from
// truncating away to a written 0.
//
// F10: the previous unchecked int64 multiply overflowed for a token count
// above roughly 1.2e11 at a $75/M price — entirely upstream-controlled,
// since the token counts come from the provider's own reported usage
// (limits.go's account, security audit run-1) — silently wrapping to a
// negative or garbage value; a negative result was then clamped to 0 by
// account, permanently masking the true cost. saturatingMulInt64/
// saturatingAddInt64 (below) clamp each multiply and the final sum to
// [math.MinInt64, math.MaxInt64] instead of wrapping, so an absurd
// upstream-reported token count bills the maximum representable cost
// rather than a wrapped-around near-zero or negative one.
func computeCostMicros(price ModelPricing, u usage) int64 {
	inputMicrosPerM := int64(math.Round(price.InputPerM * tokensPerMillion))
	outputMicrosPerM := int64(math.Round(price.OutputPerM * tokensPerMillion))

	inputCost := saturatingMulInt64(u.prompt, inputMicrosPerM)
	outputCost := saturatingMulInt64(u.completion, outputMicrosPerM)
	total := saturatingAddInt64(inputCost, outputCost)

	return roundedDivInt64(total, tokensPerMillion)
}

// roundedDivInt64 divides total by denom, rounded to the nearest integer
// (half away from zero) rather than truncated toward zero — see
// computeCostMicros' own F9 discussion. denom is always tokensPerMillion
// (a positive compile-time constant) at this function's one call site.
//
// Deliberately computed via quotient/remainder rather than the more
// obvious "(total + denom/2) / denom": total can legitimately arrive at
// math.MaxInt64 (computeCostMicros' own saturatingAddInt64 clamps there
// on overflow, finding F10), and adding denom/2 to it before dividing
// would itself silently overflow right back — exactly the wrap-around
// bug this whole function exists to avoid one step earlier. Working from
// the remainder instead never adds to total at all.
func roundedDivInt64(total, denom int64) int64 {
	q := total / denom
	r := total % denom
	if r < 0 {
		r = -r
	}
	if r*2 >= denom {
		if total >= 0 {
			q++
		} else {
			q--
		}
	}
	return q
}

// saturatingMulInt64 returns a*b, clamped to math.MinInt64/math.MaxInt64
// instead of wrapping on overflow (finding F10, 2026-09 review) — a
// simple post-multiply check (divide back and compare), not math/bits:
// this plugin runs interpreted under Yaegi (COMMON.md's own "stdlib only,
// avoid reflection-heavy code" guidance), and a division-based overflow
// check needs nothing beyond the four basic int64 operators.
func saturatingMulInt64(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	p := a * b
	if p/b != a {
		if (a > 0) == (b > 0) {
			return math.MaxInt64
		}
		return math.MinInt64
	}
	return p
}

// saturatingAddInt64 returns a+b, clamped to math.MinInt64/math.MaxInt64
// instead of wrapping on overflow (finding F10, 2026-09 review) — the
// standard "the sum moved the wrong way" overflow test for two's-
// complement addition.
func saturatingAddInt64(a, b int64) int64 {
	sum := a + b
	if b > 0 && sum < a {
		return math.MaxInt64
	}
	if b < 0 && sum > a {
		return math.MinInt64
	}
	return sum
}

// lookupPricing resolves model's price: overrides first (a nil entry does
// not count as a match, so a caller can't accidentally price a model at
// zero by leaving a map entry nil), then builtinPricing, then
// builtinModelMetaTable (pricing_data_gen.go's 236-entry LiteLLM-synced
// table — review finding F2, 2026-09 review): builtinPricing alone only
// ever carried 10 flagship models, so an operator serving anything else
// the generated table already knows a cost for (gpt-4.1, o4-mini,
// deepseek-chat, and 200+ others) billed 0 for it even though GET
// /v1/models — which reads the identical table via resolveModelMeta,
// modelmeta.go — advertised a real price for the exact same model,
// letting AllowUnpricedWithCostBudget's cost guard refuse the request as
// "unpriced" in the same breath the catalog called it priced. Converted
// from the table's integer micro-USD-per-million-tokens unit to
// ModelPricing's own float64-USD-per-million-tokens unit via
// microUSDPerMTokToUSD (modelmeta.go) — the identical conversion GET
// /v1/models' own pricing view already applies to this same table. A
// zero-valued cost field in the table means "no data" (builtinModelMeta's
// own doc comment), not "free" — at least one side must be positive
// before an entry is trusted, the same rule resolveModelMeta's own cost
// switch already applies to it. Precedence is unchanged by this addition:
// an operator override always wins, builtinPricing (curated, more likely
// to be current) wins over the generated table next, and the generated
// table is only ever the last resort.
func lookupPricing(model string, overrides map[string]*ModelPricing) (ModelPricing, bool) {
	if p, ok := overrides[model]; ok && p != nil {
		return *p, true
	}
	if p, ok := builtinPricing[model]; ok {
		return p, true
	}
	if m, ok := builtinModelMetaTable[model]; ok && (m.InputCostPerMTokMicroUSD > 0 || m.OutputCostPerMTokMicroUSD > 0) {
		return ModelPricing{
			InputPerM:  microUSDPerMTokToUSD(m.InputCostPerMTokMicroUSD),
			OutputPerM: microUSDPerMTokToUSD(m.OutputCostPerMTokMicroUSD),
		}, true
	}
	return ModelPricing{}, false
}

// validatePricing validates every entry in raw (Config.Pricing overrides)
// and returns it unchanged — a constructor error rather than a silently
// unenforceable cost budget (finding F4, 2026-09 review): costMicrosKnownFor
// had no validation of its own, so a negative, NaN, or Inf price still
// passed priceKnown/modelPriceKnown's own "is this model priced" check
// (modelmeta.go) yet produced a meaningless or negative cost — permanently
// defeating any costPerDayUSD/costPerMonthUSD budget configured for that
// model (limits.go's account already clamps a negative cost to 0 and logs
// it on every request, but by then the budget has already never fired,
// silently, since construction). Mirrors LimitsConfig.validate's own
// "non-nil, finite, >= 0" rule (limits.go) applied to InputPerM/OutputPerM
// instead of a count/cost limit. raw is walked in sorted key order,
// matching validateModelMeta's own convention (modelmeta.go), so a config
// with more than one invalid entry always reports the same one first,
// deterministically across repeated runs.
func validatePricing(raw map[string]*ModelPricing) (map[string]*ModelPricing, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		p := raw[k]
		if p == nil {
			return nil, fmt.Errorf("llmgateway: pricing: entry %q must not be nil", k)
		}
		fields := []struct {
			name string
			v    float64
		}{
			{"inputPerM", p.InputPerM},
			{"outputPerM", p.OutputPerM},
		}
		for _, f := range fields {
			if math.IsNaN(f.v) || math.IsInf(f.v, 0) {
				return nil, fmt.Errorf("llmgateway: pricing: entry %q: %s must be a finite number, got %v", k, f.name, f.v)
			}
			if f.v < 0 {
				return nil, fmt.Errorf("llmgateway: pricing: entry %q: %s must not be negative, got %v", k, f.name, f.v)
			}
		}
	}
	return raw, nil
}

// warnUnknownModelFor calls warn for model, once per process lifetime, via
// the process-global dedup set (warnedModels) — deliberately not scoped
// per-caller, so a model already warned about under one Gateway
// instance's warn func is not warned about again under a different
// instance's (finding F11 / review-auth F8's own "keep the process-global
// dedup cap if needed" allowance; costMicrosKnownFor passes its caller's
// own warn func). Once
// warnedModels reaches unknownModelWarnCap, it stops recording new model
// ids and instead fires a single one-time summary warning via
// warnCapMessage — after that, further unknown models are silently
// unwarned rather than growing the set forever.
func warnUnknownModelFor(model string, warn func(string)) {
	warnedModelsMu.Lock()
	defer warnedModelsMu.Unlock()
	if warnedModels[model] {
		return
	}
	if len(warnedModels) >= unknownModelWarnCap {
		if !warnCapNotified {
			warnCapNotified = true
			warn(warnCapMessage)
		}
		return
	}
	warnedModels[model] = true
	warn(model)
}
