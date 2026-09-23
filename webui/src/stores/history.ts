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

/** WINDOW_LABEL is the switcher's own display text for each window, for the three TIME-SERIES tabs (requests/tokens/cost) — a real span of buckets, matching WINDOW_SPAN above. */
export const WINDOW_LABEL: Record<HistoryWindow, string> = {
  hour: '24h',
  day: '30d',
  month: '12mo',
}

/**
 * MODEL_WINDOW_LABEL is the Models tab's OWN window-switcher text — that
 * tab shares the same three-way Tabs control as the time-series tabs
 * (ChartsView.vue), but reads a fundamentally different query: modelTotals
 * (limits.go) returns ONE counter at window's CURRENT bucket, never a span
 * of WINDOW_SPAN buckets. Labeling that control "24h" when it actually
 * means "since the top of the current UTC hour" understates how little
 * data is behind it — at 14:05 UTC, "24h" would visually promise a full
 * day while the ranking covers five minutes. "(UTC)" is explicit rather
 * than implied, matching formatBucketLabel's own hour-bucket fix
 * (lib/format.ts) — bucketFor (limits.go) always buckets in UTC.
 */
export const MODEL_WINDOW_LABEL: Record<HistoryWindow, string> = {
  hour: 'This hour (UTC)',
  day: 'Today (UTC)',
  month: 'This month (UTC)',
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
    /** True once the current selection's first fetch committed; cleared by the setters. Drives the "Loading…" line so background refreshes don't flash it over rendered data. */
    loaded: false,
    error: '',
    timer: undefined as ReturnType<typeof setInterval> | undefined,
    /**
     * seriesReqId guards fetchSeries and fetchModelRanking against
     * out-of-order responses (review finding: "latest-request-wins"). The
     * two are mutually exclusive per refresh() call (exactly one runs,
     * picked by `tab`), so ONE shared counter is enough — a fetch started
     * by an OLDER selection (scope/window/tab/modelMetric) always loses to
     * whichever fetch started most recently, never to load order. Each
     * fetch captures the id it was issued under before its own await, then
     * only commits its result (and clears `loading`) if that id is still
     * the current one when it resolves.
     */
    seriesReqId: 0,
    /** optionsReqId is fetchModelOptions' own independent counter (a background, parallel fetch — see that action's own doc comment for why it never touches `loading`/`error`). */
    optionsReqId: 0,
  }),
  actions: {
    setScope(scope: string): void {
      if (scope === this.scope) return
      this.scope = scope
      // Clear the OLD selection's series before fetching the new one (review
      // finding: stale data must never render under the new selection's
      // labels) — done in the setter, not inside refresh()/fetchSeries(),
      // so the 30s auto-refresh timer (same selection, no setter involved)
      // does not blank the chart on every tick.
      this.seriesByMetric = {}
      this.error = ''
      this.loaded = false
      void this.refresh()
    },
    setWindow(window: HistoryWindow): void {
      if (window === this.window) return
      this.window = window
      // Window changes both the time-series bucket resolution AND the
      // Models ranking's single current bucket — clear both.
      this.seriesByMetric = {}
      this.modelRanking = []
      this.error = ''
      this.loaded = false
      void this.refresh()
    },
    setTab(tab: ChartTab): void {
      if (tab === this.tab) return
      this.tab = tab
      this.seriesByMetric = {}
      this.modelRanking = []
      this.error = ''
      this.loaded = false
      void this.refresh()
    },
    setModelMetric(metric: ModelMetric): void {
      if (metric === this.modelMetric) return
      this.modelMetric = metric
      this.modelRanking = []
      this.error = ''
      this.loaded = false
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
      const requestId = ++this.seriesReqId
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
        // Stale: a newer selection (or the models-tab fetch) already
        // superseded this one — drop the result rather than overwriting
        // whatever the current selection has since fetched or cleared.
        if (requestId !== this.seriesReqId) return
        const next: Partial<Record<HistoryMetric, UsageHistoryPoint[]>> = {}
        metrics.forEach((metric, i) => {
          next[metric] = results[i]!.points
        })
        this.seriesByMetric = next
        this.loaded = true
        this.error = ''
      } catch (err) {
        if (requestId !== this.seriesReqId) return
        if (err instanceof AdminApiError && (err.status === 401 || err.status === 403)) {
          return
        }
        this.error = err instanceof Error ? err.message : String(err)
      } finally {
        // Only the still-current request clears `loading` — an older,
        // already-superseded fetch settling later must not flip it back to
        // false while the newer one it lost to is still in flight.
        if (requestId === this.seriesReqId) this.loading = false
      }
    },
    async fetchModelRanking(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      const requestId = ++this.seriesReqId
      this.loading = true
      try {
        const res = await adminFetch<AdminUsageModelsResponse>(
          `/admin/api/usage/models?metric=${this.modelMetric}&window=${this.window}&limit=${MODEL_RANKING_LIMIT}`,
        )
        if (requestId !== this.seriesReqId) return
        this.modelRanking = res.models
        this.loaded = true
        this.error = ''
      } catch (err) {
        if (requestId !== this.seriesReqId) return
        if (err instanceof AdminApiError && (err.status === 401 || err.status === 403)) {
          return
        }
        this.error = err instanceof Error ? err.message : String(err)
      } finally {
        if (requestId === this.seriesReqId) this.loading = false
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
      const requestId = ++this.optionsReqId
      try {
        const res = await adminFetch<AdminUsageModelsResponse>(
          `/admin/api/usage/models?metric=req&window=${this.window}&limit=${MODEL_OPTIONS_LIMIT}`,
        )
        // Stale: a later window change already fired its own picker-list
        // fetch — never let an older window's model list overwrite it.
        if (requestId !== this.optionsReqId) return
        this.modelOptions = res.models
      } catch {
        // Intentionally ignored — see the doc comment above.
      }
    },
    startAutoRefresh(): void {
      if (this.timer !== undefined) return
      // Skip a tick while the previous fetch is still in flight: each fetch
      // bumps seriesReqId, so an unconditional tick would supersede (and
      // discard) any request slower than REFRESH_MS, forever.
      this.timer = setInterval(() => {
        if (!this.loading) void this.refresh()
      }, REFRESH_MS)
    },
    stopAutoRefresh(): void {
      if (this.timer === undefined) return
      clearInterval(this.timer)
      this.timer = undefined
    },
  },
})
