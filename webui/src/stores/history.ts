import { defineStore } from 'pinia'

import { AdminApiError, adminFetch } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import type { HistoryMetric, HistoryWindow, UsageHistoryPoint, UsageHistoryResponse } from '@/types/api'

/** The three chart tabs the Charts view offers — see metricsForTab below. */
export type ChartTab = 'requests' | 'tokens' | 'cost'

/**
 * How often the current selection refetches while the Charts view is open.
 * Slower than the dashboard's 5s poll (dashboard.ts) — history buckets
 * change on the timescale of the window itself, not every request.
 */
const REFRESH_MS = 30_000

/**
 * WINDOW_SPAN is the bucket count requested per window: 24h at hour
 * resolution, 30d at day resolution, 12mo at month resolution. Each stays
 * within historyMaxSpan's own per-window ceiling (admin.go: 48/35/13), so
 * every request is valid without the UI needing to know that ceiling.
 */
export const WINDOW_SPAN: Record<HistoryWindow, number> = {
  hour: 24,
  day: 30,
  month: 12,
}

/** WINDOW_LABEL is the switcher's own display text for each window. */
export const WINDOW_LABEL: Record<HistoryWindow, string> = {
  hour: '24h',
  day: '30d',
  month: '12mo',
}

/**
 * metricsForTab maps a chart tab to the metric(s) it needs. "tokens" is the
 * one tab needing two: GET /admin/api/usage/history takes a single metric
 * per call (tokin XOR tokout), and the Charts view stacks both into one bar
 * chart (operator brief: "STACKED tokens-in vs tokens-out").
 */
export function metricsForTab(tab: ChartTab): HistoryMetric[] {
  switch (tab) {
    case 'requests':
      return ['req']
    case 'tokens':
      return ['tokin', 'tokout']
    case 'cost':
      return ['cost']
  }
}

/**
 * useHistoryStore drives the Charts view: current scope/window/tab
 * selection, the fetched series for whatever metric(s) that selection
 * needs, and a 30s auto-refresh of the current selection. Changing scope,
 * window, or tab always triggers an immediate refetch (operator brief:
 * "history fetch on selection + 30s refresh").
 */
export const useHistoryStore = defineStore('history', {
  state: () => ({
    /** "total" | "user:{id}" | "group:{id}" — the raw scope query value serveAdminUsageHistory expects. */
    scope: 'total',
    window: 'hour' as HistoryWindow,
    tab: 'requests' as ChartTab,
    seriesByMetric: {} as Partial<Record<HistoryMetric, UsageHistoryPoint[]>>,
    loading: false,
    error: '',
    timer: undefined as ReturnType<typeof setInterval> | undefined,
  }),
  actions: {
    setScope(scope: string): void {
      if (scope === this.scope) return
      this.scope = scope
      void this.fetchSeries()
    },
    setWindow(window: HistoryWindow): void {
      if (window === this.window) return
      this.window = window
      void this.fetchSeries()
    },
    setTab(tab: ChartTab): void {
      if (tab === this.tab) return
      this.tab = tab
      void this.fetchSeries()
    },
    async fetchSeries(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      this.loading = true
      try {
        const span = WINDOW_SPAN[this.window]
        const metrics = metricsForTab(this.tab)
        const results = await Promise.all(
          metrics.map((metric) =>
            adminFetch<UsageHistoryResponse>(
              `/admin/api/usage/history?scope=${encodeURIComponent(this.scope)}&metric=${metric}&window=${this.window}&span=${span}`,
            ),
          ),
        )
        const next: Partial<Record<HistoryMetric, UsageHistoryPoint[]>> = {}
        metrics.forEach((metric, i) => {
          next[metric] = results[i]!.points
        })
        this.seriesByMetric = next
        this.error = ''
      } catch (err) {
        if (err instanceof AdminApiError && (err.status === 401 || err.status === 403)) {
          return
        }
        this.error = err instanceof Error ? err.message : String(err)
      } finally {
        this.loading = false
      }
    },
    startAutoRefresh(): void {
      if (this.timer !== undefined) return
      this.timer = setInterval(() => void this.fetchSeries(), REFRESH_MS)
    },
    stopAutoRefresh(): void {
      if (this.timer === undefined) return
      clearInterval(this.timer)
      this.timer = undefined
    },
  },
})
