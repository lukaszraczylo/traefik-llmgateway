import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/lib/api', async () => {
  const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
  return { ...actual, adminFetch: vi.fn() }
})

import { AdminApiError, adminFetch } from '@/lib/api'
import { isSpendBy, isSpendMetric, MODEL_BREAKDOWN_TOP_N, useSpendStore } from '@/stores/spend'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { useFiltersStore } from '@/stores/filters'
import type { AdminOverviewResponse, AdminSeriesResponse, AdminTotalsResponse, AdminUsageModelsResponse } from '@/types/api'

const mockedAdminFetch = vi.mocked(adminFetch)

function seriesFor(scope: string[], value = 10): AdminSeriesResponse {
  return {
    metric: 'cost',
    window: 'day',
    span: 2,
    offset: 0,
    buckets: ['20260922', '20260923'],
    series: scope.map((s) => ({ scope: s, points: [value, value] })),
    unknown: [],
  }
}

function rankingWith(ids: string[]): AdminUsageModelsResponse {
  return {
    metric: 'cost',
    window: 'day',
    span: 2,
    detail: true,
    offset: 0,
    models: ids.map((id, i) => ({ id, value: 100 - i, requests: 100 - i, tokensIn: 1, tokensOut: 1, costMicroUsd: 1, free: false })),
  }
}

/** mockRoutes wires adminFetch's mock by matching the route prefix — the four concerns this store fetches (models ranking, series, totals, and again totals for drilldown) all hit different admin.go routes. */
function mockRoutes(handlers: { series?: (url: string) => AdminSeriesResponse; totals?: (url: string) => AdminTotalsResponse; models?: (url: string) => AdminUsageModelsResponse }): void {
  mockedAdminFetch.mockImplementation((async (path: string) => {
    if (path.startsWith('/admin/api/usage/models')) return handlers.models ? handlers.models(path) : rankingWith([])
    if (path.startsWith('/admin/api/usage/series')) return handlers.series ? handlers.series(path) : seriesFor(['total'], 0)
    if (path.startsWith('/admin/api/usage/totals')) return handlers.totals ? handlers.totals(path) : { kind: 'group', window: 'day', span: 2, offset: 0, metrics: [], rows: [] }
    throw new Error(`unexpected path ${path}`)
  }) as typeof adminFetch)
}

beforeEach(() => {
  setActivePinia(createPinia())
  mockedAdminFetch.mockReset()
  useAuthStore().submit('test-admin-key')
})

describe('SPEND_BY / SPEND_METRICS guards', () => {
  it('isSpendBy accepts model/provider/group and rejects anything else', () => {
    expect(isSpendBy('model')).toBe(true)
    expect(isSpendBy('provider')).toBe(true)
    expect(isSpendBy('group')).toBe(true)
    expect(isSpendBy('bogus')).toBe(false)
  })

  it('isSpendMetric accepts cost/req/tokin/tokout and rejects anything else', () => {
    expect(isSpendMetric('cost')).toBe(true)
    expect(isSpendMetric('tokout')).toBe(true)
    expect(isSpendMetric('bogus')).toBe(false)
  })
})

describe('useSpendStore.fetchModelRanking', () => {
  it('populates modelRanking on success', async () => {
    mockRoutes({ models: () => rankingWith(['a/one', 'a/two']) })
    const spend = useSpendStore()
    await spend.fetchModelRanking()
    expect(spend.modelRanking.map((m) => m.id)).toEqual(['a/one', 'a/two'])
  })

  it('is a no-op while unauthenticated', async () => {
    setActivePinia(createPinia())
    mockRoutes({})
    const spend = useSpendStore()
    await spend.fetchModelRanking()
    expect(spend.modelRanking).toEqual([])
  })
})

