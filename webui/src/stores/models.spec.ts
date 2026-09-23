// models.spec.ts covers useModelsStore.fetch's catalog-store integration
// (useCatalogStore.ensureLoaded, then join via lib/model-table-columns.ts's
// buildModelCatalogRows) — same mocked-adminFetch, Pinia-under-vitest
// harness as dashboard.spec.ts/reliability.spec.ts.

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/lib/api', async () => {
  const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
  return { ...actual, adminFetch: vi.fn() }
})

import { adminFetch } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import { useCatalogStore } from '@/stores/catalog'
import { useModelsStore } from '@/stores/models'
import type { AdminCatalogResponse, AdminPerfResponse, AdminUsageModelsResponse } from '@/types/api'

const mockedAdminFetch = vi.mocked(adminFetch)

const catalogResponse: AdminCatalogResponse = {
  providers: [
    {
      lastRefresh: '2026-01-01T00:00:00Z',
      openUntil: '0001-01-01T00:00:00Z',
      name: 'openai',
      type: 'openai',
      healthState: 'closed',
      discoveryEnabled: true,
      models: [{ id: 'openai/gpt-5', model: 'gpt-5', priceSource: 'builtin', inputPerMTokUsd: 1, outputPerMTokUsd: 2 }],
    },
  ],
  aliases: [],
}

const rankingResponse: AdminUsageModelsResponse = {
  metric: 'cost',
  window: 'day',
  span: 7,
  offset: 0,
  detail: true,
  models: [{ id: 'openai/gpt-5', value: 1, requests: 5, tokensIn: 50, tokensOut: 60, costMicroUsd: 100 }],
}

const perfResponse: AdminPerfResponse = {
  kind: 'model',
  window: 'day',
  span: 7,
  offset: 0,
  latencyEnabled: true,
  rows: [{ id: 'openai/gpt-5', p50Ms: 100, p95Ms: 300, count: 5, attempts: 5, failures: 0 }],
}

const providerPerfResponse: AdminPerfResponse = {
  kind: 'provider',
  window: 'day',
  span: 7,
  offset: 0,
  latencyEnabled: true,
  rows: [{ id: 'openai', p50Ms: 90, p95Ms: 250, count: 20, attempts: 20, failures: 1, timeouts: 0, failovers: 2 }],
}

function mockPerformance(path: string): AdminPerfResponse {
  return path.includes('kind=provider') ? providerPerfResponse : perfResponse
}

describe('useModelsStore.fetch', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
  })

  it('ensures the catalog store is loaded, then joins ranking + performance onto it, plus provider performance', async () => {
    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path === '/admin/api/catalog') return catalogResponse
      if (path.startsWith('/admin/api/usage/models')) return rankingResponse
      if (path.startsWith('/admin/api/performance')) return mockPerformance(path)
      throw new Error(`unexpected path ${path}`)
    }) as typeof adminFetch)

    const models = useModelsStore()
    await models.fetch()

    expect(models.rows).toHaveLength(1)
    expect(models.rows[0]).toMatchObject({ id: 'openai/gpt-5', requests: 5, p50Ms: 100, attempts: 5 })
    expect(models.providerPerf).toEqual(providerPerfResponse.rows)
    expect(models.perfWindow).toEqual({ window: 'day', span: 7 })
    expect(models.latencyEnabled).toBe(true)
    expect(models.lastUpdated).not.toBeNull()
    expect(models.error).toBe('')
  })

  // P1 item 1: the ranking fetch used to omit `metric` entirely, which
  // admin.go's validHistoryMetric rejects as a 400 "unknown metric" — the
  // previous version of this spec matched adminFetch by path PREFIX only
  // (`path.startsWith('/admin/api/usage/models')`), which is why it never
  // caught a missing/wrong query parameter. This test asserts the exact
  // URL string.
  it('requests the ranking with metric=req (not omitted, not the Spend page\'s own metric)', async () => {
    let rankingUrl = ''
    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path === '/admin/api/catalog') return catalogResponse
      if (path.startsWith('/admin/api/usage/models')) {
        rankingUrl = path
        return rankingResponse
      }
      if (path.startsWith('/admin/api/performance')) return mockPerformance(path)
      throw new Error(`unexpected path ${path}`)
    }) as typeof adminFetch)

    const models = useModelsStore()
    await models.fetch()

    expect(rankingUrl).toBe('/admin/api/usage/models?window=day&span=7&limit=100&detail=1&metric=req')
  })

  it('reuses an already-loaded, still-fresh catalog store without re-fetching /admin/api/catalog', async () => {
    const catalog = useCatalogStore()
    catalog.data = catalogResponse
    catalog.fetchedAt = new Date()

    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path.startsWith('/admin/api/usage/models')) return rankingResponse
      if (path.startsWith('/admin/api/performance')) return perfResponse
      throw new Error(`unexpected path ${path}`)
    }) as typeof adminFetch)

    const models = useModelsStore()
    await models.fetch()

    expect(mockedAdminFetch).not.toHaveBeenCalledWith('/admin/api/catalog')
    expect(models.rows).toHaveLength(1)
  })

  it('records an error and leaves rows empty when the catalog itself fails to load', async () => {
    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path === '/admin/api/catalog') throw new Error('catalog boom')
      throw new Error(`unexpected path ${path}`)
    }) as typeof adminFetch)

    const models = useModelsStore()
    await models.fetch()

    expect(models.rows).toEqual([])
    expect(models.error).toContain('catalog boom')
  })

  // P2 item 6: fetch() used to have an `if (this.loading) return` guard,
  // which silently dropped a fetch() call started during a range change
  // mid-flight (a reader on Models who changes the range twice quickly
  // saw the SECOND range change simply do nothing). The replacement is
  // latest-request-wins (reqId), matching stores/catalog.ts's own
  // convention — this test exercises the actual property that matters: a
  // SLOWER, earlier fetch() resolving AFTER a faster, later one must
  // never overwrite the later (more current) result with stale data.
  it('discards a slower first fetch() result once a newer fetch() has already resolved (latest-request-wins)', async () => {
    const staleRanking: AdminUsageModelsResponse = {
      ...rankingResponse,
      models: [{ ...rankingResponse.models[0], id: 'openai/stale-model' }],
    }
    let resolveFirstRanking: ((v: AdminUsageModelsResponse) => void) | undefined
    let rankingCallCount = 0
    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path === '/admin/api/catalog') return catalogResponse
      if (path.startsWith('/admin/api/usage/models')) {
        rankingCallCount += 1
        // The FIRST fetch()'s own ranking call hangs until the test
        // resolves it by hand, below — the SECOND (and any later)
        // ranking call resolves immediately, so `second` finishes well
        // before `first` does.
        if (rankingCallCount === 1) return new Promise<AdminUsageModelsResponse>((resolve) => (resolveFirstRanking = resolve))
        return rankingResponse
      }
      if (path.startsWith('/admin/api/performance')) return mockPerformance(path)
      throw new Error(`unexpected path ${path}`)
    }) as typeof adminFetch)

    const models = useModelsStore()
    const first = models.fetch()
    await vi.waitFor(() => {
      if (!resolveFirstRanking) throw new Error('first ranking request not issued yet')
    })
    const second = models.fetch()
    await second
    expect(models.rows[0]?.id).toBe('openai/gpt-5')

    // `first` finally resolves, with STALE data — it must be discarded,
    // not overwrite the already-current `second` result.
    resolveFirstRanking?.(staleRanking)
    await first
    expect(models.rows[0]?.id).toBe('openai/gpt-5')
    expect(models.loading).toBe(false)
  })
})
