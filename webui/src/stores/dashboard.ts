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
      try {
        const [overview, usage, targets] = await Promise.all([
          adminFetch<AdminOverviewResponse>('/admin/api/overview'),
          adminFetch<AdminUsageResponse>('/admin/api/usage'),
          adminFetch<AdminTargetsResponse>('/admin/api/targets'),
        ])
        this.overview = overview
        this.usage = usage
        this.targets = targets
        this.lastUpdated = new Date()
        this.error = ''
      } catch (err) {
        // A 401/403 already rejected the key (lib/api.ts) — the AuthGate
        // takes over the view, nothing left to report here.
        if (err instanceof AdminApiError && (err.status === 401 || err.status === 403)) {
          return
        }
        this.error = err instanceof Error ? err.message : String(err)
      }
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
