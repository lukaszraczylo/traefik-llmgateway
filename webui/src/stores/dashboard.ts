import { defineStore } from 'pinia'

import { AdminApiError, adminFetch } from '@/lib/api'
import { createVisibilityPoller, type VisibilityPoller } from '@/lib/polling'
import { useAuthStore } from '@/stores/auth'
import type { AdminOverviewResponse, AdminTargetsResponse, AdminUsageResponse } from '@/types/api'

/** How often the Providers/Usage/Targets views poll (matches the replaced vanilla-JS page). */
const POLL_MS = 5000

/**
 * useDashboardStore polls GET /admin/api/overview, GET /admin/api/usage,
 * and GET /admin/api/targets together every POLL_MS, feeding the
 * Providers, Usage, and MCP & Agents views (the store's own `overview`
 * field keeps the backend's name — see ProvidersView.vue's doc comment
 * for the UI-label/API-name split). A single store (not three) because
 * every route is always fetched together — the old vanilla-JS page's own
 * Promise.all([overview, usage]) pattern, extended to targets (Feature B,
 * v0.21) rather than given its own separate poll timer.
 */
export const useDashboardStore = defineStore('dashboard', {
  state: () => ({
    overview: null as AdminOverviewResponse | null,
    usage: null as AdminUsageResponse | null,
    targets: null as AdminTargetsResponse | null,
    lastUpdated: null as Date | null,
    error: '',
    poller: undefined as VisibilityPoller | undefined,
    /**
     * refreshing guards against overlapping polls (review finding): the 5s
     * setInterval has no in-flight check on its own, so a round trip that
     * takes longer than POLL_MS (a slow Redis behind /admin/api/overview,
     * say) would otherwise stack a second refresh() on top of the first —
     * two requests per section in flight at once, free to resolve in
     * either order and briefly show older data after the newer one already
     * landed. A tick that finds a refresh still in flight simply skips —
     * the next tick (or the already-in-flight one finishing) catches up,
     * so the dashboard never resorts to comparing response ordering.
     */
    refreshing: false,
  }),
  actions: {
    async refresh(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      if (this.refreshing) return
      this.refreshing = true
      try {
        await this.doRefresh()
      } finally {
        this.refreshing = false
      }
    },
    /** doRefresh is refresh()'s actual body, split out so the in-flight guard above wraps it in one place rather than every early-return branch below needing its own `finally`. */
    async doRefresh(): Promise<void> {
      const [overviewResult, usageResult, targetsResult] = await Promise.allSettled([
        adminFetch<AdminOverviewResponse>('/admin/api/overview'),
        adminFetch<AdminUsageResponse>('/admin/api/usage'),
        adminFetch<AdminTargetsResponse>('/admin/api/targets'),
      ])

      // Each section is assigned independently (review round 2, v0.21
      // fix): one endpoint failing (e.g. GET /admin/api/targets) must
      // never blank the OTHER two, already-populated sections. The
      // earlier Promise.all threw on the FIRST rejection and updated
      // nothing at all — a single flaky route degraded every view in the
      // dashboard, not just its own.
      if (overviewResult.status === 'fulfilled') this.overview = overviewResult.value
      if (usageResult.status === 'fulfilled') this.usage = usageResult.value
      if (targetsResult.status === 'fulfilled') this.targets = targetsResult.value

      const failures: { label: string; reason: unknown }[] = []
      if (overviewResult.status === 'rejected') failures.push({ label: 'overview', reason: overviewResult.reason })
      if (usageResult.status === 'rejected') failures.push({ label: 'usage', reason: usageResult.reason })
      if (targetsResult.status === 'rejected') failures.push({ label: 'targets', reason: targetsResult.reason })

      if (failures.length === 0) {
        this.lastUpdated = new Date()
        this.error = ''
        return
      }

      // A 401/403 on ANY section already rejected the key (lib/api.ts) —
      // the AuthGate takes over the whole view in that case, matching the
      // pre-existing single-Promise.all behavior; nothing left to report.
      const authRejected = failures.some(
        ({ reason }) => reason instanceof AdminApiError && (reason.status === 401 || reason.status === 403),
      )
      if (authRejected) return

      // A partial failure still advances lastUpdated: at least one
      // section genuinely has fresh data, even though `error` (below)
      // still surfaces that something is degraded.
      if (failures.length < 3) this.lastUpdated = new Date()
      this.error = failures
        .map(({ label, reason }) => `${label}: ${reason instanceof Error ? reason.message : String(reason)}`)
        .join('; ')
    },
    /**
     * startPolling is idempotent and safe to call before a key is stored —
     * refresh() no-ops until then. Driven by the shared
     * createVisibilityPoller (lib/polling.ts, F5) rather than a bare
     * setInterval: an immediate tick (the same "fetch once, then poll"
     * shape this action already had), then every POLL_MS while the tab is
     * visible, pausing while it is hidden and catching up the moment it is
     * looked at again.
     */
    startPolling(): void {
      if (this.poller) return
      this.poller = createVisibilityPoller({ intervalMs: POLL_MS, tick: () => void this.refresh() })
      this.poller.start()
    },
    // P11 review fix: a stopPolling action mirroring history.ts/events.ts's
    // own stop() used to live here too, but nothing outside its OWN spec
    // ever called it — App.vue mounts this store's poller once, for the
    // app's whole session, and never unmounts it (unlike history.ts/
    // events.ts, whose stores are torn down when ChartsView/EventsView
    // unmount). Dead application-facing API surface, removed; `poller`
    // itself stays (startPolling's own idempotency guard still needs it).
  },
})
