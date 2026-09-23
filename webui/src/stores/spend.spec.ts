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
    expect(spend.error).toBe('')
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
