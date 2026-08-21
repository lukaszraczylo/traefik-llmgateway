import { defineStore } from 'pinia'

import { AdminApiError, adminFetch } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import type { AdminOverviewResponse, AdminUsageResponse } from '@/types/api'

/** How often the Providers/Usage views poll (matches the replaced vanilla-JS page). */
const POLL_MS = 5000

/**
 * useDashboardStore polls GET /admin/api/overview and GET /admin/api/usage
 * together every POLL_MS, feeding the Providers and Usage views (the store's
 * own `overview` field keeps the backend's name — see ProvidersView.vue's
 * doc comment for the UI-label/API-name split). A single
 * store (not two) because both routes are always fetched together — the
 * old vanilla-JS page's own Promise.all([overview, usage]) pattern.
 */
export const useDashboardStore = defineStore('dashboard', {
  state: () => ({
    overview: null as AdminOverviewResponse | null,
    usage: null as AdminUsageResponse | null,
    lastUpdated: null as Date | null,
    error: '',
    timer: undefined as ReturnType<typeof setInterval> | undefined,
  }),
  actions: {
    async refresh(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      try {
        const [overview, usage] = await Promise.all([
          adminFetch<AdminOverviewResponse>('/admin/api/overview'),
          adminFetch<AdminUsageResponse>('/admin/api/usage'),
        ])
        this.overview = overview
        this.usage = usage
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
