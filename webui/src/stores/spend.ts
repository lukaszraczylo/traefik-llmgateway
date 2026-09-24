import { defineStore } from 'pinia'

import { AdminApiError, adminFetch, isAuthRejection, messageOf } from '@/lib/api'
import { currentMonthDaySpan } from '@/lib/burndown'
import { createVisibilityPoller, type VisibilityPoller } from '@/lib/polling'
import { dayOrMonthWindow, seriesUrl, totalsUrl } from '@/lib/range'
import type { TotalsUrlParams } from '@/lib/range'
import { drilldownLevel, scopeBudgetLimits } from '@/lib/scope'
import { other, stackTopN, sumByProvider, type Series } from '@/lib/series'
import { usdToMicros } from '@/lib/usage-bars'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { useFiltersStore } from '@/stores/filters'
import type { AdminSeriesResponse, AdminTotalsResponse, AdminUsageModelsResponse, HistoryMetric, HistoryWindow } from '@/types/api'

/** SPEND_BY is the Spend page's breakdown-dimension selector (redesign-plan.md section 3.1's `by` page param). */
export const SPEND_BY = ['model', 'provider', 'group'] as const
export type SpendBy = (typeof SPEND_BY)[number]

/** isSpendBy is SPEND_BY's own runtime type guard, mirroring lib/range.ts's isRangeKey — the only place a hash-restored `by` value is checked against untrusted input. */
export function isSpendBy(value: string): value is SpendBy {
  return (SPEND_BY as readonly string[]).includes(value)
}

/** SPEND_METRICS is the Spend page's metric selector, in the plan's own display order (section 3.1: "metric=cost|req|tokin|tokout") — the same four values HistoryMetric already names, reused rather than redeclared. */
export const SPEND_METRICS: HistoryMetric[] = ['cost', 'req', 'tokin', 'tokout']

export const SPEND_METRIC_LABEL: Record<HistoryMetric, string> = {
  cost: 'Cost',
  req: 'Requests',
  tokin: 'Tokens in',
  tokout: 'Tokens out',
}

/** isSpendMetric guards a hash-restored `metric` param against HistoryMetric's own union. */
export function isSpendMetric(value: string): value is HistoryMetric {
  return (SPEND_METRICS as readonly string[]).includes(value)
}

/** How many of the by=model breakdown's ranked models get their OWN charted series before the remainder folds into "other" (redesign-plan.md section 3.3: "series for top 10 models"). */
export const MODEL_BREAKDOWN_TOP_N = 10

/**
 * MODEL_RANKING_LIMIT is the shared model-ranking fetch's own limit —
 * redesign-plan.md's own usageModelsMaxLimit ceiling (section 0 ground
 * truth: "limit<=100"). ONE ranking fetch at this size serves three
 * different Spend cards: the by=model chart (top MODEL_BREAKDOWN_TOP_N of
 * these), PricingHealthTable (every row), and CostAvoidedCard's
 * reference-model picker — rather than each fetching its own.
 */
const MODEL_RANKING_LIMIT = 100

/** GET /admin/api/usage/series accepts at most this many `scope` params per request (redesign-plan.md section 1.3.iv: "scope repeated 1..100") — the by=provider/by=group breakdown fetches cap their own model/group id lists at this so a large fleet never trips the server's own 400. */
const MAX_SERIES_SCOPES = 100

/** How often the Spend page's own data (breakdown, ranking, burn-down, drilldown) re-polls while visible — slower than the 5s dashboard poll (stores/dashboard.ts), matching stores/history.ts's own REFRESH_MS for the identical reason: history buckets move on the timescale of the window itself, not every request. */
const REFRESH_MS = 60_000

