// reliability.spec.ts covers the pure helpers (hourOrDayWindow,
// errorRateSeries, seriesColor) directly, plus useReliabilityStore.fetch's
// provider-scope-list-from-dashboard-overview wiring and its zero-providers
// short-circuit — mirroring dashboard.spec.ts's own mocked-adminFetch,
// Pinia-under-vitest approach (that file's own doc comment).

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/lib/api', async () => {
  const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
  return { ...actual, adminFetch: vi.fn() }
})

import { adminFetch } from '@/lib/api'
import { errorRateSeries, hourOrDayWindow, RELIABILITY_SERIES_COLORS, seriesColor, useReliabilityStore } from '@/stores/reliability'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import type { AdminOverviewResponse, AdminPerfResponse, AdminPerfRow, AdminSeriesResponse } from '@/types/api'

const mockedAdminFetch = vi.mocked(adminFetch)

describe('hourOrDayWindow', () => {
  it('passes an hour window through unchanged, with its own span', () => {
    expect(hourOrDayWindow('hour', 48)).toEqual({ window: 'hour', span: 48 })
  })

  it('passes a day window through unchanged, with its own span', () => {
    expect(hourOrDayWindow('day', 14)).toEqual({ window: 'day', span: 14 })
  })

  it('clamps a month window (no hour/day equivalent span) to a fixed 30-day window', () => {
    expect(hourOrDayWindow('month', 6)).toEqual({ window: 'day', span: 30 })
  })
})

describe('errorRateSeries', () => {
  function series(entries: { scope: string; points: number[] }[]): AdminSeriesResponse {
    return { metric: 'x', window: 'hour', span: entries[0]?.points.length ?? 0, offset: 0, buckets: [], series: entries, unknown: [] }
  }

  it('divides fail by attempt per bucket for a matching scope', () => {
    const attempts = series([{ scope: 'provider:openai', points: [100, 50] }])
    const fails = series([{ scope: 'provider:openai', points: [1, 5] }])
    expect(errorRateSeries(attempts, fails)).toEqual([{ scope: 'provider:openai', points: [0.01, 0.1] }])
  })

  it('reports null (never 0) for a bucket with zero attempts', () => {
    const attempts = series([{ scope: 'provider:openai', points: [0, 10] }])
    const fails = series([{ scope: 'provider:openai', points: [0, 0] }])
    expect(errorRateSeries(attempts, fails)).toEqual([{ scope: 'provider:openai', points: [null, 0] }])
  })

  it('treats a scope missing entirely from the fail series as zero failures, not a thrown error', () => {
    const attempts = series([{ scope: 'provider:anthropic', points: [10] }])
    const fails = series([])
    expect(errorRateSeries(attempts, fails)).toEqual([{ scope: 'provider:anthropic', points: [0] }])
  })

  it('delegates the division itself to lib/series.ts ratioSeries verbatim (no extra clamping here)', () => {
    const attempts = series([{ scope: 'provider:x', points: [5] }])
    const fails = series([{ scope: 'provider:x', points: [9] }])
    // Deliberately > 1 (should not happen with real counters) — asserts
    // this function does not silently clamp on top of ratioSeries' own
    // behavior, so the two never quietly diverge.
    expect(errorRateSeries(attempts, fails)).toEqual([{ scope: 'provider:x', points: [1.8] }])
  })
})

describe('seriesColor', () => {
  it('cycles through RELIABILITY_SERIES_COLORS by index', () => {
    expect(seriesColor(0)).toBe(RELIABILITY_SERIES_COLORS[0])
    expect(seriesColor(RELIABILITY_SERIES_COLORS.length)).toBe(RELIABILITY_SERIES_COLORS[0])
    expect(seriesColor(RELIABILITY_SERIES_COLORS.length + 1)).toBe(RELIABILITY_SERIES_COLORS[1])
  })
})

