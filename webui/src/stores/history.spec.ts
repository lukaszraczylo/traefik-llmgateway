// history.spec.ts covers useHistoryStore's model-ranking behaviour: the
// Models tab reads a different endpoint from the other three tabs, and the
// scope picker's model list is refreshed alongside whichever of the two is
// active. Both must degrade independently — a failing picker list must
// never blank a rendered chart or surface an error over it, which is the
// asymmetry these tests pin.
//
// Same harness as dashboard.spec.ts: a Pinia store is plain reactive state
// in the 'node' environment, with adminFetch (lib/api.ts) fully mocked, so
// no real network or sessionStorage access happens.

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/lib/api', async () => {
  const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
  return { ...actual, adminFetch: vi.fn() }
})

import { adminFetch } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import { metricsForTab, useHistoryStore } from '@/stores/history'
import type { AdminUsageModelsResponse, UsageHistoryResponse } from '@/types/api'

const mockedAdminFetch = vi.mocked(adminFetch)

function modelsResponse(models: AdminUsageModelsResponse['models']): AdminUsageModelsResponse {
  return { metric: 'cost', window: 'hour', models }
}
function seriesResponse(): UsageHistoryResponse {
  return { scope: 'total', metric: 'req', window: 'hour', points: [{ bucket: '2026082012', value: 5 }] }
}

/** Routes a mocked adminFetch call by URL, so tests assert on behaviour rather than call order. */
function routeFetch(handlers: { models?: () => unknown; series?: () => unknown }): void {
  mockedAdminFetch.mockImplementation((path: string) => {
    if (path.startsWith('/admin/api/usage/models')) {
      return Promise.resolve(handlers.models ? handlers.models() : modelsResponse([]))
    }
    return Promise.resolve(handlers.series ? handlers.series() : seriesResponse())
  })
}

/** deferred exposes a promise's resolve function, so a test controls exactly when a mocked fetch "arrives" — the out-of-order-responses tests below depend on resolving the NEWER request before the OLDER one. */
function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void
  const promise = new Promise<T>((res) => {
    resolve = res
  })
  return { promise, resolve }
}

/** flush drains the microtask queue — a setTimeout(0) macrotask fires only after every already-queued microtask (every chained .then/await) has run, which a plain `await Promise.resolve()` is not guaranteed to cover in one hop through fetchSeries' own multi-step await chain. */
function flush(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0))
}

describe('useHistoryStore model ranking', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
  })

  it('asks the history endpoint for no metric on the Models tab', () => {
    // The Models tab reads the ranking endpoint instead, so requesting a
    // history metric for it would be a wasted round trip per refresh.
    expect(metricsForTab('models')).toEqual([])
    expect(metricsForTab('tokens')).toEqual(['tokin', 'tokout'])
  })

  it('fetches the ranking, not a series, when the Models tab is active', async () => {
    routeFetch({ models: () => modelsResponse([{ id: 'uni/qwen3.8-flash-next', value: 4_100_000 }]) })
    const history = useHistoryStore()
    history.tab = 'models'

    await history.refresh()

    expect(history.modelRanking).toEqual([{ id: 'uni/qwen3.8-flash-next', value: 4_100_000 }])
    const paths = mockedAdminFetch.mock.calls.map((c) => String(c[0]))
    expect(paths.some((p) => p.startsWith('/admin/api/usage/history'))).toBe(false)
  })

  it('ranks by the selected metric, independently of the tab', async () => {
    routeFetch({})
    const history = useHistoryStore()
    history.tab = 'models'

    history.setModelMetric('tokin')
    await history.refresh()

    const rankingCall = mockedAdminFetch.mock.calls
      .map((c) => String(c[0]))
      .find((p) => p.startsWith('/admin/api/usage/models') && p.includes('metric=tokin'))
    expect(rankingCall).toBeDefined()
  })

  it('refreshes the picker list alongside a time series', async () => {
    routeFetch({ models: () => modelsResponse([{ id: 'alpha/a-model-1', value: 3 }]) })
    const history = useHistoryStore()

    await history.refresh()

    // A model that has just started receiving traffic becomes selectable
    // without a reload, even while a non-Models tab is on screen.
    expect(history.modelOptions).toEqual([{ id: 'alpha/a-model-1', value: 3 }])
    expect(history.seriesByMetric.req).toHaveLength(1)
  })

  it('asks the picker list for requests, so a free model is still selectable', async () => {
    routeFetch({})
    const history = useHistoryStore()

    await history.fetchModelOptions()

    const optionsCall = mockedAdminFetch.mock.calls.map((c) => String(c[0])).find((p) => p.includes('/usage/models'))
    expect(optionsCall).toContain('metric=req')
  })

  it('swallows a picker-list failure without touching the chart error', async () => {
    // Asserted directly on fetchModelOptions rather than through refresh:
    // inside refresh a succeeding series fetch clears `error` afterwards
    // anyway, which would let this pass even if the swallow were removed.
    routeFetch({
      models: () => {
        throw new Error('models endpoint down')
      },
    })
    const history = useHistoryStore()
    history.error = 'pre-existing chart error'

    await history.fetchModelOptions()

    expect(history.error).toBe('pre-existing chart error')
    expect(history.modelOptions).toEqual([])
  })

  it('keeps a rendered chart intact when the picker list fails', async () => {
    routeFetch({
      models: () => {
        throw new Error('models endpoint down')
      },
    })
    const history = useHistoryStore()

    await history.refresh()

    expect(history.seriesByMetric.req).toHaveLength(1)
    expect(history.error).toBe('')
  })

  it('surfaces a failing ranking fetch, unlike the picker list', async () => {
    mockedAdminFetch.mockImplementation((path: string) => {
      if (path.includes('limit=20')) return Promise.reject(new Error('ranking unavailable'))
      return Promise.resolve(modelsResponse([]))
    })
    const history = useHistoryStore()
    history.tab = 'models'

    await history.refresh()

    expect(history.error).toBe('ranking unavailable')
  })
})

