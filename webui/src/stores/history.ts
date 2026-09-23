import { defineStore } from 'pinia'

import { AdminApiError, adminFetch } from '@/lib/api'
import { createVisibilityPoller, type VisibilityPoller } from '@/lib/polling'
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
 * Exported (P3 review fix) so ChartsView.vue can show a "Top N models"
 * note whenever the fetched ranking is exactly this long — the reader must
 * be told the list is a truncated top-N, not the complete set, the same
 * signal the on-screen search/filter affordances elsewhere in this panel
 * already give for a narrowed-but-possibly-incomplete list.
 */
export const MODEL_RANKING_LIMIT = 20

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

/**
 * WINDOW_LABEL is the window-switcher's own display text — a real span of
 * buckets, matching WINDOW_SPAN above, for EVERY tab including Models: the
 * ranking endpoint now sums the same WINDOW_SPAN buckets the time-series
 * tabs chart (F2 review fix), rather than reading modelTotals' old
 * single-current-bucket behaviour, so "24h"/"30d"/"12mo" is accurate for
 * it too and this is the one label every tab shares — no separate
 * MODEL_WINDOW_LABEL needed any more.
 */
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
    /**
     * Which metric the Models ranking ranks by — independent of `tab`.
     * Defaults to 'req' (free-models-plan.md), not 'cost': a cost-default
     * ranking silently hides every free model (its cost total is always
     * 0), and the picker/table both need free models to be reachable
     * without the reader first knowing to switch metrics.
     */
    modelMetric: 'req' as ModelMetric,
    seriesByMetric: {} as Partial<Record<HistoryMetric, UsageHistoryPoint[]>>,
    /** The current Models-tab ranking. Empty means "nothing used in this window". */
    modelRanking: [] as AdminUsageModelEntry[],
    /** Models with non-zero traffic, for the scope picker's model entries. */
    modelOptions: [] as AdminUsageModelEntry[],
    /**
     * modelFilter narrows the Models tab's ranking to ids starting with
     * this prefix (F6, dashboard-plan.md). P3 review fix: while the
     * Models tab is active, this is now also a SERVER query param —
     * setModelFilter sends it to /admin/api/usage/models as `prefix`
     * (fetchModelRanking's own doc comment) and refetches, since
     * filtering must happen before the server applies its top-N limit.
     * lib/model-filter.ts's filterModelsByPrefix still re-slices the
     * result client-side too (ChartsView.vue), a harmless no-op pass once
     * the server has already narrowed the set. Set by ProvidersView.vue's
     * provider-header link (nav.goToModels(`${p.name}/`), stores/nav.ts —
     * WP-B1) so "view this provider's models" lands on a pre-filtered
     * ranking instead of the reader hunting for it themselves, and it
     * round-trips through the `#charts?tab=models&filter=` hash param
     * (lib/hash-state.ts, composables/useHashState.ts) so a reload or a
     * shared link restores it too. Empty string means no filter.
     */
    modelFilter: '',
    loading: false,
    /** True once the current selection's first fetch committed; cleared by the setters. Drives the "Loading…" line so background refreshes don't flash it over rendered data. */
    loaded: false,
    error: '',
    poller: undefined as VisibilityPoller | undefined,
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
      // Window changes the time-series bucket resolution, the Models
      // ranking's own span, AND the picker's model list (Q1,
      // dashboard-plan.md DECISIONS: fetchModelOptions runs only on a
      // window change or initial load, never on the 30s auto-refresh tick)
      // — clear/refetch all three.
      this.seriesByMetric = {}
      this.modelRanking = []
      this.error = ''
      this.loaded = false
      void this.refresh()
      void this.fetchModelOptions()
    },
    setTab(tab: ChartTab): void {
      if (tab === this.tab) return
      this.tab = tab
      this.seriesByMetric = {}
      this.modelRanking = []
      // A stale provider-prefix filter from an earlier nav.goToModels visit
      // must not silently carry into a plain "Models" tab click — F6's
      // nav.goToModels always re-applies its own setModelFilter call right
      // after this one, so clearing here unconditionally never fights it.
      this.modelFilter = ''
      this.error = ''
      this.loaded = false
      void this.refresh()
    },
    /**
     * setModelFilter narrows the Models tab's ranking to ids starting with
     * `prefix`.
     *
     * P3 review fix: this used to be a PURE client-side re-slice of
     * already-fetched data, never a refetch (modelFilter's own state doc
     * comment above still describes the re-slice half, lib/model-filter.ts's
     * filterModelsByPrefix). Now that fetchModelRanking also sends `prefix`
     * to the SERVER (see that action's own doc comment), the already-fetched
     * ranking can be stale relative to a NEW prefix — it may have been
     * fetched under the old prefix (or no prefix at all), so simply
     * re-slicing it client-side no longer reflects what the server would
     * return for the new one. Refetching only when the Models tab is
     * actually active (the only tab that reads modelFilter at all — see
     * filterModelsByPrefix's own call site, ChartsView.vue) keeps this a
     * no-op fetch-wise everywhere else, and the early return on an
     * unchanged value keeps repeated identical calls (or nav.goToModels
     * re-applying the same prefix) from firing a redundant fetch.
     */
    setModelFilter(prefix: string): void {
      if (prefix === this.modelFilter) return
      this.modelFilter = prefix
      if (this.tab !== 'models') return
      this.modelRanking = []
      this.error = ''
      this.loaded = false
      void this.refresh()
    },
    /**
     * setSelection (P12 review fix) applies scope/window/tab/modelMetric/
     * modelFilter as ONE atomic change, instead of the caller invoking
     * several of the single-field setters above back to back. Each of
     * those setters independently clears state and calls refresh() (and,
     * for window, fetchModelOptions() too) — restoring a Charts hash with
     * several params set at once (composables/useHashState.ts's
     * applyFromHash) or nav.ts's goToCharts/goToModels used to call three
     * or four of them in a row, firing that many fetches, every one but
     * the LAST immediately discarded by seriesReqId's own
     * latest-request-wins guard (stores/history.ts state doc comment) —
     * wasted round trips against a Redis-backed endpoint for a result that
     * was never going to render. setSelection instead applies every
     * provided field directly, clears the derived state ONCE, and fires
     * exactly one refresh() (plus one fetchModelOptions() only when
     * `window` is part of this call) once every field has been applied.
     *
     * Field application order matters for one case: `tab` resets
     * `modelFilter` to '' first (mirroring setTab's own "a stale
     * provider-prefix filter must not silently carry into a plain tab
     * switch" convention), and only THEN does this function's own
     * `modelFilter` field (if provided in the SAME call) apply the real
     * value — so `setSelection({ tab: 'models', modelFilter: prefix })`
     * (nav.ts's goToModels) lands on the intended prefix, not '' followed
     * by a second, separate fetch to correct it.
     */
    setSelection(next: {
      scope?: string
      window?: HistoryWindow
      tab?: ChartTab
      modelMetric?: ModelMetric
      modelFilter?: string
    }): void {
      let changed = false
      let windowChanged = false
      if (next.window !== undefined && next.window !== this.window) {
        this.window = next.window
        changed = true
        windowChanged = true
      }
      if (next.tab !== undefined && next.tab !== this.tab) {
        this.tab = next.tab
        this.modelFilter = ''
        changed = true
      }
      if (next.scope !== undefined && next.scope !== this.scope) {
        this.scope = next.scope
        changed = true
      }
      if (next.modelMetric !== undefined && next.modelMetric !== this.modelMetric) {
        this.modelMetric = next.modelMetric
        changed = true
      }
      if (next.modelFilter !== undefined && next.modelFilter !== this.modelFilter) {
        this.modelFilter = next.modelFilter
        changed = true
      }
      if (!changed) return
      this.seriesByMetric = {}
      this.modelRanking = []
      this.error = ''
      this.loaded = false
      void this.refresh()
      if (windowChanged) void this.fetchModelOptions()
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
     * the Models tab, a time series otherwise. It deliberately does NOT
     * also refresh the scope picker's model list any more (Q1,
     * dashboard-plan.md DECISIONS — folded from a review finding on read
     * amplification): fetchModelOptions is a real extra store read
     * (modelSpanTotals, admin.go), and this action is what the 30s
     * auto-refresh timer calls every tick — repeating that read every
     * cycle bought the picker nothing a reader would ever notice. See
     * fetchModelOptions' own doc comment for its two actual call sites.
     */
    async refresh(): Promise<void> {
      await (this.tab === 'models' ? this.fetchModelRanking() : this.fetchSeries())
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
        const span = WINDOW_SPAN[this.window]
        // P3 review fix: when a provider-prefix filter is active
        // (modelFilter, set by ProvidersView.vue's provider-header link via
        // nav.goToModels), send it to the server so filtering happens
        // BEFORE the limit is applied — the client-side filterModelsByPrefix
        // pass in ChartsView.vue stays too, as a harmless second pass (it
        // is a no-op once the server has already narrowed the set), but
        // without this the server's own fleet-wide top-N could omit a
        // provider's models entirely, or return only a partial view of them
        // (the CONFIRMED problem this fixes).
        const prefixParam = this.modelFilter ? `&prefix=${encodeURIComponent(this.modelFilter)}` : ''
        // detail=1 (free-models-plan.md): the Models tab's own ranking
        // fetch always asks for the per-entry requests/tokensIn/tokensOut/
        // costMicroUsd/free breakdown, not just the single ranked `value` —
        // lib/model-table-columns.ts's table renders all four figures
        // (plus the free badge) beside every row regardless of which
        // metric is currently ranking. fetchModelOptions below stays
        // detail-free: the scope picker only ever needs an id list.
        const res = await adminFetch<AdminUsageModelsResponse>(
          `/admin/api/usage/models?metric=${this.modelMetric}&window=${this.window}&limit=${MODEL_RANKING_LIMIT}&span=${span}${prefixParam}&detail=1`,
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
     * one REQUEST across the current window's span. Requests, not the
     * ranking's own metric, deliberately — a model can serve traffic while
     * reporting no tokens and costing nothing, and such a model must still
     * be selectable. A failure here is swallowed rather than surfaced: it
     * degrades the picker, and must not replace a rendered chart's own
     * error (or clear it).
     *
     * Called only on initial load and on a window change (Q1,
     * dashboard-plan.md DECISIONS) — never from refresh() itself, so the
     * 30s auto-refresh tick pays for the ranking/series alone. See
     * setWindow above and ChartsView.vue's onMounted for the two call
     * sites.
     */
    async fetchModelOptions(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      const requestId = ++this.optionsReqId
      try {
        const span = WINDOW_SPAN[this.window]
        const res = await adminFetch<AdminUsageModelsResponse>(
          `/admin/api/usage/models?metric=req&window=${this.window}&limit=${MODEL_OPTIONS_LIMIT}&span=${span}`,
        )
        // Stale: a later window change already fired its own picker-list
        // fetch — never let an older window's model list overwrite it.
        if (requestId !== this.optionsReqId) return
        this.modelOptions = res.models
      } catch {
        // Intentionally ignored — see the doc comment above.
      }
    },
    /**
     * startAutoRefresh drives refresh() through the shared
     * createVisibilityPoller (lib/polling.ts, F5) instead of a bare
     * setInterval: it ticks once immediately (the selection's initial
     * fetch — see ChartsView.vue's onMounted, which no longer calls
     * refresh() itself), then every REFRESH_MS while the tab is visible,
     * pausing while it is hidden and catching up the moment it is looked
     * at again. The in-flight skip is unchanged from before this move: a
     * tick bumps seriesReqId on every real fetch, so an unconditional tick
     * would supersede (and discard) any request slower than REFRESH_MS,
     * forever.
     */
    startAutoRefresh(): void {
      if (this.poller) return
      this.poller = createVisibilityPoller({
        intervalMs: REFRESH_MS,
        tick: () => {
          if (!this.loading) void this.refresh()
        },
      })
      this.poller.start()
    },
    stopAutoRefresh(): void {
      this.poller?.stop()
      this.poller = undefined
    },
  },
})
