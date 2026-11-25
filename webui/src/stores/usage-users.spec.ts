import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/lib/api', async () => {
  const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
  return { ...actual, adminFetch: vi.fn() }
})

import { AdminApiError, adminFetch } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { USERS_RANGE_TOO_LARGE, useUsageUsersStore } from '@/stores/usage-users'

const mocked = vi.mocked(adminFetch)

function route(url: string): unknown {
  const u = new URL(url, 'http://x')
  if (u.pathname.endsWith('/usage/totals')) {
    return { rows: [{ id: 'bob', values: { cost: 3 } }, { id: 'alice', values: { cost: 9 } }, { id: 'idle', values: { cost: 0 } }] }
  }
  const scopes = u.searchParams.getAll('scope')
  return { series: scopes.map((scope) => ({ scope, points: scope === 'total' ? [20, 20] : [4, 4] })) }
}

describe('useUsageUsersStore.refresh', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mocked.mockReset()
    mocked.mockImplementation(async (url: string) => route(url) as never)
    useAuthStore().submit('test-admin-key')
  })

  it('ranks active users by the metric and derives the other bucket from the total', async () => {
    const s = useUsageUsersStore()
    await s.refresh('cost')
    expect(s.rows).toEqual([{ id: 'alice', value: 9 }, { id: 'bob', value: 3 }])
    expect(s.series.map((x) => x.scope)).toEqual(['user:alice', 'user:bob'])
    expect(s.otherPoints).toEqual([12, 12])
    expect(s.loaded).toBe(true)
    expect(s.error).toBe('')
  })

  it('refuses ranges whose user count would exceed the per-request key cap', async () => {
    const dashboard = useDashboardStore()
    dashboard.usage = { users: new Array(20000).fill({ id: 'u' }) } as never
    const s = useUsageUsersStore()
    await s.refresh('cost')
    expect(s.error).toBe(USERS_RANGE_TOO_LARGE)
    expect(mocked).not.toHaveBeenCalled()
  })

  it('reports a fetch failure and stays unloaded', async () => {
    mocked.mockRejectedValue(new AdminApiError('boom', 503))
    const s = useUsageUsersStore()
    await s.refresh('cost')
    expect(s.error).toBe('boom')
    expect(s.loaded).toBe(false)
  })
})
