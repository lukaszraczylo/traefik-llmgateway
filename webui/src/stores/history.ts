import { defineStore } from 'pinia'

import { AdminApiError, adminFetch } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import type {
  AdminUsageModelEntry,
  AdminUsageModelsResponse,
  HistoryMetric,
  HistoryWindow,
  UsageHistoryPoint,
  UsageHistoryResponse,
} from '@/types/api'

/**
 * The chart tabs the Charts view offers. The first three are metrics over
 * time (see metricsForTab below); "models" is the odd one out — a ranking
 * across models for a single point in time, with its own metric picked
 * separately (modelMetric below), because "which model" is the dimension
 * that tab varies rather than the metric.
 */
export type ChartTab = 'requests' | 'tokens' | 'cost' | 'models'

/**
 * The tabs that render a time series — every ChartTab except the ranking.
 * UsageChart is typed on this rather than on ChartTab so that adding
 * another ranking-style tab is a compile error there, not a chart with a
 * silently missing dataset spec.
 */
export type TimeSeriesTab = Exclude<ChartTab, 'models'>

/**
 * The metrics the Models ranking can rank by. Deliberately a single
 * metric, unlike the stacked "tokens" time-series tab: a bar's LENGTH in a
 * ranking is the comparison being made, so splitting it across two stacked
 * series would make two models with different in/out mixes visually
 * incomparable.
 */
export type ModelMetric = 'req' | 'tokin' | 'tokout' | 'cost'

/** MODEL_METRIC_LABEL is the ranking metric picker's display text. */
export const MODEL_METRIC_LABEL: Record<ModelMetric, string> = {
  cost: 'Cost',
  req: 'Requests',
  tokin: 'Tokens in',
  tokout: 'Tokens out',
}

/**
 * How many ranked models the Models tab requests. Matches the server's own
 * default (admin.go: usageModelsDefaultLimit) and stays well under its max.
 */
const MODEL_RANKING_LIMIT = 20

/**
 * How many models the SCOPE PICKER offers. Higher than the ranking's own
 * limit — the picker is a lookup, not a top-N — and capped at the server's
 * maximum (admin.go: usageModelsMaxLimit).
 */
const MODEL_OPTIONS_LIMIT = 100

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
    case 'models':
      // The Models tab reads the ranking endpoint, not the history one —
      // it needs no time series, so it asks for no metric here.
      return []
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
    /** Which metric the Models ranking ranks by — independent of `tab`. */
    modelMetric: 'cost' as ModelMetric,
    seriesByMetric: {} as Partial<Record<HistoryMetric, UsageHistoryPoint[]>>,
    /** The current Models-tab ranking. Empty means "nothing used in this window". */
    modelRanking: [] as AdminUsageModelEntry[],
    /** Models with non-zero traffic, for the scope picker's model entries. */
    modelOptions: [] as AdminUsageModelEntry[],
    loading: false,
    error: '',
    timer: undefined as ReturnType<typeof setInterval> | undefined,
  }),
  actions: {
    setScope(scope: string): void {
      if (scope === this.scope) return
      this.scope = scope
      void this.refresh()
    },
    setWindow(window: HistoryWindow): void {
      if (window === this.window) return
      this.window = window
      void this.refresh()
    },
    setTab(tab: ChartTab): void {
      if (tab === this.tab) return
      this.tab = tab
      void this.refresh()
    },
    setModelMetric(metric: ModelMetric): void {
      if (metric === this.modelMetric) return
      this.modelMetric = metric
      void this.refresh()
    },
    /**
     * refresh fetches whatever the CURRENT selection needs: the ranking on
     * the Models tab, a time series otherwise. The scope picker's model
     * list is refreshed alongside either, so a model that has just started
     * receiving traffic becomes selectable without a page reload.
     */
    async refresh(): Promise<void> {
      await Promise.all([this.tab === 'models' ? this.fetchModelRanking() : this.fetchSeries(), this.fetchModelOptions()])
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
    async fetchModelRanking(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      this.loading = true
      try {
        const res = await adminFetch<AdminUsageModelsResponse>(
          `/admin/api/usage/models?metric=${this.modelMetric}&window=${this.window}&limit=${MODEL_RANKING_LIMIT}`,
        )
        this.modelRanking = res.models
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
    /**
     * fetchModelOptions loads the picker's model list: models with at least
     * one REQUEST in the current window. Requests, not the ranking's own
     * metric, deliberately — a model can serve traffic while reporting no
     * tokens and costing nothing, and such a model must still be
     * selectable. A failure here is swallowed rather than surfaced: it
     * degrades the picker, and must not replace a rendered chart's own
     * error (or clear it) on a background refresh.
     */
    async fetchModelOptions(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      try {
        const res = await adminFetch<AdminUsageModelsResponse>(
          `/admin/api/usage/models?metric=req&window=${this.window}&limit=${MODEL_OPTIONS_LIMIT}`,
        )
        this.modelOptions = res.models
      } catch {
        // Intentionally ignored — see the doc comment above.
      }
    },
    startAutoRefresh(): void {
      if (this.timer !== undefined) return
      this.timer = setInterval(() => void this.refresh(), REFRESH_MS)
    },
    stopAutoRefresh(): void {
      if (this.timer === undefined) return
      clearInterval(this.timer)
      this.timer = undefined
    },
  },
})
