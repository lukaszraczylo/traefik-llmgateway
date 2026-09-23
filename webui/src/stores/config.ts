import { defineStore } from 'pinia'

import { AdminApiError, adminFetch } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import type { AdminConfigResponse } from '@/types/api'

/**
 * useConfigStore backs the Config page (redesign-plan.md section 3.4): the
 * redacted config tree plus config warnings, from GET /admin/api/config —
 * fetched on demand (ConfigPage.vue's own onMounted), not polled: the
 * plugin's own config is immutable for a running replica's lifetime (a
 * config edit needs a reload/restart), so there is nothing to poll for.
 * ConfigPage.vue still exposes a manual refresh (in case the operator just
 * redeployed and wants the freshly-loaded config without a full page
 * reload).
 */
export const useConfigStore = defineStore('config', {
  state: () => ({
    data: null as AdminConfigResponse | null,
    loading: false,
    error: '',
  }),
  actions: {
    async fetchConfig(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      this.loading = true
      try {
        this.data = await adminFetch<AdminConfigResponse>('/admin/api/config')
        this.error = ''
      } catch (err) {
        if (err instanceof AdminApiError && (err.status === 401 || err.status === 403)) return
        this.error = err instanceof Error ? err.message : String(err)
      } finally {
        this.loading = false
      }
    },
    /** ensureConfig fetches only when there is no data yet — ConfigPage.vue's own onMounted call. */
    async ensureConfig(): Promise<void> {
      if (this.loading || this.data) return
      await this.fetchConfig()
    },
  },
})
