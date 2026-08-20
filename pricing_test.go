package traefikllmgateway

import (
	"strconv"
	"sync"
	"testing"
)

// TestCostMicrosBuiltin exercises the integer micro-USD math against
// builtinPricing entries directly, so a regression to float-based pricing
// (drift) or a wrong tokensPerMillion divisor shows up as an exact
// mismatch, not a tolerance check.
func TestCostMicrosBuiltin(t *testing.T) {
	cases := []struct {
		model string
		u     usage
		want  int64
	}{
		// 1M prompt tokens @ $1.25/M + 1M completion tokens @ $10/M = $11.25.
		{"gpt-5", usage{prompt: 1_000_000, completion: 1_000_000}, 11_250_000},
		// 500 prompt @ $2.5/M + 200 completion @ $10/M.
		{"gpt-4o", usage{prompt: 500, completion: 200}, 1250 + 2000},
		// Zero usage costs zero regardless of price.
		{"claude-opus-4-5", usage{}, 0},
		// Prompt-only usage.
		{"o3", usage{prompt: 100_000}, 200_000}, // 100_000 * 2_000_000 / 1_000_000
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			if got := costMicros(c.model, c.u, nil); got != c.want {
				t.Errorf("costMicros(%q, %+v, nil) = %d, want %d", c.model, c.u, got, c.want)
			}
		})
	}
}

// TestCostMicrosOverride asserts an exact-match override takes priority
// over a builtin entry for the same model id, and that an override for a
// model absent from builtinPricing still prices correctly.
func TestCostMicrosOverride(t *testing.T) {
	overrides := map[string]*ModelPricing{
		"gpt-5":          {InputPerM: 1, OutputPerM: 1}, // overrides the builtin gpt-5 price
		"in-house-model": {InputPerM: 1, OutputPerM: 2},
	}
	u := usage{prompt: 1_000_000, completion: 1_000_000}

	if got, want := costMicros("gpt-5", u, overrides), int64(2_000_000); got != want {
		t.Errorf("override for known model: costMicros = %d, want %d", got, want)
	}
	if got, want := costMicros("in-house-model", u, overrides), int64(3_000_000); got != want {
		t.Errorf("override for unknown model: costMicros = %d, want %d", got, want)
	}
}

// TestCostMicrosOverrideNilEntryFallsBack asserts a nil override map entry
// does not shadow the builtin price with a silent zero — it falls through
// to builtinPricing, same as if the key were absent.
func TestCostMicrosOverrideNilEntryFallsBack(t *testing.T) {
	overrides := map[string]*ModelPricing{"gpt-5": nil}
	u := usage{prompt: 1_000_000, completion: 1_000_000}
	if got, want := costMicros("gpt-5", u, overrides), int64(11_250_000); got != want {
		t.Errorf("nil override entry: costMicros = %d, want %d (builtin price)", got, want)
	}
}

// resetPricingWarnState snapshots and clears the package-level unknown-
// model dedup state (warnedModels, warnCapNotified) and pricingWarnFn, and
// registers a t.Cleanup to restore them — so tests exercising the
// warn-once/warn-cap behavior are independent of each other and of test
// order. All access goes through setPricingWarnFn / warnedModelsMu so
// these tests stay race-clean.
func resetPricingWarnState(t *testing.T) *[]string {
	t.Helper()
	warnedModelsMu.Lock()
	prevWarn, prevWarned, prevCapNotified := pricingWarnFn, warnedModels, warnCapNotified
	warnedModels = map[string]bool{}
	warnCapNotified = false
	warnedModelsMu.Unlock()

	t.Cleanup(func() {
		warnedModelsMu.Lock()
		warnedModels, warnCapNotified = prevWarned, prevCapNotified
		warnedModelsMu.Unlock()
		setPricingWarnFn(prevWarn)
	})

	var warned []string
	var mu sync.Mutex
	setPricingWarnFn(func(model string) {
		mu.Lock()
		defer mu.Unlock()
		warned = append(warned, model)
	})
	return &warned
}

