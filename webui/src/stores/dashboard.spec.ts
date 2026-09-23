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
  replica: 'pod-a',
  instance: 'test-gateway',
  warnings: [],
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
    rejectionsPerDay: 0,
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

  // Review finding: the 5s poll interval had no in-flight guard, so a round
  // trip slower than POLL_MS could stack a second refresh() on top of the
  // first — twice the requests, free to resolve in either order.
  it('skips a refresh() call that starts while one is already in flight', async () => {
    let resolveOverview!: (v: AdminOverviewResponse) => void
    mockedAdminFetch.mockImplementation((async (path: string) => {
      if (path === '/admin/api/overview') return new Promise<AdminOverviewResponse>((resolve) => (resolveOverview = resolve))
      if (path === '/admin/api/usage') return emptyUsage
      if (path === '/admin/api/targets') return emptyTargets
      throw new Error(`unexpected path ${path}`)
    }) as typeof adminFetch)
    const dashboard = useDashboardStore()

    const first = dashboard.refresh() // overview never resolves yet — still "in flight"
    const second = dashboard.refresh() // must skip entirely, not queue behind the first

    resolveOverview(emptyOverview)
    await Promise.all([first, second])

    // One overview call for the first refresh(), none for the skipped
    // second one — not two.
    const overviewCalls = mockedAdminFetch.mock.calls.filter((c) => c[0] === '/admin/api/overview')
    expect(overviewCalls).toHaveLength(1)
  })
})

// F5 (dashboard-plan.md): startPolling now drives refresh() through the
// shared createVisibilityPoller (lib/polling.ts) instead of a bare
// setInterval — same observable "fetch once, then poll" shape as before,
// just built on the tested shared primitive.
describe('useDashboardStore.startPolling', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
    mockRoutes({})
  })

  it('fetches immediately on startPolling, then again every POLL_MS', async () => {
    vi.useFakeTimers()
    try {
      const dashboard = useDashboardStore()

      dashboard.startPolling()
      await vi.advanceTimersByTimeAsync(0)
      expect(dashboard.lastUpdated).not.toBeNull()

      const callsAfterFirst = mockedAdminFetch.mock.calls.length
      await vi.advanceTimersByTimeAsync(5000) // POLL_MS
      expect(mockedAdminFetch.mock.calls.length).toBeGreaterThan(callsAfterFirst)
    } finally {
      vi.useRealTimers()
    }
  })

  it('startPolling is idempotent — a second call does not double the interval', async () => {
    vi.useFakeTimers()
    try {
      const dashboard = useDashboardStore()

      dashboard.startPolling()
      dashboard.startPolling()
      await vi.advanceTimersByTimeAsync(0)
      const callsAfterFirst = mockedAdminFetch.mock.calls.length

      await vi.advanceTimersByTimeAsync(5000)
      // One additional round of three requests (overview/usage/targets),
      // not two.
      expect(mockedAdminFetch.mock.calls.length - callsAfterFirst).toBe(3)
    } finally {
      vi.useRealTimers()
    }
  })
  // P11 review fix: a `stopPolling` test used to live here too — the
  // action itself was dead application-facing API surface (nothing outside
  // this spec ever called it, App.vue polls this store for the app's whole
  // session) and has been removed from stores/dashboard.ts; see that
  // file's own comment.
})