// verify-ui-states-3.md pre-existing fix: fetchModelRanking's failures used
// to fold into breakdownError, which a later, unrelated fetchBreakdown
// success then cleared unconditionally — silently wiping a real ranking
// failure. rankingError is its own field, never touched by fetchBreakdown.
describe('useSpendStore.fetchModelRanking: rankingError', () => {
  it('sets rankingError on failure, and a later breakdown success never clears it', async () => {
    mockRoutes({
      models: () => {
        throw new Error('ranking boom')
      },
      series: () => seriesFor(['total'], 5),
    })
    const spend = useSpendStore()
    await spend.fetchModelRanking()
    expect(spend.rankingError).toBe('ranking boom')
    expect(spend.breakdownError).toBe('')

    await spend.fetchBreakdown() // succeeds even off a still-empty ranking (by=model with 0 ids just charts nothing)
    expect(spend.breakdownError).toBe('')
    expect(spend.rankingError).toBe('ranking boom') // NOT cross-cleared
  })

  it('clears rankingError on the next successful ranking fetch', async () => {
    mockRoutes({
      models: () => {
        throw new Error('ranking boom')
      },
    })
    const spend = useSpendStore()
    await spend.fetchModelRanking()
    expect(spend.rankingError).toBe('ranking boom')

    mockRoutes({ models: () => rankingWith(['a/one']) })
    await spend.fetchModelRanking()
    expect(spend.rankingError).toBe('')
  })
})

// P3 item 21: PricingHealthTable.vue's "unpriced first" ordering needs a
// ranking sorted by TRAFFIC (req), independent of whatever metric the
// Spend breakdown chart currently sorts by — sorting by `cost` (the
// default `this.metric`) cuts the top-100 ranking off BY COST, and an
// unpriced model bills $0, so it is one of the first rows a cost-sorted
// cutoff drops.
describe('useSpendStore.fetchPricingHealthRanking', () => {
  it('always requests metric=req, regardless of the currently-selected Spend metric', async () => {
    let requestedUrl = ''
    mockRoutes({
      models: (url) => {
        requestedUrl = url
        return rankingWith(['a/one'])
      },
    })
    const spend = useSpendStore()
    spend.$patch({ metric: 'cost' }) // deliberately NOT req
    await spend.fetchPricingHealthRanking()
    expect(requestedUrl).toBe('/admin/api/usage/models?metric=req&window=day&span=7&limit=100&detail=1')
    expect(spend.pricingHealthRanking.map((m) => m.id)).toEqual(['a/one'])
  })
})

describe('useSpendStore.fetchBreakdown: by=model, scope=all', () => {
  it('fetches total + top-N model series and computes other = total - sum(top)', async () => {
    mockRoutes({
      models: () => rankingWith(['a/one', 'a/two']),
      series: (url) => {
        if (url.includes('scope=total') && !url.includes('scope=model')) return seriesFor(['total'], 100)
        return seriesFor(['model:a/one', 'model:a/two'], 20)
      },
    })
    const spend = useSpendStore()
    await spend.fetchModelRanking()
    await spend.fetchBreakdown()

    expect(spend.totalPoints).toEqual([100, 100])
    expect(spend.breakdown.map((s) => s.scope)).toEqual(['model:a/one', 'model:a/two'])
    // 100 - (20 + 20) = 60 per bucket.
    expect(spend.otherPoints).toEqual([60, 60])
  })

  it('caps the charted series at MODEL_BREAKDOWN_TOP_N even with a larger ranking', async () => {
    const ids = Array.from({ length: 25 }, (_, i) => `a/model-${i}`)
    mockRoutes({
      models: () => rankingWith(ids),
      series: (url) => {
        if (url.includes('scope=model')) {
          const requested = [...url.matchAll(/scope=model%3A([^&]+)/g)].map((m) => `model:${decodeURIComponent(m[1]!)}`)
          expect(requested.length).toBeLessThanOrEqual(MODEL_BREAKDOWN_TOP_N)
          return seriesFor(requested, 1)
        }
        return seriesFor(['total'], 1000)
      },
    })
    const spend = useSpendStore()
    await spend.fetchModelRanking()
    await spend.fetchBreakdown()
    expect(spend.breakdown).toHaveLength(MODEL_BREAKDOWN_TOP_N)
  })
})

