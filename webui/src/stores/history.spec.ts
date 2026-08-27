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