/**
 * useSpendStore drives the Spend page (redesign-plan.md section 3.3,
 * replacing stores/history.ts): the cost/req/tokens breakdown chart (by
 * model, provider, or group), the model ranking that breakdown and two
 * other Spend cards (PricingHealthTable, CostAvoidedCard) all share, the
 * current-month burn-down line, and the group -> user -> model attribution
 * drilldown. Every fetch reads the GLOBAL filters store (range/cmp/scope)
 * directly rather than taking them as action parameters — the same reason
 * any Pinia store reads another store from inside an action (stores/
 * dashboard.ts reads useAuthStore()) — so SpendPage.vue only needs to call
 * refresh() again when a global filter OR this store's own `by`/`metric`/
 * `drill` selection changes, never thread values through by hand.
 *
 * SCOPE NARROWING (filters.scope): when narrowed to a single group or user
 * (`group:x`/`user:x`), the breakdown chart shows that ONE scope's own
 * series directly — the `by` dimension has nothing left to break down once
 * the reader has already picked one entity, and AttributionDrilldown below
 * it is exactly the tool for descending further within it. When narrowed
 * to a provider (`provider:x`), the breakdown is derived the same way
 * by=provider already is (lib/series.ts's sumByProvider), pre-filtered to
 * that one provider's own models — cost/req/tokin/tokout has no direct
 * `provider:{name}` series scope (redesign-plan.md section 1.3.iv's metric
 * x kind table). The burn-down line has no meaningful reading for a
 * provider scope at all (a provider carries no configured budget) and is
 * left empty in that case (`burndownAvailable` getter below) rather than a
 * fabricated one.
 */
