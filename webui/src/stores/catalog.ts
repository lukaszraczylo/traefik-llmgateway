import { defineStore } from 'pinia'

import { AdminApiError, adminFetch } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import type { AdminCatalogModel, AdminCatalogResponse } from '@/types/api'

/**
 * How long a fetched catalog stays fresh before ensureLoaded() will fetch
 * again (redesign-plan.md section 3.3: "catalog (/catalog on demand,
 * 5-minute stale)"). GET /admin/api/catalog reads the registry snapshot —
 * no live counter round trip (admin_catalog.go's own doc comment) — so it
 * changes only on a discovery refresh or a config reload, far slower than
 * the 5s overview poll; every page that needs it (Spend's pricing health
 * and cost-avoided reference picker, WP-F's Models catalog table) can
 * treat a 5-minute-old copy as current rather than re-fetching on every
 * mount.
 */
const STALE_MS = 5 * 60 * 1000

/**
 * useCatalogStore fetches GET /admin/api/catalog on demand (never part of
 * the 5s dashboard poll, Q12/DECISIONS) and indexes it by canonical
 * "provider/model" id (admin_catalog.go's providerModelScopeID — the SAME
 * id AdminUsageModelEntry.id and AdminSeriesResponse's `model:{canonical}`
 * scope already use, so every consumer joins against it with a plain map
 * lookup, no id reformatting).
 *
 * Lands FIRST in WP-D (redesign-plan.md section 4): WP-F's Models page
 * (ModelCatalogTable.vue, ProviderHealthPanel.vue) depends on this store
 * existing before it can build against it.
 */
export const useCatalogStore = defineStore('catalog', {
  state: () => ({
    data: null as AdminCatalogResponse | null,
    fetchedAt: null as Date | null,
    loading: false,
    error: '',
    /** reqId guards against an out-of-order response overwriting a newer one — the same latest-request-wins convention every other store's own fetch action follows (stores/dashboard.ts, stores/spend.ts). */
    reqId: 0,
  }),
  getters: {
    /** stale is true before the first successful fetch, or once STALE_MS has elapsed since it — ensureLoaded's own refetch condition, also readable by a caller that wants to show "catalog data may be out of date" copy. */
    stale(state): boolean {
      if (!state.fetchedAt) return true
      return Date.now() - state.fetchedAt.getTime() > STALE_MS
    },
    /**
     * modelsById indexes every provider's models by canonical id — the one
     * join key every consumer (PricingHealthTable, CostAvoidedCard's
     * reference-model picker, WP-F's ModelCatalogTable) needs, computed
     * once per fetch (a Pinia getter is cached per state reference) rather
     * than every consumer re-scanning `data.providers` itself.
     */
    modelsById(state): Map<string, AdminCatalogModel> {
      const map = new Map<string, AdminCatalogModel>()
      for (const provider of state.data?.providers ?? []) {
        for (const model of provider.models) map.set(model.id, model)
      }
      return map
    },
  },
  actions: {
    /**
     * ensureLoaded fetches only when the catalog has never loaded, or has
     * gone stale (STALE_MS) — every page that reads catalog data calls
     * this from its own onMounted rather than unconditionally fetching, so
     * navigating between Spend/Models repeatedly does not re-pay this read
     * every time. `force` bypasses the staleness check (a reader-initiated
     * "refresh catalog" action, if one is ever added).
     */
    async ensureLoaded(force = false): Promise<void> {
      if (!force && this.data && !this.stale) return
      await this.fetch()
    },
    async fetch(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      const requestId = ++this.reqId
      this.loading = true
      try {
        const res = await adminFetch<AdminCatalogResponse>('/admin/api/catalog')
        // Stale: a newer fetch() call already superseded this one.
        if (requestId !== this.reqId) return
        this.data = res
        this.fetchedAt = new Date()
        this.error = ''
      } catch (err) {
        if (requestId !== this.reqId) return
        if (err instanceof AdminApiError && (err.status === 401 || err.status === 403)) {
          return
        }
        this.error = err instanceof Error ? err.message : String(err)
      } finally {
        if (requestId === this.reqId) this.loading = false
      }
    },
  },
})
