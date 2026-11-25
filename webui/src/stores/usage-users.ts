import { defineStore } from 'pinia'

import { adminFetch, isAuthRejection, messageOf } from '@/lib/api'
import { seriesUrl } from '@/lib/range'
import { other } from '@/lib/series'
import type { Series } from '@/lib/series'
import { topUsersCapCheckResult } from '@/lib/top-users'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { useFiltersStore } from '@/stores/filters'
import type { AdminSeriesResponse, AdminTotalsResponse, HistoryMetric } from '@/types/api'

/** Users given their own chart series; the rest fold into "Other". */
export const USER_SERIES_LIMIT = 10
const TOTALS_LIMIT = 1000
const TOTAL_SCOPE = 'total'

export const USERS_RANGE_TOO_LARGE = 'Too many configured users to rank for this range. Pick a shorter range.'

export interface UserRow {
  id: string
  value: number
}

/** Per-user ranking and top-user series for the Usage page's user breakdown. */
export const useUsageUsersStore = defineStore('usage-users', {
  state: () => ({
    rows: [] as UserRow[],
    series: [] as Series[],
    otherPoints: null as number[] | null,
    loaded: false,
    error: '',
    fetchedFor: '',
    reqId: 0,
  }),
  actions: {
    async refresh(metric: HistoryMetric): Promise<void> {
      if (!useAuthStore().isAuthenticated) return
      const { window, span } = useFiltersStore()
      const key = `${metric}:${window}:${span}`
      if (key !== this.fetchedFor) {
        this.rows = []
        this.series = []
        this.otherPoints = null
        this.loaded = false
      }
      this.fetchedFor = key
      const id = ++this.reqId
      const configured = useDashboardStore().usage?.users.length ?? null
      if (topUsersCapCheckResult(configured, 1, span)) {
        this.error = USERS_RANGE_TOO_LARGE
        return
      }
      try {
        const q = new URLSearchParams({ kind: 'user', window, span: String(span), offset: '0', metrics: metric, limit: String(TOTALS_LIMIT) })
        const totals = await adminFetch<AdminTotalsResponse>(`/admin/api/usage/totals?${q.toString()}`)
        const rows = totals.rows.map((r) => ({ id: r.id, value: r.values[metric] ?? 0 })).filter((r) => r.value > 0).sort((a, b) => b.value - a.value)
        const top = rows.slice(0, USER_SERIES_LIMIT)
        const scopes = top.map((r) => `user:${r.id}`)
        let series: Series[] = []
        let totalPoints: number[] = []
        if (scopes.length > 0) {
          const res = await adminFetch<AdminSeriesResponse>(seriesUrl({ scope: [TOTAL_SCOPE, ...scopes], metric, window, span }))
          series = res.series.filter((s) => s.scope !== TOTAL_SCOPE)
          totalPoints = res.series.find((s) => s.scope === TOTAL_SCOPE)?.points ?? []
        }
        if (id !== this.reqId) return
        this.rows = rows
        this.series = series
        this.otherPoints = totalPoints.length > 0 ? other(totalPoints, series) : null
        this.loaded = true
        this.error = ''
      } catch (err) {
        if (id === this.reqId && !isAuthRejection(err)) this.error = messageOf(err)
      }
    },
  },
})