export const useSpendStore = defineStore('spend', {
  state: () => ({
    by: 'model' as SpendBy,
    metric: 'cost' as HistoryMetric,
    /** '' (top level, group list), 'group:{name}' (user list within that group), or 'user:{name}' (model list for that user) — AttributionDrilldown.vue's own breadcrumb state, mirrored from the `drill` page param (SpendPage.vue owns syncing it both ways). */
    drill: '',

    // --- breakdown chart (TimeSeriesChart, by/scope-driven) ---
    buckets: [] as string[],
    /** The charted breakdown series (top-N models, providers, or groups — or a single series when filters.scope narrows to one entity). Never includes the synthesized "other" bucket; see `otherPoints`. */
    breakdown: [] as Series[],
    /** by=model only: total minus the sum of the charted top-N (lib/series.ts's `other`) — null for every other `by`/scope combination, where nothing is left out of the chart. */
    otherPoints: null as number[] | null,
    totalPoints: [] as number[],
    /** The same total, shifted back by filters.cmpSpec.offset buckets — null unless filters.cmpSpec.requested. */
    comparisonPoints: null as number[] | null,
    /**
     * comparisonUnavailableReason (P3 item 19, Q5's "show a reason"): set
     * when a comparison WAS requested (filters.cmpSpec.requested) but this
     * store could not build one anyway — currently only the provider-scope
     * case (fetchComparison's own doc comment: no direct series scope for
     * cost/req/tokin/tokout on a `provider:{name}` scope, and doubling
     * every provider-scope request through sumByProvider was not worth it
     * for a reference overlay). Null whenever a comparison was not
     * requested at all, or was requested and DID build successfully — a
     * reader only needs this note when the dashed line they asked for is
     * silently missing, not as a blanket status message.
     */
    comparisonUnavailableReason: null as string | null,

    // --- model ranking: the by=model chart and CostAvoidedCard's reference-model picker ---
    modelRanking: [] as AdminUsageModelsResponse['models'],
    /**
     * pricingHealthRanking (P3 item 21) is a SEPARATE ranking, always
     * sorted by `req` — PricingHealthTable.vue's own "unpriced first"
     * ordering needs the models with the most TRAFFIC, not whichever
     * metric the reader currently has the Spend breakdown sorted by
     * (`this.metric`). With more than MODEL_RANKING_LIMIT served models,
     * sorting by `cost` (the default metric) cuts the ranking off at 100
     * models ranked BY COST — and an unpriced model bills $0, so it is
     * one of the FIRST rows dropped by that cutoff, defeating "unpriced
     * first" for the operator who most needs to see it. Sorting by `req`
     * instead still has a cutoff (a model with vanishingly few requests
     * AND no price could still fall outside the top 100), but it no
     * longer discards a row specifically BECAUSE it is unpriced.
     */
    pricingHealthRanking: [] as AdminUsageModelsResponse['models'],

    // --- burn-down (BurnDownChart) ---
    burndownBuckets: [] as string[],
    /** Raw per-day cost (micro-USD), NOT yet cumulative — BurnDownChart.vue runs lib/burndown.ts's own `cumulative` over this, keeping the running-sum step in one place rather than duplicated between the store and the component. */
    burndownPoints: [] as number[],
    /** The scope's own configured cost/month budget in micro-USD, or null when none is configured (or the scope has no budget concept — see `burndownAvailable`). */
    burndownBudgetMicros: null as number | null,

    // --- attribution drilldown (AttributionDrilldown) ---
    drilldownRows: [] as AdminTotalsResponse['rows'],
    drilldownMetrics: [] as string[],
    /** True once GET /admin/api/usage/totals reported this exact drill level as unavailable (usermodel with admin.stats.userModel off, adminTotalsResponse's own 404) — AttributionDrilldown.vue renders the hint naming the config key instead of an empty table. */
    drilldownDisabled: false,

    loading: false,
    /**
     * breakdownError/pricingHealthError/drilldownError/burndownError
     * (verify-ui-states-2.md #3 fix) replace a single shared `error` field
     * — that field was cleared ONLY on fetchBreakdown's own success (never
     * by the other three legs), so a pricing-health/drilldown/burndown
     * failure that happened to land before a later, unrelated breakdown
     * success got silently wiped (a false "no traffic" reading over a real
     * failure), or, the other direction, an already-cleared error from one
     * leg could sit stale under a DIFFERENT card that loaded fine. Each
     * card below (SpendPage.vue) now reads and retries only its own field.
     * fetchModelRanking's own failures used to fold into `breakdownError`
     * too — see `rankingError` below (verify-ui-states-3.md pre-existing
     * fix) for why that was also wrong and what replaced it.
     */
    breakdownError: '',
    /**
     * rankingError (verify-ui-states-3.md pre-existing-bug fix) is
     * fetchModelRanking's OWN error field. It used to fold into
     * `breakdownError`, which a SUBSEQUENT fetchBreakdown success then
     * cleared unconditionally (line below), silently wiping a real ranking
     * failure the moment breakdown itself finished fetching — even a
     * breakdown built from that same still-empty/stale ranking (a cold
     * by=model load then renders 100% "Other" with no error in sight).
     * Never touched by fetchBreakdown, so SpendPage.vue can surface it next
     * to the breakdown chart independently, with no cross-clearing.
     */
    rankingError: '',
    pricingHealthError: '',
    drilldownError: '',
    burndownError: '',
    /**
     * pricingHealthLoading/drilldownLoading/burndownLoading (verify-ui-
     * states.md #3/#7): PricingHealthTable.vue, AttributionDrilldown.vue,
     * and BurnDownChart's own SpendPage.vue wrapper each need their OWN
     * in-flight signal — `loading` above only wraps fetchBreakdown (the
     * by=model/provider/group chart), not these three, which run in
     * parallel via refresh()'s own Promise.all — so reusing `loading` for
     * any of them would read false (or wrongly true) independent of
     * whether THEIR OWN fetch is actually in flight.
     */
    pricingHealthLoading: false,
    drilldownLoading: false,
    burndownLoading: false,
    /**
     * pricingHealthLoaded/drilldownLoaded (verify-ui-states-2.md #2 fix)
     * are "has a fetch completed for the CURRENT selection" —
     * PricingHealthTable.vue's/AttributionDrilldown.vue's own `loaded`
     * prop — set once their own fetch settles into a genuine result
     * (success, or a known-disabled 404 for the drilldown), and reset the
     * moment the selection they depend on changes (see
     * pricingHealthWindow/pricingHealthSpan and
     * drilldownWindow/drilldownSpan/drilldownDrill below), so a
     * genuinely empty table still reads as settled/empty across the 60s
     * poll's own `loading` flips instead of flashing a skeleton on every
     * tick.
     */
    pricingHealthLoaded: false,
    drilldownLoaded: false,
    /** The (window, span) pricingHealthRanking was last fetched FOR — fetchPricingHealthRanking's own "did the selection this ranking depends on actually change" check, resetting pricingHealthLoaded below exactly when it did. */
    pricingHealthWindow: null as HistoryWindow | null,
    pricingHealthSpan: null as number | null,
    /** The (window, span, drill) drilldownRows were last fetched FOR — fetchDrilldown's own equivalent of the field above. */
    drilldownWindow: null as HistoryWindow | null,
    drilldownSpan: null as number | null,
    drilldownDrillFetchedFor: null as string | null,
    /**
     * The (by, metric, window, span, scope) `buckets` was last fetched FOR
     * (verify-ui-states-3.md NEW-1) — fetchBreakdown's own equivalent of
     * the two field groups above. A genuine SELECTION change clears
     * `buckets` synchronously, before the fetch starts, so SpendPage.vue's
     * breakdownState (hasData: buckets.length > 0) flips to 'skeleton'
     * immediately instead of leaving the PREVIOUS selection's series on
     * screen mislabeled under the NEW selection's metric/by/range/scope. A
     * same-selection refetch (a 60s poll tick, or a Retry) leaves `buckets`
     * alone — loadState's own 'ready' precedence (lib/load-state.ts).
     */
    breakdownByFetchedFor: null as SpendBy | null,
    breakdownMetricFetchedFor: null as HistoryMetric | null,
    breakdownWindow: null as HistoryWindow | null,
    breakdownSpan: null as number | null,
    breakdownScope: null as string | null,
    /** breakdownReqId/rankingReqId/burndownReqId/drilldownReqId are FOUR independent latest-request-wins counters (stores/history.ts's own seriesReqId precedent), one per concern — unlike history.ts's tabs, every one of these fetches concurrently on a single Spend page render, so a single shared counter would make an in-flight ranking fetch spuriously discard a still-relevant breakdown fetch (or vice versa) merely for starting first. */
    breakdownReqId: 0,
    rankingReqId: 0,
    pricingHealthReqId: 0,
    burndownReqId: 0,
    drilldownReqId: 0,
    /** refreshReqId (verify-ui-states-2.md #10 fix) is refresh()'s own latest-request-wins counter over `this.loading` — an overlapping poll-tick refresh and a selection/filter-change refresh can otherwise resolve in either order, and the FIRST to finish clears `loading` while the SECOND is still in flight. */
    refreshReqId: 0,
    poller: undefined as VisibilityPoller | undefined,
  }),
  getters: {
    /** burndownAvailable is false only for a provider-narrowed scope (see this store's own doc comment) — BurnDownChart.vue renders "not available for this scope" instead of an empty chart. */
    burndownAvailable: (): boolean => burndownScopeParam(useFiltersStore().scope) !== null,
  },
  actions: {
    /** setSelection applies by/metric/drill as one atomic change (stores/history.ts's own setSelection precedent) and fires exactly one refresh(), rather than each field's own setter independently clearing state and refetching. */
    setSelection(next: { by?: SpendBy; metric?: HistoryMetric; drill?: string }): void {
      let changed = false
      if (next.by !== undefined && next.by !== this.by) {
        this.by = next.by
        changed = true
      }
      if (next.metric !== undefined && next.metric !== this.metric) {
        this.metric = next.metric
        changed = true
      }
      if (next.drill !== undefined && next.drill !== this.drill) {
        this.drill = next.drill
        changed = true
      }
      if (!changed) return
      void this.refresh()
    },
    /**
     * refresh re-fetches every section this store owns against the
     * CURRENT by/metric/drill selection and the current global filters —
     * the one action both the 60s poller and any selection/filter change
     * call. fetchBreakdown is sequenced AFTER fetchModelRanking (not run
     * in the same Promise.all) because by=model/by=provider both read
     * `this.modelRanking` to decide which ids to chart — starting them
     * concurrently would race fetchBreakdown against a still-in-flight (or
     * still-empty, on first load) ranking. fetchBurndown/fetchDrilldown
     * depend on neither and run fully in parallel with that chain.
     *
     * `this.loading` is now owned HERE, wrapping the whole
     * fetchModelRanking-then-fetchBreakdown chain (verify-ui-states.md
     * live-preview fix), not just fetchBreakdown's own body: SpendPage.
     * vue's breakdownState (lib/load-state.ts) reads `spend.loading` to
     * decide skeleton-vs-empty for the breakdown chart, and with the flag
     * previously scoped to fetchBreakdown alone, it stayed FALSE for the
     * entire fetchModelRanking leg — live verification caught the
     * breakdown chart rendering as a bare empty grid (loadState's 'empty'
     * branch, not 'skeleton') for that whole first leg on every load.
     */
    async refresh(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      // verify-ui-states-2.md #10: latest-request-wins over `this.loading`
      // itself — a poll tick and a selection/filter-change refresh can
      // overlap and resolve in either order; only the LATEST refresh call
      // is allowed to clear the flag, so an earlier, still-settling one
      // finishing first never clears it out from under the later one.
      const requestId = ++this.refreshReqId
      this.loading = true
      try {
        await Promise.all([
          this.fetchModelRanking().then(() => this.fetchBreakdown()),
          this.fetchPricingHealthRanking(),
          this.fetchBurndown(),
          this.fetchDrilldown(),
        ])
      } finally {
        if (requestId === this.refreshReqId) this.loading = false
      }
    },
    async fetchModelRanking(): Promise<void> {
      const filters = useFiltersStore()
      const requestId = ++this.rankingReqId
      try {
        const res = await adminFetch<AdminUsageModelsResponse>(
          `/admin/api/usage/models?metric=${this.metric}&window=${filters.window}&span=${filters.span}&limit=${MODEL_RANKING_LIMIT}&detail=1`,
        )
        if (requestId !== this.rankingReqId) return
        this.modelRanking = res.models
        this.rankingError = ''
      } catch (err) {
        if (requestId !== this.rankingReqId) return
        if (isAuthRejection(err)) return
        // Own field, never folded into breakdownError — see this store's
        // own doc comment on rankingError (verify-ui-states-3.md
        // pre-existing fix).
        this.rankingError = messageOf(err)
      }
    },
    /** fetchPricingHealthRanking is pricingHealthRanking's own fetch — always metric=req, independent of `this.metric` (see that field's own doc comment, P3 item 21). Runs in parallel with fetchModelRanking in refresh(), not chained after it: neither depends on the other's result. */
    async fetchPricingHealthRanking(): Promise<void> {
      const filters = useFiltersStore()
      // verify-ui-states-2.md #2: a genuine SELECTION change (this ranking
      // depends only on window/span, not `by`/`metric`/`drill`/scope)
      // resets pricingHealthLoaded so the new selection's own skeleton
      // shows — a background poll tick (same window/span) leaves it alone.
      if (this.pricingHealthWindow !== filters.window || this.pricingHealthSpan !== filters.span) {
        this.pricingHealthLoaded = false
      }
      this.pricingHealthWindow = filters.window
      this.pricingHealthSpan = filters.span
      const requestId = ++this.pricingHealthReqId
      this.pricingHealthLoading = true
      try {
        const res = await adminFetch<AdminUsageModelsResponse>(
          `/admin/api/usage/models?metric=req&window=${filters.window}&span=${filters.span}&limit=${MODEL_RANKING_LIMIT}&detail=1`,
        )
        if (requestId !== this.pricingHealthReqId) return
        this.pricingHealthRanking = res.models
        this.pricingHealthError = ''
        this.pricingHealthLoaded = true
      } catch (err) {
        if (requestId !== this.pricingHealthReqId) return
        if (isAuthRejection(err)) return
        this.pricingHealthError = messageOf(err)
      } finally {
        if (requestId === this.pricingHealthReqId) this.pricingHealthLoading = false
      }
    },
    async fetchBreakdown(): Promise<void> {
      const filters = useFiltersStore()
      // verify-ui-states-3.md NEW-1: a genuine SELECTION change (by,
      // metric, window, span, or scope) clears `buckets` synchronously,
      // BEFORE the fetch starts — flips breakdownState to 'skeleton'
      // immediately instead of leaving the OLD selection's series on
      // screen mislabeled under the NEW selection. A same-selection
      // refetch (a 60s poll tick, or a Retry) leaves `buckets` alone.
      if (
        this.breakdownByFetchedFor !== this.by ||
        this.breakdownMetricFetchedFor !== this.metric ||
        this.breakdownWindow !== filters.window ||
        this.breakdownSpan !== filters.span ||
        this.breakdownScope !== filters.scope
      ) {
        this.buckets = []
      }
      this.breakdownByFetchedFor = this.by
      this.breakdownMetricFetchedFor = this.metric
      this.breakdownWindow = filters.window
      this.breakdownSpan = filters.span
      this.breakdownScope = filters.scope
      const requestId = ++this.breakdownReqId
      try {
        if (filters.scope === 'all') {
          await this.fetchBreakdownFleetWide(filters.window, filters.span, requestId)
        } else if (filters.scope.startsWith('group:') || filters.scope.startsWith('user:')) {
          await this.fetchBreakdownSingleScope(filters.scope, filters.window, filters.span, requestId)
        } else {
          // provider:{name} — no direct cost/req/tokin/tokout series scope
          // (see this store's own doc comment); derive from the model
          // ranking's own ids, pre-filtered to that one provider.
          await this.fetchBreakdownProviderScope(filters.scope.slice('provider:'.length), filters.window, filters.span, requestId)
        }
        if (requestId !== this.breakdownReqId) return
        this.breakdownError = ''
        if (filters.cmpSpec.requested) {
          await this.fetchComparison(filters.scope, filters.window, filters.span, filters.cmpSpec.offset, requestId)
        } else {
          this.comparisonPoints = null
          this.comparisonUnavailableReason = null
        }
      } catch (err) {
        if (requestId !== this.breakdownReqId) return
        if (isAuthRejection(err)) return
        this.breakdownError = messageOf(err)
      }
    },
    /**
     * fetchBreakdownFleetWide honors `this.by` exactly (model/provider/
     * group breakdown) — the ordinary, unscoped Spend view. The fleet-wide
     * TOTAL is always fetched in its own single-scope request first (never
     * folded into the breakdown request itself): by=provider/by=group can
     * legitimately need up to MAX_SERIES_SCOPES ids on their own, and
     * mixing 'total' into that same request would risk tipping it over
     * the server's own scope-count cap.
     */
    async fetchBreakdownFleetWide(window: HistoryWindow, span: number, requestId: number): Promise<void> {
      const totalRes = await this.fetchSeries(['total'], this.metric, window, span, 0)
      if (requestId !== this.breakdownReqId) return
      this.buckets = totalRes.buckets
      const total = totalRes.series.find((s) => s.scope === 'total')?.points ?? new Array<number>(totalRes.buckets.length).fill(0)
      this.totalPoints = total

      if (this.by === 'model') {
        const ids = this.modelRanking.slice(0, MODEL_BREAKDOWN_TOP_N).map((m) => `model:${m.id}`)
        if (ids.length === 0) {
          this.breakdown = []
          this.otherPoints = other(total, [])
          return
        }
        const res = await this.fetchSeries(ids, this.metric, window, span, 0)
        if (requestId !== this.breakdownReqId) return
        const top = stackTopN(res.series, MODEL_BREAKDOWN_TOP_N)
        this.breakdown = top
        this.otherPoints = other(total, top)
        return
      }

      if (this.by === 'provider') {
        const ids = this.modelRanking.slice(0, MAX_SERIES_SCOPES).map((m) => `model:${m.id}`)
        if (ids.length === 0) {
          this.breakdown = []
          this.otherPoints = null
          return
        }
        const res = await this.fetchSeries(ids, this.metric, window, span, 0)
        if (requestId !== this.breakdownReqId) return
        this.breakdown = sumByProvider(res.series)
        this.otherPoints = null
        return
      }

      // by === 'group': one series per currently configured group, from
      // GET /admin/api/usage/totals?kind=group (the authoritative group
      // list) for the id list, then a real time series per group id.
      const groupsRes = await adminFetch<AdminTotalsResponse>(totalsUrl({ kind: 'group', window, span, metrics: [this.metric] }))
      if (requestId !== this.breakdownReqId) return
      const groupIds = groupsRes.rows.slice(0, MAX_SERIES_SCOPES).map((r) => `group:${r.id}`)
      if (groupIds.length === 0) {
        this.breakdown = []
        this.otherPoints = null
        return
      }
      const res = await this.fetchSeries(groupIds, this.metric, window, span, 0)
      if (requestId !== this.breakdownReqId) return
      this.breakdown = res.series
      this.otherPoints = null
    },
    /** fetchBreakdownSingleScope renders exactly one line — the narrowed group/user itself — ignoring `by` (nothing left to break down within one already-chosen entity). */
    async fetchBreakdownSingleScope(scope: string, window: HistoryWindow, span: number, requestId: number): Promise<void> {
      const res = await this.fetchSeries([scope], this.metric, window, span, 0)
      if (requestId !== this.breakdownReqId) return
      this.buckets = res.buckets
      const points = res.series.find((s) => s.scope === scope)?.points ?? new Array<number>(res.buckets.length).fill(0)
      this.breakdown = [{ scope, points }]
      this.otherPoints = null
      this.totalPoints = points
    },
    /** fetchBreakdownProviderScope sums the ranking's own model ids under this one provider (lib/series.ts's sumByProvider, applied to a pre-filtered set) — see this store's own doc comment on why a provider scope cannot use a direct series scope for cost/req/tokin/tokout. */
    async fetchBreakdownProviderScope(providerName: string, window: HistoryWindow, span: number, requestId: number): Promise<void> {
      const ids = this.modelRanking
        .filter((m) => m.id.startsWith(`${providerName}/`))
        .slice(0, MAX_SERIES_SCOPES)
        .map((m) => `model:${m.id}`)
      if (ids.length === 0) {
        this.buckets = []
        this.breakdown = []
        this.otherPoints = null
        this.totalPoints = []
        return
      }
      const res = await this.fetchSeries(ids, this.metric, window, span, 0)
      if (requestId !== this.breakdownReqId) return
      this.buckets = res.buckets
      const [summed] = sumByProvider(res.series)
      const points = summed?.points ?? new Array<number>(res.buckets.length).fill(0)
      this.breakdown = [{ scope: `provider:${providerName}`, points }]
      this.otherPoints = null
      this.totalPoints = points
    },
    async fetchComparison(scope: string, window: HistoryWindow, span: number, offset: number, requestId: number): Promise<void> {
      const totalScope = scope === 'all' ? 'total' : scope.startsWith('provider:') ? null : scope
      if (totalScope === null) {
        // A provider scope's comparison line would need the same
        // sumByProvider derivation as the primary series — skipped rather
        // than doubling every provider-scope request; the dashed line
        // simply does not render (TimeSeriesChart's own
        // `comparisonDatasets` defaults to empty). Q5 requires a reason
        // when a requested comparison is unavailable (P3 item 19) — this
        // used to leave the reader guessing why "vs previous period" was
        // pressed but no dashed line appeared.
        this.comparisonPoints = null
        this.comparisonUnavailableReason = 'Comparison is not available for a single-provider scope.'
        return
      }
      const res = await this.fetchSeries([totalScope], this.metric, window, span, offset)
      if (requestId !== this.breakdownReqId) return
      this.comparisonPoints = res.series.find((s) => s.scope === totalScope)?.points ?? null
      this.comparisonUnavailableReason = null
    },
    async fetchSeries(scope: string[], metric: string, window: HistoryWindow, span: number, offset: number): Promise<AdminSeriesResponse> {
      return adminFetch<AdminSeriesResponse>(seriesUrl({ scope, metric, window, span, offset: offset || undefined }))
    },
    async fetchBurndown(): Promise<void> {
      const filters = useFiltersStore()
      const requestId = ++this.burndownReqId
      const scope = burndownScopeParam(filters.scope)
      if (scope === null) {
        this.burndownBuckets = []
        this.burndownPoints = []
        this.burndownBudgetMicros = null
        // verify-ui-states-3.md NEW-2: also reset burndownError/Loading —
        // an in-flight burndown for a PREVIOUS scope has its own `finally`
        // fail the reqId check below (burndownReqId was just bumped
        // above), so without this its stale error/loading would otherwise
        // survive a detour through a provider scope (chart hidden) and
        // reappear once the reader switches back to a scope with a chart.
        this.burndownError = ''
        this.burndownLoading = false
        return
      }
      this.burndownLoading = true
      try {
        const span = currentMonthDaySpan(new Date())
        const res = await this.fetchSeries([scope], 'cost', 'day', span, 0)
        if (requestId !== this.burndownReqId) return
        this.burndownBuckets = res.buckets
        this.burndownPoints = res.series.find((s) => s.scope === scope)?.points ?? new Array<number>(res.buckets.length).fill(0)
        this.burndownBudgetMicros = burndownBudgetMicros(scope)
        this.burndownError = ''
      } catch (err) {
        if (requestId !== this.burndownReqId) return
        if (isAuthRejection(err)) return
        this.burndownError = messageOf(err)
      } finally {
        if (requestId === this.burndownReqId) this.burndownLoading = false
      }
    },
    async fetchDrilldown(): Promise<void> {
      const filters = useFiltersStore()
      // verify-ui-states-2.md #2: a genuine SELECTION change (window,
      // span, or the drill breadcrumb itself) resets drilldownLoaded so
      // the new selection's own skeleton shows — a background poll tick
      // (same three values) leaves it alone.
      if (this.drilldownWindow !== filters.window || this.drilldownSpan !== filters.span || this.drilldownDrillFetchedFor !== this.drill) {
        this.drilldownLoaded = false
      }
      this.drilldownWindow = filters.window
      this.drilldownSpan = filters.span
      this.drilldownDrillFetchedFor = this.drill
      const requestId = ++this.drilldownReqId
      const level = drilldownLevel(this.drill)
      this.drilldownLoading = true
      try {
        // usermodel only accepts day/month (redesign-plan.md section
        // 1.3.v) — clamped via lib/range.ts's shared dayOrMonthWindow so
        // an hour-resolution global range (24h/48h) never 400s BEFORE the
        // server's own 404-when-disabled feature gate has a chance to run
        // (stats_read.go: the window check runs first), which previously
        // masked the "enable admin.stats.userModel" hint behind a generic
        // error even when the feature really was off.
        const params: TotalsUrlParams =
          level.kind === 'usermodel'
            ? { kind: 'usermodel', ...dayOrMonthWindow(filters.window, filters.span), user: level.user }
            : level.kind === 'user'
              ? { kind: 'user', window: filters.window, span: filters.span, group: level.group, metrics: ['req', 'tokin', 'tokout', 'cost'] }
              : { kind: 'group', window: filters.window, span: filters.span, metrics: ['req', 'tokin', 'tokout', 'cost'] }
        const res = await adminFetch<AdminTotalsResponse>(totalsUrl(params))
        if (requestId !== this.drilldownReqId) return
        this.drilldownRows = res.rows
        this.drilldownMetrics = res.metrics
        this.drilldownDisabled = false
        this.drilldownError = ''
        this.drilldownLoaded = true
      } catch (err) {
        if (requestId !== this.drilldownReqId) return
        if (isAuthRejection(err)) return
        if (err instanceof AdminApiError && err.status === 404) {
          // usermodel with admin.stats.userModel off (redesign-plan.md
          // section 1.3.v) — a known, named-in-the-hint state, not an
          // error the reader needs to see in the status line. Still a
          // SETTLED fetch for this selection (AttributionDrilldown.vue's
          // own hint Alert takes precedence over skeleton/error/empty
          // regardless), so drilldownLoaded is set here too.
          this.drilldownRows = []
          this.drilldownMetrics = []
          this.drilldownDisabled = true
          this.drilldownError = ''
          this.drilldownLoaded = true
          return
        }
        this.drilldownError = messageOf(err)
      } finally {
        if (requestId === this.drilldownReqId) this.drilldownLoading = false
      }
    },
    startPolling(): void {
      if (this.poller) return
      this.poller = createVisibilityPoller({ intervalMs: REFRESH_MS, tick: () => void this.refresh() })
      this.poller.start()
    },
    stopPolling(): void {
      this.poller?.stop()
      this.poller = undefined
    },
  },
})

