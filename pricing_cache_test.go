package traefikllmgateway

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func f64(v float64) *float64 { return &v }

func TestComputeCostMicrosNoCachedPriceUnchanged(t *testing.T) {
	t.Parallel()
	p := ModelPricing{InputPerM: 3, OutputPerM: 15}
	require.EqualValues(t, 3_000_000, computeCostMicros(p, usage{prompt: 1_000_000, cached: 900_000}))
}

func TestComputeCostMicrosCachedRead(t *testing.T) {
	t.Parallel()
	p := ModelPricing{InputPerM: 3, OutputPerM: 15, CachedInputPerM: f64(0.3)}
	require.EqualValues(t, 570_000, computeCostMicros(p, usage{prompt: 1_000_000, cached: 900_000}))
}

func TestComputeCostMicrosCacheWrite(t *testing.T) {
	t.Parallel()
	p := ModelPricing{InputPerM: 3, OutputPerM: 15, CachedInputPerM: f64(0.3), CacheWritePerM: f64(3.75)}
	require.EqualValues(t, 915_000, computeCostMicros(p, usage{prompt: 1_000_000, cached: 800_000, cacheWrite: 100_000}))
}

func TestComputeCostMicrosCachedClamp(t *testing.T) {
	t.Parallel()
	p := ModelPricing{InputPerM: 3, OutputPerM: 15, CachedInputPerM: f64(0.3), CacheWritePerM: f64(3.75)}
	uncached := computeCostMicros(ModelPricing{InputPerM: 3, OutputPerM: 15}, usage{prompt: 1_000_000})
	cases := []struct {
		name string
		u    usage
		want int64
	}{
		// 1M prompt priced entirely at the cached rate.
		{"cached above prompt", usage{prompt: 1_000_000, cached: 2_000_000}, 300_000},
		// cached 800k at 0.3 + write clamped to 200k at 3.75.
		{"write clamped to remainder", usage{prompt: 1_000_000, cached: 800_000, cacheWrite: 800_000}, 240_000 + 750_000},
		// negatives clamp to 0: whole prompt uncached at 3.
		{"negative cache counts", usage{prompt: 1_000_000, cached: -5, cacheWrite: -5}, uncached},
	}
	for _, c := range cases {
		require.EqualValues(t, c.want, computeCostMicros(p, c.u), c.name)
	}
}

func TestPromptCacheSavedMicros(t *testing.T) {
	t.Parallel()
	p := ModelPricing{InputPerM: 3, OutputPerM: 15, CachedInputPerM: f64(0.3)}
	require.EqualValues(t, 2_430_000, promptCacheSavedMicros(p, usage{prompt: 1_000_000, cached: 900_000}))
	require.EqualValues(t, 0, promptCacheSavedMicros(ModelPricing{InputPerM: 3}, usage{prompt: 1_000_000, cached: 900_000}))
	pricey := ModelPricing{InputPerM: 1, CachedInputPerM: f64(2)}
	require.EqualValues(t, 0, promptCacheSavedMicros(pricey, usage{prompt: 10, cached: 5}))
}

func TestValidatePricingRejectsNegativeCached(t *testing.T) {
	t.Parallel()
	_, err := validatePricing(map[string]*ModelPricing{"m": {InputPerM: 1, CachedInputPerM: f64(-1)}})
	require.Error(t, err)
	_, err = validatePricing(map[string]*ModelPricing{"m": {InputPerM: 1, CacheWritePerM: f64(-1)}})
	require.Error(t, err)
	_, err = validatePricing(map[string]*ModelPricing{"m": {InputPerM: 1, CachedInputPerM: f64(0.1), CacheWritePerM: f64(1)}})
	require.NoError(t, err)
}

func TestBuiltinCachedPriceFlowsToLookup(t *testing.T) {
	// Mutates the package-global table, so no t.Parallel.
	const id = "test-only-cache-model"
	builtinModelMetaTable[id] = builtinModelMeta{
		InputCostPerMTokMicroUSD: 3_000_000, OutputCostPerMTokMicroUSD: 15_000_000,
		CachedInputCostPerMTokMicroUSD: 300_000, CacheWriteCostPerMTokMicroUSD: 3_750_000,
	}
	t.Cleanup(func() { delete(builtinModelMetaTable, id) })

	p, src, ok := lookupPricingSource(id, nil)
	require.True(t, ok)
	require.Equal(t, priceSourceLitellm, src)
	require.NotNil(t, p.CachedInputPerM)
	require.InDelta(t, 0.3, *p.CachedInputPerM, 1e-9)
	require.NotNil(t, p.CacheWritePerM)
	require.InDelta(t, 3.75, *p.CacheWritePerM, 1e-9)

	builtinModelMetaTable[id] = builtinModelMeta{InputCostPerMTokMicroUSD: 3_000_000}
	p, _, _ = lookupPricingSource(id, nil)
	require.Nil(t, p.CachedInputPerM)
	require.Nil(t, p.CacheWritePerM)
}

func TestBuiltinPricingGetsGeneratedCachePrices(t *testing.T) {
	// Mutates package-global tables, so no t.Parallel.
	const id = "claude-sonnet-4-5"
	_, curated := builtinPricing[id]
	require.True(t, curated)

	p, src, ok := lookupPricingSource(id, nil)
	require.True(t, ok)
	require.Equal(t, priceSourceBuiltin, src)
	require.NotNil(t, p.CachedInputPerM)
	require.NotNil(t, p.CacheWritePerM)
	require.Equal(t, builtinPricing[id].InputPerM, p.InputPerM)
	require.Nil(t, builtinPricing[id].CachedInputPerM, "shared map entry must not be mutated")

	// An override stays as written, with no merge.
	o := map[string]*ModelPricing{id: {InputPerM: 1, OutputPerM: 2}}
	p, src, _ = lookupPricingSource(id, o)
	require.Equal(t, priceSourceOverride, src)
	require.Nil(t, p.CachedInputPerM)
	require.Nil(t, p.CacheWritePerM)
}
