package traefikllmgateway

import (
	"math"
	"strconv"
	"sync"
	"testing"
)

// costMicrosOnly is costMicrosKnown for a test that only needs the price
// itself, mirroring the removed package-level costMicros helper (dead
// code removal, review finding, 2026-09 review): costMicros had no
// production caller left once every real caller moved to
// costMicrosKnown/unifiedCostMicrosKnown and their own -ok-returning
// variants, but this file's existing table tests only ever asserted on
// the price, never on the ok bool, so they keep that exact shape here
// instead of threading a discarded ok through every case.
func costMicrosOnly(model string, u usage, overrides map[string]*ModelPricing) int64 {
	got, _ := costMicrosKnown(model, u, overrides)
	return got
}

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
			if got := costMicrosOnly(c.model, c.u, nil); got != c.want {
				t.Errorf("costMicrosOnly(%q, %+v, nil) = %d, want %d", c.model, c.u, got, c.want)
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

	if got, want := costMicrosOnly("gpt-5", u, overrides), int64(2_000_000); got != want {
		t.Errorf("override for known model: costMicros = %d, want %d", got, want)
	}
	if got, want := costMicrosOnly("in-house-model", u, overrides), int64(3_000_000); got != want {
		t.Errorf("override for unknown model: costMicros = %d, want %d", got, want)
	}
}

// TestCostMicrosOverrideNilEntryFallsBack asserts a nil override map entry
// does not shadow the builtin price with a silent zero — it falls through
// to builtinPricing, same as if the key were absent.
func TestCostMicrosOverrideNilEntryFallsBack(t *testing.T) {
	overrides := map[string]*ModelPricing{"gpt-5": nil}
	u := usage{prompt: 1_000_000, completion: 1_000_000}
	if got, want := costMicrosOnly("gpt-5", u, overrides), int64(11_250_000); got != want {
		t.Errorf("nil override entry: costMicros = %d, want %d (builtin price)", got, want)
	}
}

// pricingWarnFn is the test-only warn sink costMicrosKnown passes to
// costMicrosKnownFor; production callers pass their own g.pricingWarn.
// Guarded by warnedModelsMu so tests stay race-clean.
var pricingWarnFn = func(string) {}

func setPricingWarnFn(fn func(string)) {
	warnedModelsMu.Lock()
	defer warnedModelsMu.Unlock()
	pricingWarnFn = fn
}