/** burndownScopeParam maps the global scope filter to the /usage/series scope this store's burn-down line requests — null for a provider scope, which has no cost series and no budget concept (see useSpendStore's own doc comment). */
function burndownScopeParam(filtersScope: string): string | null {
  if (filtersScope === 'all') return 'total'
  if (filtersScope.startsWith('group:') || filtersScope.startsWith('user:')) return filtersScope
  return null
}

/**
 * burndownBudgetMicros resolves the configured cost/month budget (micro-
 * USD) for the burn-down line's own scope, via lib/scope.ts's shared
 * scopeBudgetLimits (reuse-audit.md F5 — the fleet-wide sum-of-group-
 * budgets 'total' case and the per-group/user lookup both used to be
 * reimplemented here a third time) — read from stores/dashboard.ts's
 * already-polled overview/usage data rather than a second network round
 * trip. `burndownScopeParam`'s own 'total' sentinel is scopeBudgetLimits'
 * "unscoped root" case (same as filters.scope's 'all'); a provider scope
 * never reaches here (burndownScopeParam itself returns null for one).
 */
function burndownBudgetMicros(scope: string): number | null {
  const dashboard = useDashboardStore()
  const usd = scopeBudgetLimits(scope, dashboard.overview?.groups ?? [], dashboard.usage?.users ?? [])?.costPerMonthUSD
  return usd && usd > 0 ? usdToMicros(usd) : null
}

