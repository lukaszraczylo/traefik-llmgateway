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
  return { metric: 'cost', window: 'hour', span: 24, models }
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

  // F2 (dashboard-plan.md): the ranking now sums the same WINDOW_SPAN
  // buckets the time-series tabs chart, not just window's current bucket —
  // WINDOW_LABEL ("24h"/"30d"/"12mo") is honest for the Models tab too.
  it('asks the ranking endpoint for the window span, not just the current bucket', async () => {
    routeFetch({})
    const history = useHistoryStore()
    history.tab = 'models'

    await history.refresh()

    const rankingCall = mockedAdminFetch.mock.calls
      .map((c) => String(c[0]))
      .find((p) => p.startsWith('/admin/api/usage/models'))
    expect(rankingCall).toContain('span=24') // WINDOW_SPAN.hour, the default window
  })

  it('asks the picker list for requests, so a free model is still selectable', async () => {
    routeFetch({})
    const history = useHistoryStore()

    await history.fetchModelOptions()

    const optionsCall = mockedAdminFetch.mock.calls.map((c) => String(c[0])).find((p) => p.includes('/usage/models'))
    expect(optionsCall).toContain('metric=req')
    expect(optionsCall).toContain('span=24') // WINDOW_SPAN.hour, the default window
  })

  // Q1 (dashboard-plan.md DECISIONS, read-amplification review finding):
  // fetchModelOptions is a real extra store read and must NOT repeat on
  // every refresh() call — refresh() is what both the 30s auto-refresh
  // tick and every setScope/setTab/setModelMetric change call.
  it('never fetches the picker list from a plain refresh() call (Q1: read amplification)', async () => {
    routeFetch({ series: () => seriesResponse() })
    const history = useHistoryStore()

    await history.refresh()

    expect(history.seriesByMetric.req).toHaveLength(1)
    expect(history.modelOptions).toEqual([])
    const optionsCall = mockedAdminFetch.mock.calls.map((c) => String(c[0])).some((p) => p.includes('/usage/models'))
    expect(optionsCall).toBe(false)
  })

  it('fetches the picker list on a window change, unlike a scope/tab/modelMetric change', async () => {
    routeFetch({ models: () => modelsResponse([{ id: 'alpha/a-model-1', value: 3 }]) })
    const history = useHistoryStore()

    history.setWindow('day')
    await flush()

    expect(history.modelOptions).toEqual([{ id: 'alpha/a-model-1', value: 3 }])
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

  // setWindow fires refresh() (series/ranking) and fetchModelOptions
  // (picker list) in parallel (Q1's two trigger points) — one failing must
  // never touch the other's own state.
  it('keeps the freshly-fetched chart intact when the picker list fails on the same window change', async () => {
    routeFetch({
      series: () => seriesResponse(),
      models: () => {
        throw new Error('models endpoint down')
      },
    })
    const history = useHistoryStore()

    history.setWindow('day')
    await flush()

    expect(history.seriesByMetric.req).toHaveLength(1)
    expect(history.error).toBe('')
    expect(history.modelOptions).toEqual([])
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

// F6 (dashboard-plan.md): modelFilter narrows the Models tab's ranking to a
// provider/model prefix, set by ProvidersView.vue's provider-header link
// (nav.goToModels) or a `#charts?tab=models&filter=...` hash param
// (lib/hash-state.ts, composables/useHashState.ts). P3 review fix: while
// the Models tab is active, setModelFilter ALSO sends the new prefix to
// the server and refetches the ranking (fetchModelRanking's own doc
// comment) — it is no longer a pure client-side re-slice. Setting it while
// any OTHER tab is active stays fetch-free, since only the Models tab
// reads modelFilter at all (ChartsView.vue).
describe('useHistoryStore model filter (F6)', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
  })

  it('starts with no filter', () => {
    expect(useHistoryStore().modelFilter).toBe('')
  })

  it('setModelFilter sets the prefix without fetching anything when not on the Models tab', async () => {
    routeFetch({})
    const history = useHistoryStore()
    history.tab = 'requests' // explicit, not relying on the store's own default
    const callsBefore = mockedAdminFetch.mock.calls.length

    history.setModelFilter('openai/')
    await flush()

    expect(history.modelFilter).toBe('openai/')
    expect(mockedAdminFetch.mock.calls.length).toBe(callsBefore)
  })

  it('setModelFilter refetches the ranking with the encoded prefix when the Models tab IS active', async () => {
    routeFetch({})
    const history = useHistoryStore()
    history.tab = 'models'
    const callsBefore = mockedAdminFetch.mock.calls.length

    history.setModelFilter('openai/')
    await flush()

    expect(history.modelFilter).toBe('openai/')
    expect(mockedAdminFetch.mock.calls.length).toBeGreaterThan(callsBefore)
    const rankingCall = mockedAdminFetch.mock.calls.map((c) => String(c[0])).find((p) => p.startsWith('/admin/api/usage/models'))
    expect(rankingCall).toContain(`prefix=${encodeURIComponent('openai/')}`)
  })

  it('setTab clears a previously-set modelFilter (a plain tab click must not carry a stale provider filter)', () => {
    routeFetch({})
    const history = useHistoryStore()
    history.setModelFilter('openai/')

    history.setTab('tokens')

    expect(history.modelFilter).toBe('')
  })

  it('setTab(models) also clears modelFilter — nav.goToModels relies on re-applying setModelFilter AFTER this', () => {
    routeFetch({})
    const history = useHistoryStore()
    history.tab = 'requests'
    history.setModelFilter('openai/')

    history.setTab('models')

    expect(history.modelFilter).toBe('')
  })

  it('setTab is a no-op (including on modelFilter) when the tab does not actually change', () => {
    routeFetch({})
    const history = useHistoryStore()
    history.tab = 'models'
    history.setModelFilter('openai/')

    history.setTab('models') // already on 'models' — the early return must skip the clear too

    expect(history.modelFilter).toBe('openai/')
  })

  // P3 review fix: the ranking fetch now sends the active prefix to the
  // server (filtering BEFORE the limit is applied), instead of only ever
  // re-slicing an already-fetched, unprefixed top-N client-side.
  it('sends &prefix=<encoded filter> on the ranking fetch when modelFilter is set', async () => {
    routeFetch({})
    const history = useHistoryStore()
    history.tab = 'models'
    history.modelFilter = 'openai/'

    await history.fetchModelRanking()

    const rankingCall = mockedAdminFetch.mock.calls.map((c) => String(c[0])).find((p) => p.startsWith('/admin/api/usage/models'))
    expect(rankingCall).toContain(`prefix=${encodeURIComponent('openai/')}`)
  })

  it('omits the prefix param entirely when modelFilter is empty', async () => {
    routeFetch({})
    const history = useHistoryStore()
    history.tab = 'models'

    await history.fetchModelRanking()

    const rankingCall = mockedAdminFetch.mock.calls.map((c) => String(c[0])).find((p) => p.startsWith('/admin/api/usage/models'))
    expect(rankingCall).not.toContain('prefix=')
  })

  // P3 review fix: setModelFilter now refetches too, but ONLY while the
  // Models tab is active — a stale (unprefixed, or differently-prefixed)
  // already-fetched ranking must not linger once the filter changes, since
  // the server-side prefix now genuinely changes what that fetch returns.
  it('setModelFilter refetches the ranking when the Models tab is active and the prefix actually changes', async () => {
    routeFetch({})
    const history = useHistoryStore()
    history.tab = 'models'
    const callsBefore = mockedAdminFetch.mock.calls.length

    history.setModelFilter('openai/')
    await flush()

    expect(mockedAdminFetch.mock.calls.length).toBeGreaterThan(callsBefore)
    const rankingCall = mockedAdminFetch.mock.calls.map((c) => String(c[0])).find((p) => p.startsWith('/admin/api/usage/models'))
    expect(rankingCall).toContain(`prefix=${encodeURIComponent('openai/')}`)
  })

  it('setModelFilter does NOT refetch when the value is unchanged (early return)', async () => {
    routeFetch({})
    const history = useHistoryStore()
    history.tab = 'models'
    history.setModelFilter('openai/')
    await flush()
    const callsBefore = mockedAdminFetch.mock.calls.length

    history.setModelFilter('openai/') // same value again

    expect(mockedAdminFetch.mock.calls.length).toBe(callsBefore)
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

  it('the 30s auto-refresh tick (startAutoRefresh) never re-fetches the picker list — only refresh() itself (Q1)', async () => {
    vi.useFakeTimers()
    try {
      routeFetch({ series: () => seriesResponse() })
      const history = useHistoryStore()

      history.startAutoRefresh() // ticks once immediately, then every REFRESH_MS (lib/polling.ts)
      await vi.advanceTimersByTimeAsync(0)
      await vi.advanceTimersByTimeAsync(90_000) // three more ticks
      await vi.advanceTimersByTimeAsync(0)

      expect(history.modelOptions).toEqual([])
      const optionsCall = mockedAdminFetch.mock.calls.map((c) => String(c[0])).some((p) => p.includes('/usage/models'))
      expect(optionsCall).toBe(false)
      history.stopAutoRefresh()
    } finally {
      vi.useRealTimers()
    }
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
// P12 review fix: restoring a Charts hash with several params set at once
// (composables/useHashState.ts's applyFromHash) or clicking through via
// nav.ts's goToCharts/goToModels used to call several of the single-field
// setters above back to back, each independently clearing state and
// firing its OWN refresh() (and, for a window change, fetchModelOptions()
// too) — every fetch but the last immediately superseded and discarded.
// setSelection batches the whole change into ONE refresh() call.
describe('useHistoryStore.setSelection (P12: batched, one fetch)', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
  })

  it('applying scope+window+tab+modelMetric together fires exactly one series fetch (plus one picker-list fetch for the window change) — not one per field', async () => {
    routeFetch({ series: () => seriesResponse() })
    const history = useHistoryStore()

    // tab stays at its default ('requests', metricsForTab -> 1 metric), so
    // the "one series fetch" below is a single adminFetch call, not the
    // 2-call 'tokens' case — isolating the batching behavior itself from
    // metricsForTab's own separate multi-metric fan-out.
    history.setSelection({ scope: 'user:alice', window: 'day', tab: 'requests', modelMetric: 'req' })
    await flush()

    expect(history.scope).toBe('user:alice')
    expect(history.window).toBe('day')
    expect(history.modelMetric).toBe('req')
    const calls = mockedAdminFetch.mock.calls.map((c) => String(c[0]))
    const seriesCalls = calls.filter((p) => p.startsWith('/admin/api/usage/history'))
    const optionsCalls = calls.filter((p) => p.startsWith('/admin/api/usage/models'))
    expect(seriesCalls).toHaveLength(1) // one series fetch, not the 3-4 the old per-field setters would have fired
    expect(optionsCalls).toHaveLength(1) // fetchModelOptions, fired once for the window change
  })

  it('does nothing (no state change, no fetch) when every field already matches the current selection', async () => {
    routeFetch({})
    const history = useHistoryStore()
    const callsBefore = mockedAdminFetch.mock.calls.length

    history.setSelection({ scope: 'total', window: 'hour', tab: 'requests', modelMetric: 'cost' }) // all defaults
    await flush()

    expect(mockedAdminFetch.mock.calls.length).toBe(callsBefore)
  })

  it('applies modelFilter AFTER tab resets it — setSelection({ tab: "models", modelFilter: prefix }) lands on the intended prefix (nav.ts goToModels\' own call shape)', async () => {
    routeFetch({})
    const history = useHistoryStore()

    history.setSelection({ tab: 'models', modelFilter: 'openai/' })
    await flush()

    expect(history.tab).toBe('models')
    expect(history.modelFilter).toBe('openai/')
    const rankingCall = mockedAdminFetch.mock.calls.map((c) => String(c[0])).find((p) => p.startsWith('/admin/api/usage/models'))
    expect(rankingCall).toContain(`prefix=${encodeURIComponent('openai/')}`)
    // Only ONE ranking fetch — not a first unprefixed one from the tab
    // switch followed by a second, corrective one from the filter.
    const rankingCalls = mockedAdminFetch.mock.calls.map((c) => String(c[0])).filter((p) => p.startsWith('/admin/api/usage/models'))
    expect(rankingCalls).toHaveLength(1)
  })

  it('only fetchModelOptions is skipped when window is not part of the call, even though other fields change', async () => {
    routeFetch({ series: () => seriesResponse() })
    const history = useHistoryStore()

    history.setSelection({ scope: 'user:alice' })
    await flush()

    const optionsCalls = mockedAdminFetch.mock.calls.map((c) => String(c[0])).filter((p) => p.startsWith('/admin/api/usage/models'))
    expect(optionsCalls).toHaveLength(0)
  })
})