describe('useSpendStore.fetchBreakdown: by=provider, scope=all', () => {
  it('sums the ranking models by provider prefix', async () => {
    mockRoutes({
      models: () => rankingWith(['openai/gpt-5', 'openai/gpt-5-mini', 'anthropic/claude']),
      series: (url) => {
        if (url.includes('scope=model')) {
          return seriesFor(['model:openai/gpt-5', 'model:openai/gpt-5-mini', 'model:anthropic/claude'], 10)
        }
        return seriesFor(['total'], 30)
      },
    })
    const spend = useSpendStore()
    spend.setSelection({ by: 'provider' })
    await spend.fetchModelRanking()
    await spend.fetchBreakdown()

    expect(spend.breakdown).toEqual(
      expect.arrayContaining([
        { scope: 'openai', points: [20, 20] },
        { scope: 'anthropic', points: [10, 10] },
      ]),
    )
    expect(spend.otherPoints).toBeNull()
  })
})

describe('useSpendStore.fetchBreakdown: by=group, scope=all', () => {
  it('fetches the group id list from totals, then a series per group', async () => {
    mockRoutes({
      totals: () => ({ kind: 'group', window: 'day', span: 2, offset: 0, metrics: ['cost'], rows: [{ id: 'friends', values: { cost: 40 } }, { id: 'eng', values: { cost: 60 } }] }),
      series: (url) => {
        if (url.includes('scope=group')) return seriesFor(['group:friends', 'group:eng'], 5)
        return seriesFor(['total'], 100)
      },
    })
    const spend = useSpendStore()
    spend.setSelection({ by: 'group' })
    await spend.fetchModelRanking()
    await spend.fetchBreakdown()

    expect(spend.breakdown.map((s) => s.scope).sort()).toEqual(['group:eng', 'group:friends'])
    expect(spend.otherPoints).toBeNull()
  })
})

describe('useSpendStore.fetchBreakdown: scope narrowed to a single group/user', () => {
  it('renders exactly one series for the narrowed scope, ignoring `by`', async () => {
    useFiltersStore().setFilters({ scope: 'group:friends' })
    mockRoutes({ series: () => seriesFor(['group:friends'], 42) })
    const spend = useSpendStore()
    await spend.fetchBreakdown()

    expect(spend.breakdown).toEqual([{ scope: 'group:friends', points: [42, 42] }])
    expect(spend.totalPoints).toEqual([42, 42])
    expect(spend.otherPoints).toBeNull()
  })
})

describe('useSpendStore.fetchBreakdown: scope narrowed to a provider', () => {
  it('derives a single provider-summed series from the ranking, pre-filtered to that provider', async () => {
    useFiltersStore().setFilters({ scope: 'provider:openai' })
    mockRoutes({
      models: () => rankingWith(['openai/gpt-5', 'anthropic/claude']),
      series: (url) => {
        expect(url).toContain('scope=model%3Aopenai%2Fgpt-5')
        expect(url).not.toContain('anthropic')
        return seriesFor(['model:openai/gpt-5'], 7)
      },
    })
    const spend = useSpendStore()
    await spend.fetchModelRanking()
    await spend.fetchBreakdown()

    expect(spend.breakdown).toEqual([{ scope: 'provider:openai', points: [7, 7] }])
  })
})