const emptyOverview: AdminOverviewResponse = {
  version: '1',
  replica: 'pod-a',
  instance: 'test-gateway',
  warnings: [],
  providers: [],
  groups: [],
  aliases: [],
  redis: { configured: false, lastErrAt: '0001-01-01T00:00:00Z' },
  cache: { enabled: false },
  retry: { enabled: false },
  features: { userModelStats: false, latencyStats: false, cacheStats: false, lastSeen: false, failover: false },
  pricing: { override: 0, builtin: 0, litellm: 0, free: 0, unpriced: 0 },
}

function providerOverview(names: string[]): AdminOverviewResponse {
  return {
    ...emptyOverview,
    providers: names.map((name) => ({
      name,
      type: 'openai',
      baseUrl: 'https://api.example.com',
      lastRefresh: '0001-01-01T00:00:00Z',
      models: [],
      modelCount: 0,
      modelMeta: {},
      discoveryEnabled: false,
      healthState: 'closed',
      openUntil: '0001-01-01T00:00:00Z',
      attemptsDay: 0,
      failuresDay: 0,
      attemptsMinute: 0,
      failuresMinute: 0,
      modelRates: {},
    })),
  }
}

describe('useReliabilityStore.fetch', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
  })

  it('short-circuits to empty state with no adminFetch calls when no providers are configured', async () => {
    useDashboardStore().overview = emptyOverview
    const reliability = useReliabilityStore()

    await reliability.fetch()

    expect(mockedAdminFetch).not.toHaveBeenCalled()
    expect(reliability.errorRate).toEqual([])
    expect(reliability.lastUpdated).not.toBeNull()
    expect(reliability.error).toBe('')
  })

  // Live-preview fix: dashboard.overview being null means the fleet's
  // provider list is UNKNOWN, not genuinely zero — fetch() must not resolve
  // as "loaded, empty" (the bug above's own short-circuit used to do this
  // BEFORE dashboard.overview had ever landed too), or ReliabilityPage.vue's
  // per-provider charts briefly render an empty grid on a cold reload
  // instead of staying on their skeleton.
  it('no-ops with no adminFetch calls and leaves state untouched when dashboard.overview has not loaded yet', async () => {
    // useDashboardStore().overview defaults to null — never assigned here.
    const reliability = useReliabilityStore()

    await reliability.fetch()

    expect(mockedAdminFetch).not.toHaveBeenCalled()
    expect(reliability.buckets).toEqual([])
    expect(reliability.lastUpdated).toBeNull()
    expect(reliability.error).toBe('')
    expect(reliability.loading).toBe(false)
  })

  it('fetches attempt/fail/timeout/fover per configured provider scope, plus total rej/r402, and assigns every field', async () => {
    useDashboardStore().overview = providerOverview(['openai', 'anthropic'])
    const reliability = useReliabilityStore()

    function seriesFor(metric: string, points: number[]): AdminSeriesResponse {
      return {
        metric,
        window: 'day',
        span: points.length,
        offset: 0,
        buckets: points.map((_, i) => `2026010${i + 1}`),
        series:
          metric === 'rej' || metric === 'r402'
            ? [{ scope: 'total', points }]
            : [
                { scope: 'provider:openai', points },
                { scope: 'provider:anthropic', points: points.map((p) => p * 2) },
              ],
        unknown: [],
      }
    }

    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path.includes('metric=attempt')) return seriesFor('attempt', [100, 100])
      if (path.includes('metric=fail')) return seriesFor('fail', [1, 2])
      if (path.includes('metric=timeout')) return seriesFor('timeout', [0, 1])
      if (path.includes('metric=fover')) return seriesFor('fover', [0, 0])
      if (path.includes('metric=rej')) return seriesFor('rej', [3, 4])
      if (path.includes('metric=r402')) return seriesFor('r402', [0, 1])
      throw new Error(`unexpected path ${path}`)
    }) as typeof adminFetch)

    await reliability.fetch()

    expect(reliability.buckets).toEqual(['20260101', '20260102'])
    expect(reliability.windowUsed).toBe('day')
    expect(reliability.errorRate).toEqual([
      { scope: 'provider:openai', points: [0.01, 0.02] },
      { scope: 'provider:anthropic', points: [0.01, 0.02] },
    ])
    expect(reliability.timeouts).toHaveLength(2)
    expect(reliability.failovers).toHaveLength(2)
    expect(reliability.rejections).toEqual([3, 4])
    expect(reliability.unpriced).toEqual([0, 1])
    expect(reliability.lastUpdated).not.toBeNull()
    expect(reliability.error).toBe('')
  })

  // P2 item 6: fetch() used to have an `if (this.loading) return` guard,
  // which silently dropped a range change that started while a fetch was
  // already in flight. The replacement is latest-request-wins (reqId) —
  // this test exercises the property that actually matters: a SLOWER,
  // earlier fetch() resolving AFTER a faster, later one must never
  // overwrite the later (more current) result with stale data.
  it('discards a slower first fetch() result once a newer fetch() has already resolved (latest-request-wins)', async () => {
    useDashboardStore().overview = providerOverview(['openai'])
    const reliability = useReliabilityStore()

    function seriesFor(metric: string, scope: string, points: number[]): AdminSeriesResponse {
      return { metric, window: 'day', span: points.length, offset: 0, buckets: ['20260101', '20260102'], series: [{ scope, points }], unknown: [] }
    }

    let resolveFirstAttempt: ((v: AdminSeriesResponse) => void) | undefined
    let attemptCallCount = 0
    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path.includes('metric=attempt')) {
        attemptCallCount += 1
        // The FIRST fetch()'s own attempt call hangs until the test
        // resolves it by hand, below — every LATER attempt call
        // (`second`'s, and any after it) resolves immediately.
        if (attemptCallCount === 1) return new Promise<AdminSeriesResponse>((resolve) => (resolveFirstAttempt = resolve))
        return seriesFor('attempt', 'provider:openai', [9, 9])
      }
      if (path.includes('metric=fail')) return seriesFor('fail', 'provider:openai', [0, 0])
      if (path.includes('metric=timeout')) return seriesFor('timeout', 'provider:openai', [0, 0])
      if (path.includes('metric=fover')) return seriesFor('fover', 'provider:openai', [0, 0])
      if (path.includes('metric=rej')) return seriesFor('rej', 'total', [9, 9])
      if (path.includes('metric=r402')) return seriesFor('r402', 'total', [0, 0])
      throw new Error(`unexpected path ${path}`)
    }) as typeof adminFetch)

    const first = reliability.fetch()
    await vi.waitFor(() => {
      if (!resolveFirstAttempt) throw new Error('first attempt request not issued yet')
    })
    const second = reliability.fetch()
    await second
    expect(reliability.rejections).toEqual([9, 9])

    // `first` finally resolves, with STALE data — it must be discarded,
    // not overwrite the already-current `second` result.
    resolveFirstAttempt?.(seriesFor('attempt', 'provider:openai', [1, 1]))
    await first
    expect(reliability.rejections).toEqual([9, 9])
    expect(reliability.loading).toBe(false)
  })

  it('records a non-auth failure message without throwing', async () => {
    useDashboardStore().overview = providerOverview(['openai'])
    const reliability = useReliabilityStore()
    mockedAdminFetch.mockImplementation((async () => {
      throw new Error('series boom')
    }) as typeof adminFetch)

    await reliability.fetch()

    expect(reliability.error).toContain('series boom')
  })
})

