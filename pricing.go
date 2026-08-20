package traefikllmgateway

import (
	"math"
	"sync"
)

// tokensPerMillion is the denominator for the providers' per-1M-token
// pricing unit.
const tokensPerMillion = 1_000_000

// builtinPricing is the built-in per-model price table, in USD per 1M
// tokens (InputPerM, OutputPerM). Source: public list prices, 2026-08;
// verify before release.
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

// unknownModelWarnCap bounds warnedModels. The model id in costMicros'
// calls comes from client-supplied request bodies (a group with an empty
// Models allowlist accepts any model string), so without a cap an attacker
// or a buggy client cycling through model names could grow warnedModels
// without bound. Once the cap is reached, costMicros stops recording new
// names and pricingWarnFn fires once more with a summary message instead
// of a per-model one.
const unknownModelWarnCap = 128

// warnCapMessage is the single summary warning fired once the dedup set
// in warnedModels reaches unknownModelWarnCap.
const warnCapMessage = "pricing: unknown-model warning cap reached; further unknown models unwarned"

// pricingWarnFn is called once per unknown model id encountered by
// costMicros, with the model id — or, once unknownModelWarnCap is
// reached, once more with warnCapMessage. The default is a no-op so
// pricing.go has no logging dependency of its own; a later task wires
// this to the gateway's logf via setPricingWarnFn, and tests can
// substitute their own func to capture warnings the same way. Read inside
// warnUnknownModel under warnedModelsMu — always assign through
// setPricingWarnFn, never by a bare `pricingWarnFn = …`, or a concurrent
// costMicros call can race the assignment.
var pricingWarnFn = func(string) {}

// warnedModels tracks which unknown model ids have already triggered
// pricingWarnFn, so a gateway processing many requests for the same
// unpriced model logs the warning once per process lifetime rather than
// once per request. warnCapNotified tracks whether the one-time summary
// warning has already fired once the cap is reached. Package-level and
// mutex-guarded by design: costMicros is called from the request path
// with no natural place to thread a longer-lived cache through, and the
// dedup set must survive across requests and across limiter instances,
// not reset per call.
var (
	warnedModelsMu  sync.Mutex
	warnedModels    = map[string]bool{}
	warnCapNotified bool
)

// costMicros computes the micro-USD cost of u's token usage for model,
// looking up its price in overrides first (exact model id match), then
// builtinPricing. An unknown model costs 0 and triggers one pricingWarnFn
// call per model id, up to unknownModelWarnCap distinct names.
//
// The math stays in integer micro-USD throughout to avoid float drift:
// priceMicrosPerM is the per-1M-token price scaled to micro-USD and
// rounded to the nearest micro-USD (a bare truncating cast can undershoot
// a price like 8.2 by one micro-USD due to float64 representation), and
// each of input/output cost is tokens*priceMicrosPerM/1e6, summed.
func costMicros(model string, u usage, overrides map[string]*ModelPricing) int64 {
	price, ok := lookupPricing(model, overrides)
	if !ok {
		warnUnknownModel(model)
		return 0
	}

	inputMicrosPerM := int64(math.Round(price.InputPerM * tokensPerMillion))
	outputMicrosPerM := int64(math.Round(price.OutputPerM * tokensPerMillion))

	inputCost := u.prompt * inputMicrosPerM / tokensPerMillion
	outputCost := u.completion * outputMicrosPerM / tokensPerMillion
	return inputCost + outputCost
}

// lookupPricing resolves model's price: overrides first (a nil entry does
// not count as a match, so a caller can't accidentally price a model at
// zero by leaving a map entry nil), then builtinPricing.
func lookupPricing(model string, overrides map[string]*ModelPricing) (ModelPricing, bool) {
	if p, ok := overrides[model]; ok && p != nil {
		return *p, true
	}
	if p, ok := builtinPricing[model]; ok {
		return p, true
	}
	return ModelPricing{}, false
}

// setPricingWarnFn replaces pricingWarnFn, guarded by the same mutex
// warnUnknownModel reads it under. A later task calls this at New() to
// wire warnings to the gateway's logf; tests call it to capture warnings.
// Always use this instead of assigning pricingWarnFn directly.
func setPricingWarnFn(fn func(string)) {
	warnedModelsMu.Lock()
	defer warnedModelsMu.Unlock()
	pricingWarnFn = fn
}

// warnUnknownModel calls pricingWarnFn for model, once per process
// lifetime. Once warnedModels reaches unknownModelWarnCap, it stops
// recording new model ids and instead fires a single one-time summary
// warning via warnCapMessage — after that, further unknown models are
// silently unwarned rather than growing the set forever.
func warnUnknownModel(model string) {
	warnedModelsMu.Lock()
	defer warnedModelsMu.Unlock()
	if warnedModels[model] {
		return
	}
	if len(warnedModels) >= unknownModelWarnCap {
		if !warnCapNotified {
			warnCapNotified = true
			pricingWarnFn(warnCapMessage)
		}
		return
	}
	warnedModels[model] = true
	pricingWarnFn(model)
}