describe('useSpendStore.fetchBreakdown: comparison', () => {
  it('fetches an offset total series only when cmpSpec.requested', async () => {
    useFiltersStore().setFilters({ cmp: 'prev' })
    mockRoutes({
      models: () => rankingWith([]),
      series: (url) => {
        if (url.includes('offset=')) return seriesFor(['total'], 5)
        return seriesFor(['total'], 50)
      },
    })
    const spend = useSpendStore()
    await spend.fetchModelRanking()
    await spend.fetchBreakdown()

    expect(spend.comparisonPoints).toEqual([5, 5])
  })

  it('leaves comparisonPoints null when cmp is none', async () => {
    mockRoutes({ models: () => rankingWith([]) })
    const spend = useSpendStore()
    await spend.fetchModelRanking()
    await spend.fetchBreakdown()
    expect(spend.comparisonPoints).toBeNull()
  })
})

// verify-ui-states-3.md NEW-1: a genuine selection change (by, metric,
// window, span, or scope) clears `buckets` SYNCHRONOUSLY, before the new
// fetch resolves — SpendPage.vue's breakdownState (hasData: buckets.length
// > 0) must flip to 'skeleton' immediately, never keep the OLD selection's
// series on screen mislabeled under the NEW selection. A same-selection
// refetch (a poll tick, or a Retry) must leave `buckets` alone.
describe('useSpendStore.fetchBreakdown: NEW-1 buckets-clear-on-selection-change', () => {
  it('clears buckets synchronously on a genuine selection change, before the fetch resolves', async () => {
    mockRoutes({ models: () => rankingWith([]), series: () => seriesFor(['total'], 5) })
    const spend = useSpendStore()
    await spend.fetchModelRanking()
    await spend.fetchBreakdown()
    expect(spend.buckets).toEqual(['20260922', '20260923'])

    spend.$patch({ metric: 'req' }) // a genuine selection change
    const pending = spend.fetchBreakdown()
    expect(spend.buckets).toEqual([]) // cleared before the network call even starts
    await pending
    expect(spend.buckets).toEqual(['20260922', '20260923']) // repopulated once the new selection's fetch lands
  })

  it('leaves buckets on screen across a same-selection refetch (a poll tick)', async () => {
    mockRoutes({ models: () => rankingWith([]), series: () => seriesFor(['total'], 5) })
    const spend = useSpendStore()
    await spend.fetchModelRanking()
    await spend.fetchBreakdown()
    expect(spend.buckets).toEqual(['20260922', '20260923'])

    const pending = spend.fetchBreakdown() // same by/metric/window/span/scope
    expect(spend.buckets).toEqual(['20260922', '20260923']) // NOT cleared
    await pending
    expect(spend.buckets).toEqual(['20260922', '20260923'])
  })

  it('a same-selection refetch failure sets breakdownError but leaves buckets on screen (the "ready" state the inline error line in SpendPage.vue depends on)', async () => {
    mockRoutes({ models: () => rankingWith([]), series: () => seriesFor(['total'], 5) })
    const spend = useSpendStore()
    await spend.fetchModelRanking()
    await spend.fetchBreakdown()
    expect(spend.buckets.length).toBeGreaterThan(0)
    expect(spend.breakdownError).toBe('')

    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path.startsWith('/admin/api/usage/series')) throw new Error('breakdown boom')
      return rankingWith([])
    }) as typeof adminFetch)
    await spend.fetchBreakdown()

    expect(spend.breakdownError).toBe('breakdown boom')
    expect(spend.buckets.length).toBeGreaterThan(0) // still on screen: loadState reads 'ready', not 'error'
  })
})