// Review findings 1/2/9: out-of-order responses must never overwrite a
// newer selection, stale data must never render under the new selection's
// labels, and `loading` must only ever be cleared by the request that is
// still current.
describe('useHistoryStore: latest-request-wins', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
  })

  it('drops a fetchSeries response that resolves late, after a newer scope already superseded it', async () => {
    const alice = deferred<UsageHistoryResponse>()
    const ops = deferred<UsageHistoryResponse>()
    mockedAdminFetch.mockImplementation((path: string) => {
      if (path.includes('scope=user%3Aalice')) return alice.promise
      if (path.includes('scope=group%3Aops')) return ops.promise
      return Promise.resolve(modelsResponse([])) // fetchModelOptions' own parallel call
    })
    const history = useHistoryStore()

    history.setScope('user:alice') // slow — resolves LAST, below
    history.setScope('group:ops') // fast — resolves FIRST, below; must win regardless

    ops.resolve({ scope: 'group:ops', metric: 'req', window: 'hour', points: [{ bucket: '2026082012', value: 42 }] })
    await flush()
    alice.resolve({ scope: 'user:alice', metric: 'req', window: 'hour', points: [{ bucket: '2026082012', value: 999 }] })
    await flush()

    // The stale alice response must never land, however late it arrives.
    expect(history.seriesByMetric.req).toEqual([{ bucket: '2026082012', value: 42 }])
    expect(history.scope).toBe('group:ops')
  })

  it('never re-sets `loading` to true after the CURRENT request has already cleared it (a stale request resolving late)', async () => {
    const alice = deferred<UsageHistoryResponse>()
    const ops = deferred<UsageHistoryResponse>()
    mockedAdminFetch.mockImplementation((path: string) => {
      if (path.includes('scope=user%3Aalice')) return alice.promise
      if (path.includes('scope=group%3Aops')) return ops.promise
      return Promise.resolve(modelsResponse([]))
    })
    const history = useHistoryStore()

    history.setScope('user:alice')
    history.setScope('group:ops')

    ops.resolve({ scope: 'group:ops', metric: 'req', window: 'hour', points: [] })
    await flush()
    expect(history.loading).toBe(false) // the current request has already settled

    alice.resolve({ scope: 'user:alice', metric: 'req', window: 'hour', points: [] })
    await flush()
    expect(history.loading).toBe(false) // the stale request settling later must not flip it back on
  })

  it('clears seriesByMetric synchronously on a window change, before the new fetch resolves — no stale bucket renders under the new window\'s labels', () => {
    routeFetch({ series: () => seriesResponse() })
    const history = useHistoryStore()
    history.seriesByMetric = { req: [{ bucket: '2026082012', value: 5 }] }

    history.setWindow('day')

    expect(history.seriesByMetric).toEqual({})
  })

  it('clears modelRanking synchronously on a model-metric change, before the new fetch resolves', () => {
    routeFetch({})
    const history = useHistoryStore()
    history.tab = 'models'
    history.modelRanking = [{ id: 'alpha/a-model-1', value: 3 }]

    history.setModelMetric('req')

    expect(history.modelRanking).toEqual([])
  })

  it('does not clear seriesByMetric on an auto-refresh tick (same selection) — only a selection CHANGE clears it', async () => {
    routeFetch({ series: () => seriesResponse() })
    const history = useHistoryStore()
    await history.refresh()
    expect(history.seriesByMetric.req).toHaveLength(1)

    await history.refresh() // same scope/window/tab — the 30s auto-refresh's own call shape

    expect(history.seriesByMetric.req).toHaveLength(1)
  })

  it('marks the selection loaded after its first fetch and keeps it loaded across refreshes, clearing it only on a selection change', async () => {
    routeFetch({ series: () => seriesResponse() })
    const history = useHistoryStore()
    expect(history.loaded).toBe(false)
    await history.refresh()
    expect(history.loaded).toBe(true)

    await history.refresh()
    expect(history.loaded).toBe(true)

    history.setWindow('day')
    expect(history.loaded).toBe(false)
  })

  it('auto-refresh tick skips while a fetch is still in flight, so a slow request is never superseded by the timer', async () => {
    vi.useFakeTimers()
    try {
      const slow = deferred<UsageHistoryResponse>()
      mockedAdminFetch.mockImplementation((path: string) =>
        path.startsWith('/admin/api/usage/models') ? Promise.resolve(modelsResponse([])) : slow.promise,
      )
      const history = useHistoryStore()
      void history.refresh()
      const callsBefore = mockedAdminFetch.mock.calls.length
      history.startAutoRefresh()
      await vi.advanceTimersByTimeAsync(90_000)
      expect(mockedAdminFetch.mock.calls.length).toBe(callsBefore)

      slow.resolve(seriesResponse())
      await vi.advanceTimersByTimeAsync(0)
      expect(history.seriesByMetric.req).toHaveLength(1)
      history.stopAutoRefresh()
    } finally {
      vi.useRealTimers()
    }
  })
})
