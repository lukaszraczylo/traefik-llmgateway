import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/lib/api', async () => {
  const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
  return { ...actual, adminFetch: vi.fn() }
})

import { adminFetch, AdminApiError } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import { useCatalogStore } from '@/stores/catalog'
import type { AdminCatalogResponse } from '@/types/api'

const mockedAdminFetch = vi.mocked(adminFetch)

const catalogFixture: AdminCatalogResponse = {
  providers: [
    {
      name: 'openai',
      type: 'openai',
      lastRefresh: '2026-09-23T00:00:00Z',
      openUntil: '0001-01-01T00:00:00Z',
      healthState: 'closed',
      discoveryEnabled: true,
      models: [
        { id: 'openai/gpt-5', model: 'gpt-5', priceSource: 'builtin', inputPerMTokUsd: 1.25, outputPerMTokUsd: 10 },
        { id: 'openai/gpt-5-mini:free', model: 'gpt-5-mini:free', priceSource: 'unpriced', displayFree: true },
      ],
    },
  ],
  aliases: [],
}

describe('useCatalogStore.fetch', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
  })

  it('is a no-op while unauthenticated', async () => {
    setActivePinia(createPinia())
    const catalog = useCatalogStore()
    await catalog.fetch()
    expect(mockedAdminFetch).not.toHaveBeenCalled()
    expect(catalog.data).toBeNull()
  })

  it('populates data and fetchedAt on success, clearing any prior error', async () => {
    mockedAdminFetch.mockResolvedValue(catalogFixture)
    const catalog = useCatalogStore()
    await catalog.fetch()

    expect(catalog.data).toEqual(catalogFixture)
    expect(catalog.fetchedAt).not.toBeNull()
    expect(catalog.error).toBe('')
    expect(catalog.loading).toBe(false)
  })

  it('indexes modelsById by canonical id across every provider', async () => {
    mockedAdminFetch.mockResolvedValue(catalogFixture)
    const catalog = useCatalogStore()
    await catalog.fetch()

    expect(catalog.modelsById.get('openai/gpt-5')?.priceSource).toBe('builtin')
    expect(catalog.modelsById.get('openai/gpt-5-mini:free')?.displayFree).toBe(true)
    expect(catalog.modelsById.size).toBe(2)
  })

  it('records a plain error message on a non-auth failure', async () => {
    mockedAdminFetch.mockRejectedValue(new Error('boom'))
    const catalog = useCatalogStore()
    await catalog.fetch()

    expect(catalog.error).toBe('boom')
    expect(catalog.data).toBeNull()
  })

  it('does not surface an error for a 401/403 (the auth store owns that case)', async () => {
    mockedAdminFetch.mockRejectedValue(new AdminApiError('unauthorized', 401))
    const catalog = useCatalogStore()
    await catalog.fetch()

    expect(catalog.error).toBe('')
  })

  it('latest-request-wins: an older in-flight fetch never overwrites a newer one', async () => {
    let resolveFirst!: (v: AdminCatalogResponse) => void
    mockedAdminFetch.mockImplementationOnce(
      () => new Promise<AdminCatalogResponse>((resolve) => (resolveFirst = resolve)),
    )
    const catalog = useCatalogStore()
    const first = catalog.fetch()

    mockedAdminFetch.mockResolvedValueOnce(catalogFixture)
    await catalog.fetch()
    expect(catalog.data).toEqual(catalogFixture)

    resolveFirst({ providers: [], aliases: [] })
    await first
    // The first (older) fetch's empty result must not clobber the second's.
    expect(catalog.data).toEqual(catalogFixture)
  })
})

describe('useCatalogStore.ensureLoaded', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    mockedAdminFetch.mockResolvedValue(catalogFixture)
    useAuthStore().submit('test-admin-key')
  })

  it('fetches when never loaded', async () => {
    const catalog = useCatalogStore()
    await catalog.ensureLoaded()
    expect(mockedAdminFetch).toHaveBeenCalledTimes(1)
  })

  it('does not refetch a fresh catalog', async () => {
    const catalog = useCatalogStore()
    await catalog.ensureLoaded()
    await catalog.ensureLoaded()
    expect(mockedAdminFetch).toHaveBeenCalledTimes(1)
  })

  it('refetches once stale', async () => {
    vi.useFakeTimers()
    try {
      const catalog = useCatalogStore()
      await catalog.ensureLoaded()
      vi.advanceTimersByTime(5 * 60 * 1000 + 1)
      await catalog.ensureLoaded()
      expect(mockedAdminFetch).toHaveBeenCalledTimes(2)
    } finally {
      vi.useRealTimers()
    }
  })

  it('force bypasses the staleness check', async () => {
    const catalog = useCatalogStore()
    await catalog.ensureLoaded()
    await catalog.ensureLoaded(true)
    expect(mockedAdminFetch).toHaveBeenCalledTimes(2)
  })
})