describe('useSpendStore.fetchBurndown', () => {
  it('fetches a day-window total series spanning the current UTC day-of-month', async () => {
    mockRoutes({
      series: (url) => {
        expect(url).toContain('window=day')
        expect(url).toContain('scope=total')
        return seriesFor(['total'], 3)
      },
    })
    const spend = useSpendStore()
    await spend.fetchBurndown()
    expect(spend.burndownPoints).toEqual([3, 3])
  })

  it('reports burndownAvailable false and skips the fetch for a provider scope', async () => {
    useFiltersStore().setFilters({ scope: 'provider:openai' })
    mockRoutes({})
    const spend = useSpendStore()
    expect(spend.burndownAvailable).toBe(false)
    await spend.fetchBurndown()
    expect(mockedAdminFetch).not.toHaveBeenCalled()
    expect(spend.burndownPoints).toEqual([])
  })

  it('resolves the fleet-wide budget from the sum of configured group budgets', async () => {
    mockRoutes({ series: () => seriesFor(['total'], 1) })
    useDashboardStore().overview = {
      version: '1',
      replica: 'r',
      instance: 'i',
      warnings: [],
      providers: [],
      groups: [{ name: 'friends', memberCount: 1, limits: { costPerMonthUSD: 10 } }],
      aliases: [],
      redis: { configured: false, lastErrAt: '0001-01-01T00:00:00Z' },
      cache: { enabled: false },
      retry: { enabled: false },
      features: { userModelStats: false, latencyStats: false, cacheStats: false, lastSeen: false, failover: false },
      pricing: { override: 0, builtin: 0, litellm: 0, free: 0, unpriced: 0 },
    } satisfies AdminOverviewResponse
    const spend = useSpendStore()
    await spend.fetchBurndown()
    expect(spend.burndownBudgetMicros).toBe(10_000_000)
  })
})

// verify-ui-states-3.md NEW-2: the scope===null (provider-scope) branch
// must also reset burndownError/burndownLoading, not just buckets/points/
// budget — otherwise a stale error/loading from a PREVIOUS scope's failed
// fetch survives a detour through a provider scope (chart hidden) and
// reappears once the reader switches back to a scope with a chart.
describe('useSpendStore.fetchBurndown: NEW-2 provider-scope reset', () => {
  it('resets burndownError and burndownLoading when scope narrows to a provider', async () => {
    mockRoutes({
      series: () => {
        throw new Error('burndown boom')
      },
    })
    const spend = useSpendStore()
    await spend.fetchBurndown() // fails against the default 'all' scope
    expect(spend.burndownError).toBe('burndown boom')

    useFiltersStore().setFilters({ scope: 'provider:openai' })
    await spend.fetchBurndown() // burndownAvailable is now false -> scope===null branch
    expect(spend.burndownError).toBe('')
    expect(spend.burndownLoading).toBe(false)
    expect(spend.burndownBuckets).toEqual([])
    expect(spend.burndownPoints).toEqual([])
    expect(spend.burndownBudgetMicros).toBeNull()
  })
})