// fetchProviderPerf (N3 fix, verify-redesign-final.md: "performance
// series=1 for the selected provider" was never wired up).
describe('useReliabilityStore.fetchProviderPerf', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
  })

  function overviewWithLatency(enabled: boolean): AdminOverviewResponse {
    return { ...providerOverview(['openai']), features: { ...emptyOverview.features, latencyStats: enabled } }
  }

  it('short-circuits with no adminFetch call when no provider is selected', async () => {
    useDashboardStore().overview = overviewWithLatency(true)
    const reliability = useReliabilityStore()

    await reliability.fetchProviderPerf('')

    expect(mockedAdminFetch).not.toHaveBeenCalled()
    expect(reliability.providerPerfPoints).toEqual([])
    expect(reliability.providerPerfBuckets).toEqual([])
  })

  it('short-circuits with no adminFetch call when admin.stats.latency is off, even with a provider selected', async () => {
    useDashboardStore().overview = overviewWithLatency(false)
    const reliability = useReliabilityStore()

    await reliability.fetchProviderPerf('openai')

    expect(mockedAdminFetch).not.toHaveBeenCalled()
    expect(reliability.providerPerfLatencyEnabled).toBe(false)
  })

  it('fetches GET /admin/api/performance?kind=provider&series=1 for exactly the selected provider, and assigns every field', async () => {
    useDashboardStore().overview = overviewWithLatency(true)
    const reliability = useReliabilityStore()

    const response: AdminPerfResponse = {
      kind: 'provider',
      window: 'day',
      span: 2,
      offset: 0,
      buckets: ['20260101', '20260102'],
      points: [
        { id: '20260101', p50Ms: 10, p95Ms: 20, count: 5, attempts: 5, failures: 1 },
        { id: '20260102', count: 3, attempts: 3, failures: 0 },
      ],
      latencyEnabled: true,
    }
    mockedAdminFetch.mockImplementation((async (path: string) => {
      expect(path).toContain('kind=provider')
      expect(path).toContain('series=1')
      expect(path).toContain('id=openai')
      return response
    }) as typeof adminFetch)

    await reliability.fetchProviderPerf('openai')

    expect(reliability.providerPerfBuckets).toEqual(['20260101', '20260102'])
    expect(reliability.providerPerfWindowUsed).toBe('day')
    expect(reliability.providerPerfPoints).toEqual(response.points)
    expect(reliability.providerPerfLatencyEnabled).toBe(true)
    expect(reliability.providerPerfError).toBe('')
  })

  it('discards a slower first fetchProviderPerf() result once a newer one has already resolved (latest-request-wins)', async () => {
    useDashboardStore().overview = overviewWithLatency(true)
    const reliability = useReliabilityStore()

    function response(points: AdminPerfRow[]): AdminPerfResponse {
      return { kind: 'provider', window: 'day', span: points.length, offset: 0, buckets: ['20260101', '20260102'], points, latencyEnabled: true }
    }

    let resolveFirst: ((v: AdminPerfResponse) => void) | undefined
    let callCount = 0
    mockedAdminFetch.mockImplementation((async () => {
      callCount += 1
      if (callCount === 1) return new Promise<AdminPerfResponse>((resolve) => (resolveFirst = resolve))
      return response([{ id: '20260101', count: 9, attempts: 9, failures: 9 }])
    }) as typeof adminFetch)

    const first = reliability.fetchProviderPerf('openai')
    await vi.waitFor(() => {
      if (!resolveFirst) throw new Error('first request not issued yet')
    })
    const second = reliability.fetchProviderPerf('openai')
    await second
    expect(reliability.providerPerfPoints).toEqual([{ id: '20260101', count: 9, attempts: 9, failures: 9 }])

    resolveFirst?.(response([{ id: '20260101', count: 1, attempts: 1, failures: 1 }]))
    await first
    expect(reliability.providerPerfPoints).toEqual([{ id: '20260101', count: 9, attempts: 9, failures: 9 }])
    expect(reliability.providerPerfLoading).toBe(false)
  })

  it('records a non-auth failure message without throwing', async () => {
    useDashboardStore().overview = overviewWithLatency(true)
    const reliability = useReliabilityStore()
    mockedAdminFetch.mockImplementation((async () => {
      throw new Error('perf series boom')
    }) as typeof adminFetch)

    await reliability.fetchProviderPerf('openai')

    expect(reliability.providerPerfError).toContain('perf series boom')
  })
})