// costMicrosKnown is a test convenience wrapper around costMicrosKnownFor
// using the test-controlled pricingWarnFn.
func costMicrosKnown(model string, u usage, overrides map[string]*ModelPricing) (int64, bool) {
	warnedModelsMu.Lock()
	warn := pricingWarnFn
	warnedModelsMu.Unlock()
	return costMicrosKnownFor(model, u, overrides, warn)
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
		if got := costMicrosOnly("totally-unpriced-model", usage{prompt: 100, completion: 100}, nil); got != 0 {
			t.Errorf("iteration %d: costMicros = %d, want 0", i, got)
		}
	}
	if got := costMicrosOnly("another-unpriced-model", usage{prompt: 1}, nil); got != 0 {
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
		if got := costMicrosOnly(model, usage{prompt: 1}, nil); got != 0 {
			t.Fatalf("costMicrosOnly(%q, ...) = %d, want 0", model, got)
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
	if got, want := costMicrosOnly("round-test", u, overrides), int64(16_400_000); got != want {
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

// TestLookupPricing_FallsBackToGeneratedTable (finding F2, 2026-09
// review) asserts a model present ONLY in builtinModelMetaTable — not in
// the small curated builtinPricing list — still resolves a real price
// through lookupPricing/costMicrosKnown, matching the price GET
// /v1/models already advertises for the same model via resolveModelMeta.
// Before this fix, such a model billed 0 even though the catalog called
// it priced. "deepseek-chat" is one of 200+ such models
// (pricing_data_gen.go).
func TestLookupPricing_FallsBackToGeneratedTable(t *testing.T) {
	if _, ok := builtinPricing["deepseek-chat"]; ok {
		t.Fatal("test assumption broken: deepseek-chat must not be in the curated builtinPricing table")
	}
	meta, ok := builtinModelMetaTable["deepseek-chat"]
	if !ok || meta.InputCostPerMTokMicroUSD <= 0 || meta.OutputCostPerMTokMicroUSD <= 0 {
		t.Fatal("test assumption broken: deepseek-chat must be in builtinModelMetaTable with a positive cost on both sides")
	}

	price, ok := lookupPricing("deepseek-chat", nil)
	if !ok {
		t.Fatal(`lookupPricing("deepseek-chat", nil) ok = false, want a price resolved from builtinModelMetaTable`)
	}
	wantIn := microUSDPerMTokToUSD(meta.InputCostPerMTokMicroUSD)
	wantOut := microUSDPerMTokToUSD(meta.OutputCostPerMTokMicroUSD)
	if price.InputPerM != wantIn || price.OutputPerM != wantOut {
		t.Errorf(`lookupPricing("deepseek-chat", nil) = %+v, want {InputPerM:%v OutputPerM:%v}`, price, wantIn, wantOut)
	}

	got, ok := costMicrosKnown("deepseek-chat", usage{prompt: 1_000_000, completion: 1_000_000}, nil)
	if !ok {
		t.Fatal(`costMicrosKnown("deepseek-chat", ...) ok = false, want true`)
	}
	want := meta.InputCostPerMTokMicroUSD + meta.OutputCostPerMTokMicroUSD // 1M tokens each side == exactly the per-M micro-USD price
	if got != want {
		t.Errorf(`costMicrosKnown("deepseek-chat", 1M prompt + 1M completion) = %d, want %d`, got, want)
	}
}

// TestLookupPricing_OverrideWinsOverGeneratedTable (finding F2) pins that
// an operator override still takes priority over the generated-table
// fallback added by this fix, for a model that has no builtinPricing
// entry at all — the fallback must never shadow a configured override.
func TestLookupPricing_OverrideWinsOverGeneratedTable(t *testing.T) {
	overrides := map[string]*ModelPricing{"deepseek-chat": {InputPerM: 9, OutputPerM: 9}}
	price, ok := lookupPricing("deepseek-chat", overrides)
	if !ok || price.InputPerM != 9 || price.OutputPerM != 9 {
		t.Errorf(`lookupPricing("deepseek-chat", override) = %+v, %v, want the override {9 9}`, price, ok)
	}
}

// TestCostMicrosKnown_RoundsCombinedTotal (finding F9, 2026-09 review)
// asserts the input and output totals are summed BEFORE dividing back
// down to micro-USD, rather than each direction truncated separately: at
// $0.02/M, 30 prompt tokens alone truncate to 0 (30*20_000/1_000_000
// floors to 0) and so do 30 completion tokens alone, but their COMBINED
// total (1,200,000 micro-USD-tokens) divides to a nonzero 1 micro-USD —
// the exact "small cheap request silently bills 0" gap the finding
// reports, which also meant account (limits.go) skipped the cost-counter
// write entirely for that traffic.
// TestLookupPricingSource_MatchesLookupPricing pins lookupPricing as a
// thin wrapper around lookupPricingSource (admin dashboard redesign,
// WP-B): for every layer (override, builtin, generated table, unknown),
// the two must agree on price and ok, and lookupPricingSource must report
// the correct source label for each.
func TestLookupPricingSource_MatchesLookupPricing(t *testing.T) {
	overrides := map[string]*ModelPricing{"custom-model": {InputPerM: 7, OutputPerM: 7}}
	cases := []struct {
		model      string
		wantSource string
		wantOK     bool
	}{
		{"custom-model", priceSourceOverride, true},
		{"gpt-5", priceSourceBuiltin, true},         // builtinPricing entry
		{"deepseek-chat", priceSourceLitellm, true}, // generated table only
		{"totally-unknown-model-xyz", "", false},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			price, source, ok := lookupPricingSource(c.model, overrides)
			if ok != c.wantOK || source != c.wantSource {
				t.Fatalf("lookupPricingSource(%q) = (%+v, %q, %v), want source=%q ok=%v", c.model, price, source, ok, c.wantSource, c.wantOK)
			}
			wantPrice, wantOK := lookupPricing(c.model, overrides)
			if wantOK != ok || wantPrice != price {
				t.Errorf("lookupPricing(%q) = (%+v, %v) disagrees with lookupPricingSource = (%+v, %v)", c.model, wantPrice, wantOK, price, ok)
			}
		})
	}
}

// TestBillingPriceSource_Table exercises every layer billingPriceSource
// resolves through, in precedence order (DECISIONS Q3 for the ':free'
// suffix case): modelMetaFree wins outright; then an override on the
// canonical "provider/model" id; then an override on the bare id alone;
// then builtinPricing; then the generated LiteLLM table; then unpriced —
// including a ':free'-suffixed bare id that resolves to NOTHING in any
// table, which must still report priceSourceUnpriced (billing truth is
// unaffected by the naming convention; DisplayFree is admin_catalog.go's
// own, separate concern).
func TestBillingPriceSource_Table(t *testing.T) {
	overrides := map[string]*ModelPricing{
		"providerA/canonical-model": {InputPerM: 1, OutputPerM: 2},
		"bare-override-model":       {InputPerM: 3, OutputPerM: 4},
	}
	meta := map[string]*ModelMetaConfig{
		"providerA/free-model": {Free: true},
	}
	cases := []struct {
		name       string
		canonical  string
		bare       string
		wantSource string
		wantPrice  ModelPricing
	}{
		{"modelMeta free wins over everything", "providerA/free-model", "free-model", priceSourceFree, ModelPricing{}},
		{"override on canonical id", "providerA/canonical-model", "canonical-model", priceSourceOverride, ModelPricing{InputPerM: 1, OutputPerM: 2}},
		{"override on bare id when canonical has none", "providerB/bare-override-model", "bare-override-model", priceSourceOverride, ModelPricing{InputPerM: 3, OutputPerM: 4}},
		{"builtin table", "providerC/gpt-5", "gpt-5", priceSourceBuiltin, builtinPricing["gpt-5"]},
		{"generated table fallback", "providerC/deepseek-chat", "deepseek-chat", priceSourceLitellm, mustLookupPrice(t, "deepseek-chat", nil)},
		{"free-tier-suffixed id with no real price is unpriced, not free", "providerD/some-model:free", "some-model:free", priceSourceUnpriced, ModelPricing{}},
		{"totally unknown model", "providerD/unknown-xyz", "unknown-xyz", priceSourceUnpriced, ModelPricing{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source, price := billingPriceSource(c.canonical, c.bare, overrides, meta)
			if source != c.wantSource {
				t.Errorf("billingPriceSource(%q, %q) source = %q, want %q", c.canonical, c.bare, source, c.wantSource)
			}
			if price != c.wantPrice {
				t.Errorf("billingPriceSource(%q, %q) price = %+v, want %+v", c.canonical, c.bare, price, c.wantPrice)
			}
		})
	}
}

// mustLookupPrice is a small test helper for TestBillingPriceSource_Table's
// generated-table case, so its "want" value stays derived from the real
// table rather than a value that silently drifts if the table changes.
func mustLookupPrice(t *testing.T, model string, overrides map[string]*ModelPricing) ModelPricing {
	t.Helper()
	price, ok := lookupPricing(model, overrides)
	if !ok {
		t.Fatalf("test assumption broken: lookupPricing(%q) ok = false", model)
	}
	return price
}

// TestBillingPriceSource_ParityWithUnifiedCostMicros pins billingPriceSource
// against unifiedCostMicros' OWN resolution order (routes_unified.go),
// which it must mirror exactly: whenever billingPriceSource reports a
// priced source (anything but unpriced), costMicrosKnownFor against the
// SAME id unifiedCostMicros itself would have picked must resolve a price
// too, and the price itself must match what billingPriceSource reports —
// so GET /admin/api/catalog's PriceSource field can never disagree with
// what a real request actually gets charged.
func TestBillingPriceSource_ParityWithUnifiedCostMicros(t *testing.T) {
	overrides := map[string]*ModelPricing{
		"providerA/canonical-model": {InputPerM: 1, OutputPerM: 2},
		"bare-override-model":       {InputPerM: 3, OutputPerM: 4},
	}
	meta := map[string]*ModelMetaConfig{
		"providerA/free-model": {Free: true},
	}
	u := usage{prompt: 1_000_000, completion: 1_000_000}
	models := []struct{ canonical, bare string }{
		{"providerA/free-model", "free-model"},
		{"providerA/canonical-model", "canonical-model"},
		{"providerB/bare-override-model", "bare-override-model"},
		{"providerC/gpt-5", "gpt-5"},
		{"providerC/deepseek-chat", "deepseek-chat"},
		{"providerD/unknown-xyz", "unknown-xyz"},
	}
	for _, m := range models {
		t.Run(m.canonical, func(t *testing.T) {
			source, price := billingPriceSource(m.canonical, m.bare, overrides, meta)
			gotCost := unifiedCostMicros(m.canonical, m.bare, u, overrides, meta, func(string) {})
			if source == priceSourceUnpriced {
				if gotCost != 0 {
					t.Errorf("unifiedCostMicros = %d for an unpriced model, want 0", gotCost)
				}
				return
			}
			wantCost := computeCostMicros(price, u)
			if gotCost != wantCost {
				t.Errorf("unifiedCostMicros(%q) = %d, want %d (billingPriceSource's own price=%+v source=%q)", m.canonical, gotCost, wantCost, price, source)
			}
		})
	}
}

func TestCostMicrosKnown_RoundsCombinedTotal(t *testing.T) {
	overrides := map[string]*ModelPricing{"cheap-model": {InputPerM: 0.02, OutputPerM: 0.02}}
	got, ok := costMicrosKnown("cheap-model", usage{prompt: 30, completion: 30}, overrides)
	if !ok {
		t.Fatal("costMicrosKnown ok = false, want true")
	}
	if got != 1 {
		t.Errorf("costMicrosKnown(prompt=30, completion=30 @ $0.02/M) = %d, want 1 (summed-then-divided, not truncated-to-0 per direction)", got)
	}
}

// TestCostMicrosKnown_OverflowSaturates (finding F10, 2026-09 review)
// asserts an absurd upstream-reported token count saturates at
// math.MaxInt64 instead of silently wrapping to a negative or garbage
// value through an unchecked int64 multiply — upstream-controlled per
// limits.go's account (security audit run-1), since the token counts come
// from the provider's own reported usage.
func TestCostMicrosKnown_OverflowSaturates(t *testing.T) {
	overrides := map[string]*ModelPricing{"expensive-model": {InputPerM: 75, OutputPerM: 75}}
	got, ok := costMicrosKnown("expensive-model", usage{prompt: math.MaxInt64 / 2, completion: math.MaxInt64 / 2}, overrides)
	if !ok {
		t.Fatal("costMicrosKnown ok = false, want true")
	}
	// Both inputCost and outputCost individually overflow the int64
	// multiply and saturate at math.MaxInt64 (saturatingMulInt64), and
	// their sum saturates there too (saturatingAddInt64) rather than
	// wrapping to a negative/garbage value — but the value costMicrosKnown
	// actually RETURNS is that saturated total divided back down to
	// micro-USD (roundedDivInt64, computeCostMicros' own final step), not
	// raw math.MaxInt64 itself.
	want := roundedDivInt64(math.MaxInt64, tokensPerMillion)
	if got != want {
		t.Errorf("costMicrosKnown with an absurd upstream-reported token count = %d, want %d (the saturated total divided back to micro-USD)", got, want)
	}
	if got <= 0 {
		t.Errorf("costMicrosKnown with an absurd upstream-reported token count = %d, want a large positive value, not a wrapped negative/garbage one", got)
	}
}

// TestValidatePricing_RejectsBadEntries (finding F4, 2026-09 review)
// table-tests validatePricing's construction-time checks: nil, NaN, Inf,
// and negative entries must all fail with a config error; a valid entry
// must pass unchanged.
func TestValidatePricing_RejectsBadEntries(t *testing.T) {
	cases := []struct {
		pricing map[string]*ModelPricing
		name    string
		wantErr bool
	}{
		{nil, "nil map", false},
		{map[string]*ModelPricing{}, "empty map", false},
		{map[string]*ModelPricing{"m": {InputPerM: 1, OutputPerM: 2}}, "valid entry", false},
		{map[string]*ModelPricing{"m": nil}, "nil entry", true},
		{map[string]*ModelPricing{"m": {InputPerM: -1, OutputPerM: 1}}, "negative input", true},
		{map[string]*ModelPricing{"m": {InputPerM: 1, OutputPerM: -1}}, "negative output", true},
		{map[string]*ModelPricing{"m": {InputPerM: math.NaN(), OutputPerM: 1}}, "NaN input", true},
		{map[string]*ModelPricing{"m": {InputPerM: 1, OutputPerM: math.Inf(1)}}, "+Inf output", true},
		{map[string]*ModelPricing{"m": {InputPerM: math.Inf(-1), OutputPerM: 1}}, "-Inf input", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := validatePricing(c.pricing)
			if (err != nil) != c.wantErr {
				t.Errorf("validatePricing(%+v) err = %v, wantErr %v", c.pricing, err, c.wantErr)
			}
		})
	}
}