describe('useSpendStore.fetchDrilldown', () => {
  it('requests kind=group at the top level (drill empty)', async () => {
    mockRoutes({
      totals: (url) => {
        expect(url).toContain('kind=group')
        expect(url).not.toContain('group=')
        return { kind: 'group', window: 'day', span: 2, offset: 0, metrics: ['cost'], rows: [{ id: 'friends', values: { cost: 1 } }] }
      },
    })
    const spend = useSpendStore()
    await spend.fetchDrilldown()
    expect(spend.drilldownRows).toEqual([{ id: 'friends', values: { cost: 1 } }])
    expect(spend.drilldownDisabled).toBe(false)
  })

  it('requests kind=user&group=X once drilled into a group', async () => {
    mockRoutes({
      totals: (url) => {
        expect(url).toContain('kind=user')
        expect(url).toContain('group=friends')
        return { kind: 'user', window: 'day', span: 2, offset: 0, metrics: ['cost'], rows: [{ id: 'alice', values: { cost: 1 } }] }
      },
    })
    const spend = useSpendStore()
    spend.setSelection({ drill: 'group:friends' })
    await spend.fetchDrilldown()
    expect(spend.drilldownRows).toEqual([{ id: 'alice', values: { cost: 1 } }])
  })

  it('requests kind=usermodel&user=Y once drilled into a user', async () => {
    mockRoutes({
      totals: (url) => {
        expect(url).toContain('kind=usermodel')
        expect(url).toContain('user=alice')
        return { kind: 'usermodel', window: 'day', span: 2, offset: 0, metrics: ['cost'], rows: [] }
      },
    })
    const spend = useSpendStore()
    spend.setSelection({ drill: 'user:alice' })
    await spend.fetchDrilldown()
    expect(spend.drilldownDisabled).toBe(false)
  })

  // P1 item 2: usermodel only accepts day/month (no hour bucket at all).
  // The two tests above both pass with the default global range (7d ->
  // window=day) regardless of whether a clamp exists at all — this is
  // EXACTLY how the original bug (a bare, unclamped `window=hour` 400ing
  // server-side) went unnoticed: the previous spec never exercised an
  // hour-resolution global range against this request. This test asserts
  // the EXACT window/span the URL carries, not just that it contains
  // "kind=usermodel".
  it('clamps an hour-resolution global range (24h) to day/month for kind=usermodel, never window=hour', async () => {
    useFiltersStore().setFilters({ range: '24h' })
    let requestedUrl = ''
    mockRoutes({
      totals: (url) => {
        requestedUrl = url
        return { kind: 'usermodel', window: 'day', span: 2, offset: 0, metrics: ['cost'], rows: [] }
      },
    })
    const spend = useSpendStore()
    spend.$patch({ drill: 'user:alice' })
    await spend.fetchDrilldown()
    expect(requestedUrl).toBe('/admin/api/usage/totals?kind=usermodel&window=day&span=7&user=alice')
  })

  it('sets drilldownDisabled on a 404 (admin.stats.userModel off) rather than surfacing an error', async () => {
    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path.startsWith('/admin/api/usage/totals')) throw new AdminApiError('user-model statistics are not enabled', 404)
      throw new Error(`unexpected path ${path}`)
    }) as typeof adminFetch)
    // $patch, not setSelection: setSelection also fires a full refresh()
    // cascade (including fetchModelRanking), which this test's mock does
    // not answer — only the drilldown fetch itself is under test here.
    const spend = useSpendStore()
    spend.$patch({ drill: 'user:alice' })
    await spend.fetchDrilldown()
    expect(spend.drilldownDisabled).toBe(true)
    expect(spend.drilldownError).toBe('')
  })
})

describe('useSpendStore.setSelection', () => {
  it('applies by/metric/drill together and triggers exactly one refresh', async () => {
    mockRoutes({})
    const spend = useSpendStore()
    const refreshSpy = vi.spyOn(spend, 'refresh')
    spend.setSelection({ by: 'provider', metric: 'req', drill: 'group:eng' })
    expect(spend.by).toBe('provider')
    expect(spend.metric).toBe('req')
    expect(spend.drill).toBe('group:eng')
    await vi.waitFor(() => expect(refreshSpy).toHaveBeenCalledTimes(1))
  })

  it('is a no-op (no refresh) when nothing actually changes', () => {
    const spend = useSpendStore()
    const refreshSpy = vi.spyOn(spend, 'refresh')
    spend.setSelection({ by: 'model', metric: 'cost', drill: '' })
    expect(refreshSpy).not.toHaveBeenCalled()
  })
})

