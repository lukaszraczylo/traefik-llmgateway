import { defineStore } from 'pinia'

import { adminFetch, isAuthRejection, messageOf } from '@/lib/api'
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
    /** Latest-request-wins guard: only the newest fetch may write state. */
    reqId: 0,
  }),
  actions: {
    async fetchConfig(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      const requestId = ++this.reqId
      this.loading = true
      try {
        const res = await adminFetch<AdminConfigResponse>('/admin/api/config')
        if (requestId !== this.reqId) return
        this.data = res
        this.error = ''
      } catch (err) {
        if (requestId !== this.reqId) return
        if (isAuthRejection(err)) return
        this.error = messageOf(err)
      } finally {
        if (requestId === this.reqId) this.loading = false
      }
    },
    /** ensureConfig fetches only when there is no data yet — ConfigPage.vue's own onMounted call. */
    async ensureConfig(): Promise<void> {
      if (this.loading || this.data) return
      await this.fetchConfig()
    },
  },
})
