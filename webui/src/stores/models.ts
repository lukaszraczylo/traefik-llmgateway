import { defineStore } from 'pinia'

import { adminFetch, isAuthRejection, messageOf } from '@/lib/api'
import { buildModelCatalogRows } from '@/lib/model-table-columns'
import type { ModelCatalogRow } from '@/lib/model-table-columns'
import { useCatalogStore } from '@/stores/catalog'
import { useFiltersStore } from '@/stores/filters'
import { hourOrDayWindow } from '@/stores/reliability'
import type { AdminPerfResponse, AdminPerfRow, AdminUsageModelsResponse, HistoryWindow } from '@/types/api'

/**
 * useModelsStore fetches the Models page's own catalog table (redesign-
 * plan.md section 3.3/3.4) — on demand, not part of the 5s dashboard poll
 * (Q12 DECISIONS), refetched on mount and whenever the global range
 * changes. Joins three independent responses via lib/model-table-columns.
 * ts's buildModelCatalogRows: the catalog (useCatalogStore, WP-D — shared
 * 5-minute-stale cache, so navigating here after Spend already paid this
 * read costs nothing extra), the current range's usage ranking
 * (`detail=1`, up to the server's own 100-row cap), and fleet performance
 * (`kind=model`, server default: top 50 by req in span).
 *
 * Ranking accepts any HistoryWindow (hour/day/month — redesign-plan.md
 * section 1.3.iv's metric x kind table, model scope), but performance does
 * not (PerfWindow is hour/day only, types/api.ts) — reuses
 * stores/reliability.ts's own hourOrDayWindow clamp for the performance
 * request only, leaving the ranking request on the global filters window
 * unclamped.
 */
export const useModelsStore = defineStore('models', {
  state: () => ({
    rows: [] as ModelCatalogRow[],
    /** GET /admin/api/performance?kind=provider's own rows (fleet p50/p95, attempts/failures, timeouts, failovers per provider) — ProviderHealthPanel.vue's own doc comment. Keyed by provider name at read time, not here, so this store stays a plain array like `rows` above. */
    providerPerf: [] as AdminPerfRow[],
    /** The hour/day window + span hourOrDayWindow actually resolved for both performance requests — ProviderHealthPanel.vue's badge titles read this rather than filters.window/span directly whenever a month-range clamp was applied. */
    perfWindow: { window: 'day' as Extract<HistoryWindow, 'hour' | 'day'>, span: 0 },
    /** GET /admin/api/performance's own latencyEnabled (AdminPerfResponse) — shared by BOTH the model and provider performance requests below (the same admin.stats.latency toggle gates both), surfaced so ModelCatalogTable.vue/ProviderHealthPanel.vue can hint at it rather than silently showing every row's p50/p95 as "—". */
    latencyEnabled: false,
    loading: false,
    lastUpdated: null as Date | null,
    error: '',
    /** reqId guards against an out-of-order response overwriting a newer one (latest-request-wins, stores/catalog.ts's own convention) — a range change mid-flight no longer gets silently dropped by an `if (this.loading) return` guard. */
    reqId: 0,
  }),
  actions: {
    async fetch(): Promise<void> {
      const requestId = ++this.reqId
      this.loading = true
      try {
        const catalog = useCatalogStore()
        const filters = useFiltersStore()
        await catalog.ensureLoaded()
        if (requestId !== this.reqId) return
        if (!catalog.data) {
          this.error = catalog.error || 'catalog unavailable'
          return
        }

        const perfWindow = hourOrDayWindow(filters.window, filters.span)
        const [rankingRes, perfRes, providerPerfRes] = await Promise.all([
          adminFetch<AdminUsageModelsResponse>(
            `/admin/api/usage/models?window=${filters.window}&span=${filters.span}&limit=100&detail=1&metric=req`,
          ),
          adminFetch<AdminPerfResponse>(`/admin/api/performance?kind=model&window=${perfWindow.window}&span=${perfWindow.span}`),
          adminFetch<AdminPerfResponse>(`/admin/api/performance?kind=provider&window=${perfWindow.window}&span=${perfWindow.span}`),
        ])
        if (requestId !== this.reqId) return

        this.rows = buildModelCatalogRows(catalog.data, rankingRes, perfRes)
        this.providerPerf = providerPerfRes.rows ?? []
        this.perfWindow = perfWindow
        this.latencyEnabled = perfRes.latencyEnabled
        this.lastUpdated = new Date()
        this.error = ''
      } catch (err) {
        if (requestId !== this.reqId) return
        if (isAuthRejection(err)) return
        this.error = messageOf(err)
      } finally {
        if (requestId === this.reqId) this.loading = false
      }
    },
  },
})
