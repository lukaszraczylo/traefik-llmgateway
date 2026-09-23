// consumers.spec.ts mirrors dashboard.spec.ts's own mocking convention
// (that file's own doc comment): adminFetch (lib/api.ts) is mocked
// entirely, no real network or sessionStorage access.

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/lib/api', async () => {
  const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
  return { ...actual, adminFetch: vi.fn() }
})

import { AdminApiError, adminFetch } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import { useConsumersStore } from '@/stores/consumers'
import type { AdminConsumersResponse, AdminSeriesResponse, AdminTotalsResponse } from '@/types/api'

const mockedAdminFetch = vi.mocked(adminFetch)

const emptyConsumers: AdminConsumersResponse = { users: [], groups: [] }
const emptySeries: AdminSeriesResponse = { metric: 'req', window: 'day', span: 7, offset: 0, buckets: [], series: [], unknown: [] }
const emptyTotals: AdminTotalsResponse = { kind: 'usermodel', window: 'day', span: 7, offset: 0, metrics: ['req'], rows: [] }

describe('useConsumersStore.fetchConsumers / ensureConsumers', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
  })

  it('fetches and stores the /admin/api/consumers response', async () => {
    mockedAdminFetch.mockResolvedValue(emptyConsumers)
    const store = useConsumersStore()
    await store.fetchConsumers()

    expect(mockedAdminFetch).toHaveBeenCalledWith('/admin/api/consumers')
    // toEqual (not toBe): Pinia wraps state in a reactive Proxy, so
    // store.data is never the same object reference as the fixture
    // adminFetch resolved with, only structurally identical.
    expect(store.data).toEqual(emptyConsumers)
    expect(store.error).toBe('')
    expect(store.fetchedAt).not.toBeNull()
  })

  it('records the error message on failure', async () => {
    mockedAdminFetch.mockRejectedValue(new Error('boom'))
    const store = useConsumersStore()
    await store.fetchConsumers()

    expect(store.error).toBe('boom')
  })

  it('does not surface an error on a 401 (the auth store owns that case)', async () => {
    mockedAdminFetch.mockRejectedValue(new AdminApiError('unauthorized', 401))
    const store = useConsumersStore()
    await store.fetchConsumers()

    expect(store.error).toBe('')
  })

  it('ensureConsumers fetches when there is no data yet', async () => {
    mockedAdminFetch.mockResolvedValue(emptyConsumers)
    const store = useConsumersStore()
    await store.ensureConsumers()

    expect(mockedAdminFetch).toHaveBeenCalledTimes(1)
  })

  it('ensureConsumers skips a second call while the first fetch is still fresh', async () => {
    mockedAdminFetch.mockResolvedValue(emptyConsumers)
    const store = useConsumersStore()
    await store.ensureConsumers()
    await store.ensureConsumers()

    expect(mockedAdminFetch).toHaveBeenCalledTimes(1)
  })

  it('ensureConsumers re-fetches once the staleness window has passed', async () => {
    vi.useFakeTimers()
    try {
      mockedAdminFetch.mockResolvedValue(emptyConsumers)
      const store = useConsumersStore()
      await store.ensureConsumers()

      vi.advanceTimersByTime(60_001)
      await store.ensureConsumers()

      expect(mockedAdminFetch).toHaveBeenCalledTimes(2)
    } finally {
      vi.useRealTimers()
    }
  })
})