// TestCostMicrosUnknownModelWarnsOnce asserts an unpriced model costs 0
// and fires pricingWarnFn exactly once even across repeated calls, then
// resumes warning for a second distinct unknown model.
func TestCostMicrosUnknownModelWarnsOnce(t *testing.T) {
	warned := resetPricingWarnState(t)

	for i := 0; i < 3; i++ {
		if got := costMicros("totally-unpriced-model", usage{prompt: 100, completion: 100}, nil); got != 0 {
			t.Errorf("iteration %d: costMicros = %d, want 0", i, got)
		}
	}
	if got := costMicros("another-unpriced-model", usage{prompt: 1}, nil); got != 0 {
		t.Errorf("costMicros = %d, want 0", got)
	}

	want := []string{"totally-unpriced-model", "another-unpriced-model"}
	if len(*warned) != len(want) {
		t.Fatalf("warned = %v, want %v", *warned, want)
	}
	for i, m := range want {
		if (*warned)[i] != m {
			t.Errorf("warned[%d] = %q, want %q", i, (*warned)[i], m)
		}
	}
}

// TestCostMicrosUnknownModelWarnCap asserts warnedModels never grows past
// unknownModelWarnCap, however many distinct unknown model ids costMicros
// sees — the model id comes from client-controlled request bodies (a
// group with an empty Models allowlist accepts any string), so an
// unbounded dedup set would be a memory-growth vector. Once the cap is
// reached, one summary warning fires and no more do.
func TestCostMicrosUnknownModelWarnCap(t *testing.T) {
	warned := resetPricingWarnState(t)

	const distinctModels = 200
	for i := 0; i < distinctModels; i++ {
		model := "unpriced-model-" + strconv.Itoa(i)
		if got := costMicros(model, usage{prompt: 1}, nil); got != 0 {
			t.Fatalf("costMicros(%q, ...) = %d, want 0", model, got)
		}
	}

	warnedModelsMu.Lock()
	setSize := len(warnedModels)
	warnedModelsMu.Unlock()
	if setSize > unknownModelWarnCap {
		t.Errorf("warnedModels size = %d, want <= %d", setSize, unknownModelWarnCap)
	}

	if got, max := len(*warned), unknownModelWarnCap+1; got > max {
		t.Errorf("warn count = %d, want <= %d (cap distinct warnings + 1 summary)", got, max)
	}

	last := (*warned)[len(*warned)-1]
	if last != warnCapMessage {
		t.Errorf("last warning = %q, want the cap-reached summary %q", last, warnCapMessage)
	}
}

// TestCostMicrosRoundsPriceConversion pins costMicros' per-1M-token price
// conversion to round-to-nearest rather than truncate: at $8.20/M, a
// truncating int64(8.2*1e6) undershoots to 8_199_999 (float64 can
// represent 8.2 as very slightly under its true value); rounding must
// yield the intended 8_200_000.
func TestCostMicrosRoundsPriceConversion(t *testing.T) {
	overrides := map[string]*ModelPricing{"round-test": {InputPerM: 8.2, OutputPerM: 8.2}}
	u := usage{prompt: 1_000_000, completion: 1_000_000}
	if got, want := costMicros("round-test", u, overrides), int64(16_400_000); got != want {
		t.Errorf("costMicros with an 8.2 price = %d, want %d", got, want)
	}
}

// TestBuiltinPricingSeedComplete asserts every model the task brief seeds
// is present in builtinPricing with a positive price on both sides — a
// missing or zeroed entry would silently price real traffic at $0.
func TestBuiltinPricingSeedComplete(t *testing.T) {
	want := []string{
		"gpt-5", "gpt-5-mini", "gpt-4o", "o3",
		"claude-opus-4-5", "claude-sonnet-4-5", "claude-haiku-4-5",
		"gemini-2.5-pro", "gemini-2.5-flash", "grok-4",
	}
	if len(builtinPricing) != len(want) {
		t.Errorf("builtinPricing has %d entries, want %d", len(builtinPricing), len(want))
	}
	for _, m := range want {
		p, ok := builtinPricing[m]
		if !ok {
			t.Errorf("builtinPricing missing %q", m)
			continue
		}
		if p.InputPerM <= 0 || p.OutputPerM <= 0 {
			t.Errorf("builtinPricing[%q] = %+v, want both prices > 0", m, p)
		}
	}
}
