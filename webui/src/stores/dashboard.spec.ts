// dashboard.spec.ts covers useDashboardStore.refresh's per-section
// resilience (review round 2, v0.21 fix, SF7): GET /admin/api/overview,
// /usage, and /targets are fetched via Promise.allSettled and assigned
// independently — one endpoint failing must never blank the OTHER two,
// already-populated sections, and must never wipe an already-successful
// section's PREVIOUS value on a later failed poll.
//
// This is the first Pinia store under vitest in this package (vite.config.ts's
// own `test` block comment — "no snapshot/DOM/component testing this
// round" — predates this file). A Pinia store needs no DOM: it is plain
// reactive state and actions, well within the 'node' test environment
// already configured; adminFetch (lib/api.ts) is mocked entirely, so no
// real network or sessionStorage access ever happens.

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/lib/api', async () => {
  const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
  return { ...actual, adminFetch: vi.fn() }
})

import { adminFetch, AdminApiError } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import type { AdminOverviewResponse, AdminTargetsResponse, AdminUsageResponse } from '@/types/api'

const mockedAdminFetch = vi.mocked(adminFetch)

const emptyOverview: AdminOverviewResponse = {
  version: '1',
  providers: [],
  groups: [],
  aliases: [],
  redis: { configured: false, lastErrAt: '0001-01-01T00:00:00Z' },
  cache: { enabled: false },
  retry: { enabled: false },
}
const emptyUsage: AdminUsageResponse = {
  users: [],
  groups: [],
  total: {
    kind: 'total',
    id: 'all',
    requestsPerMinute: 0,
    requestsPerDay: 0,
    tokensInPerDay: 0,
    tokensOutPerDay: 0,
    tokensInPerMonth: 0,
    tokensOutPerMonth: 0,
    costPerDayMicroUsd: 0,
    costPerMonthMicroUsd: 0,
  },
}
const emptyTargets: AdminTargetsResponse = { mcpServers: [], agents: [] }

/** mockRoutes wires adminFetch's mock to answer each of the three routes independently — a route with no override answers its own empty fixture; an override that throws simulates that one route failing. */
function mockRoutes(routes: {
  overview?: () => AdminOverviewResponse
  usage?: () => AdminUsageResponse
  targets?: () => AdminTargetsResponse
}): void {
  mockedAdminFetch.mockImplementation((async (path: string) => {
    if (path === '/admin/api/overview') return routes.overview ? routes.overview() : emptyOverview
    if (path === '/admin/api/usage') return routes.usage ? routes.usage() : emptyUsage
    if (path === '/admin/api/targets') return routes.targets ? routes.targets() : emptyTargets
    throw new Error(`unexpected path ${path}`)
  }) as typeof adminFetch)
}

describe('useDashboardStore.refresh', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
  })

  it('assigns all three sections and clears error on full success', async () => {
    mockRoutes({})
    const dashboard = useDashboardStore()
    await dashboard.refresh()

    expect(dashboard.overview).not.toBeNull()
    expect(dashboard.usage).not.toBeNull()
    expect(dashboard.targets).not.toBeNull()
    expect(dashboard.error).toBe('')
    expect(dashboard.lastUpdated).not.toBeNull()
  })

  it('keeps overview and usage populated when targets alone fails', async () => {
    mockRoutes({
      targets: () => {
        throw new Error('targets boom')
      },
    })
    const dashboard = useDashboardStore()
    await dashboard.refresh()

    expect(dashboard.overview).not.toBeNull()
    expect(dashboard.usage).not.toBeNull()
    expect(dashboard.targets).toBeNull() // never successfully fetched yet — nothing to keep
    expect(dashboard.error).toContain('targets')
    expect(dashboard.lastUpdated).not.toBeNull() // a partial success still advances it
  })

  it('preserves a previously-successful section instead of blanking it on a later failure', async () => {
    const dashboard = useDashboardStore()

    mockRoutes({})
    await dashboard.refresh()
    const seededTargets = dashboard.targets
    expect(seededTargets).not.toBeNull()

    mockRoutes({
      targets: () => {
        throw new Error('targets boom again')
      },
    })
    await dashboard.refresh()

    expect(dashboard.targets).toBe(seededTargets) // same object — never reassigned to null
    expect(dashboard.overview).not.toBeNull()
    expect(dashboard.usage).not.toBeNull()
  })

  it('does not surface an error when a section rejects with 401 (the auth store owns that case)', async () => {
    mockRoutes({
      targets: () => {
        throw new AdminApiError('unauthorized', 401)
      },
    })
    const dashboard = useDashboardStore()
    await dashboard.refresh()

    expect(dashboard.error).toBe('')
  })
})