describe('useConsumersStore.fetchUserDetail', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
  })

  it('fetches req and cost series for the given user, keyed by user id', async () => {
    mockedAdminFetch.mockResolvedValue(emptySeries)
    const store = useConsumersStore()
    await store.fetchUserDetail('alice', 'day', 7, false)

    const paths = mockedAdminFetch.mock.calls.map((c) => c[0] as string)
    expect(paths.some((p) => p.includes('scope=user%3Aalice') && p.includes('metric=req'))).toBe(true)
    expect(paths.some((p) => p.includes('scope=user%3Aalice') && p.includes('metric=cost'))).toBe(true)
    expect(store.detail.alice.loading).toBe(false)
    expect(store.detail.alice.reqError).toBe('')
    expect(store.detail.alice.costError).toBe('')
    expect(store.detail.alice.reqSeries).toEqual(emptySeries)
    expect(store.detail.alice.costSeries).toEqual(emptySeries)
  })

  it('does not fetch usermodel totals when userModelStatsEnabled is false', async () => {
    mockedAdminFetch.mockResolvedValue(emptySeries)
    const store = useConsumersStore()
    await store.fetchUserDetail('alice', 'day', 7, false)

    const paths = mockedAdminFetch.mock.calls.map((c) => c[0] as string)
    expect(paths.some((p) => p.includes('kind=usermodel'))).toBe(false)
    expect(store.detail.alice.modelTotals).toBeNull()
  })

  it('fetches usermodel totals when userModelStatsEnabled is true', async () => {
    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path.includes('kind=usermodel')) return emptyTotals
      return emptySeries
    }) as typeof adminFetch)
    const store = useConsumersStore()
    await store.fetchUserDetail('alice', 'day', 7, true)

    expect(store.detail.alice.modelTotals).toEqual(emptyTotals)
  })

  // P1 item 2: usermodel only accepts day/month (no hour bucket at all) —
  // a bare `window=hour` 400s server-side BEFORE the 404-disabled-feature
  // gate even runs. The previous spec matched adminFetch by path PREFIX
  // only and never passed an hour-resolution window, so it never caught
  // this. This test asserts the exact usermodel URL when the caller
  // passes an hour window (the 24h/48h global ranges).
  it('clamps an hour-resolution window to day/month (span 7) for the usermodel totals call, never window=hour', async () => {
    let usermodelUrl = ''
    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path.includes('kind=usermodel')) {
        usermodelUrl = path
        return emptyTotals
      }
      return emptySeries
    }) as typeof adminFetch)
    const store = useConsumersStore()
    await store.fetchUserDetail('alice', 'hour', 48, true)

    expect(usermodelUrl).toBe('/admin/api/usage/totals?kind=usermodel&window=day&span=7&metrics=req%2Ccost&user=alice')
  })

  // P1 item 2: the req/cost series calls are UNCLAMPED — they legitimately
  // accept any window (h/d/m), unlike usermodel. This pins that the fix
  // above did not over-broadly clamp every call this action makes.
  it('does NOT clamp the req/cost series calls — they pass the caller\'s window straight through', async () => {
    const paths: string[] = []
    mockedAdminFetch.mockImplementation((async (path: string) => {
      paths.push(path)
      if (path.includes('kind=usermodel')) return emptyTotals
      return emptySeries
    }) as typeof adminFetch)
    const store = useConsumersStore()
    await store.fetchUserDetail('alice', 'hour', 48, false)

    const seriesUrls = paths.filter((p) => p.includes('/usage/series'))
    expect(seriesUrls).toHaveLength(2)
    for (const url of seriesUrls) expect(url).toContain('window=hour&span=48')
  })

  it('records a per-user error without clobbering a different user\'s already-loaded detail', async () => {
    mockedAdminFetch.mockResolvedValue(emptySeries)
    const store = useConsumersStore()
    await store.fetchUserDetail('alice', 'day', 7, false)

    mockedAdminFetch.mockRejectedValue(new Error('boom'))
    await store.fetchUserDetail('bob', 'day', 7, false)

    expect(store.detail.alice.reqError).toBe('')
    expect(store.detail.alice.costError).toBe('')
    expect(store.detail.alice.reqSeries).toEqual(emptySeries)
    expect(store.detail.bob.reqError).toBe('boom')
    expect(store.detail.bob.costError).toBe('boom')
  })

  // verify-ui-states.md #4: a failure in only ONE of the three parallel
  // requests must set only THAT field's own error, never a shared field
  // the req/cost charts both read — an unrelated modelTotals failure used
  // to replace two perfectly valid, already-loaded charts with ErrorState.
  it('scopes each request\'s failure to its own error field, never a shared one', async () => {
    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path.includes('kind=usermodel')) throw new Error('usermodel boom')
      return emptySeries
    }) as typeof adminFetch)
    const store = useConsumersStore()
    await store.fetchUserDetail('alice', 'day', 7, true)

    expect(store.detail.alice.reqError).toBe('')
    expect(store.detail.alice.costError).toBe('')
    expect(store.detail.alice.modelError).toBe('usermodel boom')
    expect(store.detail.alice.reqSeries).toEqual(emptySeries)
    expect(store.detail.alice.costSeries).toEqual(emptySeries)
  })

  // verify-ui-states.md #4: re-fetching the SAME range (a retry, or the
  // featuresUserModelStats-triggered follow-up UserDetail.vue's own watch
  // fires) must keep the previous series in `detail` while `loading` is
  // true, so the caller's own `loading && !reqSeries` skeleton condition
  // does not flip back on over data already on screen — this is also
  // exactly what makes re-opening a cached user show its data immediately.
  it('keeps the previous series while loading is true for a SAME-range refetch', async () => {
    mockedAdminFetch.mockResolvedValue(emptySeries)
    const store = useConsumersStore()
    await store.fetchUserDetail('alice', 'day', 7, false)
    expect(store.detail.alice.reqSeries).toEqual(emptySeries)

    // Second call never resolves within this assertion window — read
    // `detail.alice` synchronously right after firing it, before either
    // of the two (req, cost) in-flight fetches settle, to inspect the
    // mid-flight state. Both pending calls share this one mock
    // implementation, so every resolver gets collected and released.
    const resolvers: (() => void)[] = []
    mockedAdminFetch.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvers.push(() => resolve(emptySeries))
        }),
    )
    const inFlight = store.fetchUserDetail('alice', 'day', 7, false)
    expect(store.detail.alice.loading).toBe(true)
    expect(store.detail.alice.reqSeries).toEqual(emptySeries)

    resolvers.forEach((resolve) => resolve())
    await inFlight
  })

  // verify-ui-states.md #4: a genuine RANGE change for the same user
  // clears the previous range's own series before the new fetch starts,
  // so a stale (wrong-range) chart never sits under the new heading.
  it('clears the previous series when the range genuinely changes', async () => {
    mockedAdminFetch.mockResolvedValue(emptySeries)
    const store = useConsumersStore()
    await store.fetchUserDetail('alice', 'day', 7, false)
    expect(store.detail.alice.reqSeries).toEqual(emptySeries)

    const resolvers: (() => void)[] = []
    mockedAdminFetch.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvers.push(() => resolve(emptySeries))
        }),
    )
    const inFlight = store.fetchUserDetail('alice', 'day', 14, false)
    expect(store.detail.alice.loading).toBe(true)
    expect(store.detail.alice.reqSeries).toBeNull()
    expect(store.detail.alice.costSeries).toBeNull()

    resolvers.forEach((resolve) => resolve())
    await inFlight
  })

  // verify-ui-states-2.md #7: a genuine RANGE change must also clear the
  // PREVIOUS range's own per-section errors, not just its series/totals —
  // otherwise loadState's own "error beats loading" precedence keeps the
  // old range's ErrorState on screen under the new range's heading instead
  // of the skeleton the cleared reqSeries/costSeries/modelTotals is meant
  // to produce.
  it('clears reqError/costError/modelError when the range genuinely changes', async () => {
    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path.includes('kind=usermodel')) throw new Error('usermodel boom')
      throw new Error('series boom')
    }) as typeof adminFetch)
    const store = useConsumersStore()
    await store.fetchUserDetail('alice', 'day', 7, true)
    expect(store.detail.alice.reqError).toBe('series boom')
    expect(store.detail.alice.costError).toBe('series boom')
    expect(store.detail.alice.modelError).toBe('usermodel boom')

    const resolvers: (() => void)[] = []
    mockedAdminFetch.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvers.push(() => resolve(emptySeries))
        }),
    )
    const inFlight = store.fetchUserDetail('alice', 'day', 14, true)
    expect(store.detail.alice.reqError).toBe('')
    expect(store.detail.alice.costError).toBe('')
    expect(store.detail.alice.modelError).toBe('')

    resolvers.forEach((resolve) => resolve())
    await inFlight
  })

  it('keeps the previous errors on a SAME-range refetch (a retry) — only a genuine range change clears them', async () => {
    mockedAdminFetch.mockRejectedValue(new Error('boom'))
    const store = useConsumersStore()
    await store.fetchUserDetail('alice', 'day', 7, false)
    expect(store.detail.alice.reqError).toBe('boom')

    const resolvers: (() => void)[] = []
    mockedAdminFetch.mockImplementation(
      () =>
        new Promise((resolve) => {
          resolvers.push(() => resolve(emptySeries))
        }),
    )
    const inFlight = store.fetchUserDetail('alice', 'day', 7, false)
    expect(store.detail.alice.reqError).toBe('boom')

    resolvers.forEach((resolve) => resolve())
    await inFlight
  })
})
