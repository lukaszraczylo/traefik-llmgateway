package traefikllmgateway

import "sync"

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

// pricingWarnFn is called once per unknown model id encountered by
// costMicros, with the model id. The default is a no-op so pricing.go has
// no logging dependency of its own; a later task wires this to the
// gateway's logf, and tests can substitute their own func to capture
// warnings.
var pricingWarnFn = func(string) {}

// warnedModels tracks which unknown model ids have already triggered
// pricingWarnFn, so a gateway processing many requests for the same
// unpriced model logs the warning once per process lifetime rather than
// once per request. Package-level and mutex-guarded by design: costMicros
// is called from the request path with no natural place to thread a
// longer-lived cache through, and the dedup set must survive across
// requests and across limiter instances, not reset per call.
var (
	warnedModelsMu sync.Mutex
	warnedModels   = map[string]bool{}
)

// costMicros computes the micro-USD cost of u's token usage for model,
// looking up its price in overrides first (exact model id match), then
// builtinPricing. An unknown model costs 0 and triggers one pricingWarnFn
// call per model id.
//
// The math stays in integer micro-USD throughout to avoid float drift:
// priceMicrosPerM is the per-1M-token price scaled to micro-USD, and each
// of input/output cost is tokens*priceMicrosPerM/1e6, summed.
func costMicros(model string, u usage, overrides map[string]*ModelPricing) int64 {
	price, ok := lookupPricing(model, overrides)
	if !ok {
		warnUnknownModel(model)
		return 0
	}

	inputMicrosPerM := int64(price.InputPerM * tokensPerMillion)
	outputMicrosPerM := int64(price.OutputPerM * tokensPerMillion)

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

// warnUnknownModel calls pricingWarnFn for model, once per process
// lifetime.
func warnUnknownModel(model string) {
	warnedModelsMu.Lock()
	defer warnedModelsMu.Unlock()
	if warnedModels[model] {
		return
	}
	warnedModels[model] = true
	pricingWarnFn(model)
}
