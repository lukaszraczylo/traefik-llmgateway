import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/lib/api', async () => {
  const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
  return { ...actual, adminFetch: vi.fn() }
})

import { adminFetch } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import { useFiltersStore } from '@/stores/filters'
import { useOverviewStore } from '@/stores/overview'

const mocked = vi.mocked(adminFetch)

function route(url: string): unknown {
  const u = new URL(url, 'http://x')
  const q = u.searchParams
  if (u.pathname.endsWith('/usage/series')) {
    const scopes = q.getAll('scope')
    const off = q.get('offset') ? 100 : 0
    return { metric: q.get('metric'), buckets: ['a', 'b'], series: scopes.map((scope) => ({ scope, points: [1 + off, 2 + off] })), unknown: [] }
  }
  if (u.pathname.endsWith('/usage/models')) return { models: [{ id: 'p/m1', value: 5 }, { id: 'p/m2', value: 3 }] }
  if (u.pathname.endsWith('/usage/totals')) return { rows: [{ id: 'alice', values: { req: 9 } }] }
  throw new Error(`unexpected ${url}`)
}

describe('useOverviewStore.refresh', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mocked.mockReset()
    mocked.mockImplementation(async (url: string) => route(url) as never)
    useAuthStore().submit('test-admin-key')
  })

  it('loads totals, top models and top users for the range', async () => {
    const s = useOverviewStore()
    await s.refresh()
    expect(s.totals.req).toEqual([1, 2])
    expect(s.topModelSeries.map((m) => m.scope)).toEqual(['model:p/m1', 'model:p/m2'])
    expect(s.topModelsByCost.map((m) => m.id)).toEqual(['p/m1', 'p/m2'])
    expect(s.topUserRows).toHaveLength(1)
    expect(s.previous).toBeNull()
    expect(s.error).toEqual({ traffic: '', models: '', users: '' })
    expect(s.lastUpdated).not.toBeNull()
  })

  it('fetches the previous period only when compare is requested', async () => {
    useFiltersStore().cmp = 'prev'
    const s = useOverviewStore()
    await s.refresh()
    expect(s.previous?.req).toEqual([101, 102])
    expect(mocked.mock.calls.some(([u]) => String(u).includes('offset='))).toBe(true)
  })

  it('keeps the last good data and records the error when a group fails', async () => {
    const s = useOverviewStore()
    await s.refresh()
    mocked.mockImplementation(async (url: string) => {
      if (url.includes('/usage/totals')) throw new Error('503')
      return route(url) as never
    })
    await s.refresh()
    expect(s.error.users).toContain('503')
    expect(s.topUserRows).toHaveLength(1)
    expect(s.totals.req).toEqual([1, 2])
  })

  it('drops the old selection when the range changes', async () => {
    const s = useOverviewStore()
    await s.refresh()
    mocked.mockImplementation(async () => {
      throw new Error('503')
    })
    useFiltersStore().range = '24h'
    await s.refresh()
    expect(s.totals.req).toEqual([])
    expect(s.error.traffic).toContain('503')
  })
})