// verify-ui-states-2.md #3: breakdownError/pricingHealthError/drilldownError/
// burndownError replace one shared `error` field — each leg's own failure
// must never surface under, or get silently cleared by, an unrelated leg.
describe('useSpendStore: per-request error fields', () => {
  it('a pricing-health failure sets only pricingHealthError, leaving the other three legs at \'\'', async () => {
    mockRoutes({
      models: (url) => {
        if (url.includes('metric=req')) throw new Error('pricing boom')
        return rankingWith([])
      },
    })
    const spend = useSpendStore()
    await spend.refresh()

    expect(spend.pricingHealthError).toBe('pricing boom')
    expect(spend.breakdownError).toBe('')
    expect(spend.drilldownError).toBe('')
    expect(spend.burndownError).toBe('')
  })

  it('a drilldown failure sets only drilldownError, leaving the other three legs at \'\'', async () => {
    mockRoutes({
      totals: () => {
        throw new Error('drilldown boom')
      },
    })
    const spend = useSpendStore()
    await spend.refresh()

    expect(spend.drilldownError).toBe('drilldown boom')
    expect(spend.breakdownError).toBe('')
    expect(spend.pricingHealthError).toBe('')
    expect(spend.burndownError).toBe('')
  })

  it('a later breakdown success never clears an already-set pricingHealthError', async () => {
    mockRoutes({
      models: (url) => {
        if (url.includes('metric=req')) throw new Error('pricing boom')
        return rankingWith([])
      },
    })
    const spend = useSpendStore()
    await spend.refresh()
    expect(spend.pricingHealthError).toBe('pricing boom')

    mockRoutes({}) // every route now succeeds
    await spend.fetchBreakdown()
    expect(spend.breakdownError).toBe('')
    expect(spend.pricingHealthError).toBe('pricing boom')
  })

  it('retrying only the failing leg clears only that leg\'s own error', async () => {
    mockRoutes({
      models: (url) => {
        if (url.includes('metric=req')) throw new Error('pricing boom')
        return rankingWith([])
      },
    })
    const spend = useSpendStore()
    await spend.refresh()
    expect(spend.pricingHealthError).toBe('pricing boom')

    mockRoutes({}) // the network recovers
    await spend.fetchPricingHealthRanking()
    expect(spend.pricingHealthError).toBe('')
  })
})

// verify-ui-states-2.md #2: pricingHealthLoaded/drilldownLoaded are "has a
// fetch completed for the CURRENT selection", reset only on a genuine
// selection change (window/span for pricing health; window/span/drill for
// the drilldown) — never on a same-selection refetch (a 60s poll tick),
// which must not flash a skeleton back on over an already-settled result.
describe('useSpendStore: pricingHealthLoaded / drilldownLoaded', () => {
  it('pricingHealthLoaded flips true on a successful fetch, even with zero ranked models', async () => {
    mockRoutes({ models: () => rankingWith([]) })
    const spend = useSpendStore()
    expect(spend.pricingHealthLoaded).toBe(false)
    await spend.fetchPricingHealthRanking()
    expect(spend.pricingHealthLoaded).toBe(true)
  })

  it('pricingHealthLoaded stays true across a same-range refetch, and resets before a genuine range change resolves', async () => {
    mockRoutes({ models: () => rankingWith(['a/one']) })
    const spend = useSpendStore()
    await spend.fetchPricingHealthRanking()
    expect(spend.pricingHealthLoaded).toBe(true)

    // Same range again (a poll tick): must read true DURING the fetch too
    // — a caller (PricingHealthTable) checking mid-flight must not see a
    // skeleton flash for an unchanged selection.
    let sawLoadedDuringSameRangeRefetch: boolean | undefined
    mockedAdminFetch.mockImplementation((async () => {
      sawLoadedDuringSameRangeRefetch = spend.pricingHealthLoaded
      return rankingWith(['a/one'])
    }) as typeof adminFetch)
    await spend.fetchPricingHealthRanking()
    expect(sawLoadedDuringSameRangeRefetch).toBe(true)

    // A genuine range change (span 7 -> 30, still window=day) DOES reset
    // it before the new fetch resolves.
    useFiltersStore().setFilters({ range: '30d' })
    let sawLoadedDuringRangeChange: boolean | undefined
    mockedAdminFetch.mockImplementation((async () => {
      sawLoadedDuringRangeChange = spend.pricingHealthLoaded
      return rankingWith(['a/one'])
    }) as typeof adminFetch)
    await spend.fetchPricingHealthRanking()
    expect(sawLoadedDuringRangeChange).toBe(false)
  })

  it('drilldownLoaded flips true on a successful fetch and on a 404 (a settled, known-disabled state)', async () => {
    mockRoutes({ totals: () => ({ kind: 'group', window: 'day', span: 2, offset: 0, metrics: ['cost'], rows: [] }) })
    const spend = useSpendStore()
    expect(spend.drilldownLoaded).toBe(false)
    await spend.fetchDrilldown()
    expect(spend.drilldownLoaded).toBe(true)

    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path.startsWith('/admin/api/usage/totals')) throw new AdminApiError('user-model statistics are not enabled', 404)
      throw new Error(`unexpected path ${path}`)
    }) as typeof adminFetch)
    const spend2 = useSpendStore()
    spend2.$patch({ drill: 'user:alice' })
    await spend2.fetchDrilldown()
    expect(spend2.drilldownDisabled).toBe(true)
    expect(spend2.drilldownLoaded).toBe(true)
  })

  it('drilldownLoaded resets when `drill` changes, not on a same-selection refetch', async () => {
    mockRoutes({ totals: () => ({ kind: 'group', window: 'day', span: 2, offset: 0, metrics: ['cost'], rows: [] }) })
    const spend = useSpendStore()
    await spend.fetchDrilldown()
    expect(spend.drilldownLoaded).toBe(true)

    let sawLoadedDuringSameDrillRefetch: boolean | undefined
    mockedAdminFetch.mockImplementation((async () => {
      sawLoadedDuringSameDrillRefetch = spend.drilldownLoaded
      return { kind: 'group', window: 'day', span: 2, offset: 0, metrics: ['cost'], rows: [] }
    }) as typeof adminFetch)
    await spend.fetchDrilldown()
    expect(sawLoadedDuringSameDrillRefetch).toBe(true)

    spend.$patch({ drill: 'group:friends' })
    let sawLoadedDuringDrillChange: boolean | undefined
    mockedAdminFetch.mockImplementation((async () => {
      sawLoadedDuringDrillChange = spend.drilldownLoaded
      return { kind: 'user', window: 'day', span: 2, offset: 0, metrics: ['cost'], rows: [] }
    }) as typeof adminFetch)
    await spend.fetchDrilldown()
    expect(sawLoadedDuringDrillChange).toBe(false)
  })
})

