import { defineStore } from 'pinia'

import { AdminApiError, adminFetch } from '@/lib/api'
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
    timer: undefined as ReturnType<typeof setInterval> | undefined,
  }),
  actions: {
    async refresh(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return

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
    /** startPolling is idempotent and safe to call before a key is stored — refresh() no-ops until then. */
    startPolling(): void {
      if (this.timer !== undefined) return
      void this.refresh()
      this.timer = setInterval(() => void this.refresh(), POLL_MS)
    },
    stopPolling(): void {
      if (this.timer === undefined) return
      clearInterval(this.timer)
      this.timer = undefined
    },
  },
})