// verify-ui-states-2.md #10: an overlapping poll-tick refresh() and a
// selection/filter-change refresh() can resolve in either order — only the
// LATEST refresh call may clear `loading`.
describe('useSpendStore.refresh: overlapping calls', () => {
  it('an earlier refresh finishing after a newer one has started does not clear loading', async () => {
    const spend = useSpendStore()
    vi.spyOn(spend, 'fetchModelRanking').mockResolvedValue(undefined)

    const pending: (() => void)[] = []
    const controlled = (): Promise<void> => new Promise((resolve) => pending.push(resolve))
    vi.spyOn(spend, 'fetchBreakdown').mockImplementation(controlled)
    vi.spyOn(spend, 'fetchPricingHealthRanking').mockImplementation(controlled)
    vi.spyOn(spend, 'fetchBurndown').mockImplementation(controlled)
    vi.spyOn(spend, 'fetchDrilldown').mockImplementation(controlled)

    const firstRefresh = spend.refresh()
    await vi.waitFor(() => expect(pending.length).toBe(4))
    const firstPending = pending.splice(0, pending.length)

    const secondRefresh = spend.refresh()
    await vi.waitFor(() => expect(pending.length).toBe(4))
    const secondPending = pending.splice(0, pending.length)

    // Resolve only the FIRST (now stale) refresh's own legs.
    firstPending.forEach((resolve) => resolve())
    await firstRefresh
    expect(spend.loading).toBe(true) // the second, newer refresh is still in flight

    secondPending.forEach((resolve) => resolve())
    await secondRefresh
    expect(spend.loading).toBe(false)
  })
})
