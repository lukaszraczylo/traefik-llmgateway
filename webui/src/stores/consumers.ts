import { defineStore } from 'pinia'

import { AdminApiError, adminFetch, isAuthRejection, messageOf } from '@/lib/api'
import { dayOrMonthWindow, seriesUrl, totalsUrl } from '@/lib/range'
import { useAuthStore } from '@/stores/auth'
import type { AdminConsumersResponse, AdminSeriesResponse, AdminTotalsResponse, HistoryWindow } from '@/types/api'

/** REFRESH_STALE_MS is how long a previously-fetched /admin/api/consumers response is trusted before ConsumersPage.vue's mount triggers a fresh fetch — this endpoint reflects the operator's own config (users/groups/access lists), which changes on a config edit, not every 5s like the polled dashboard store. A 60s staleness window (matches Q12's "new endpoints on demand or 60s" DECISION) means switching between pages within the Consumers tab never re-fetches on every mount, while a long-open session still notices a config reload eventually. */
const REFRESH_STALE_MS = 60_000

/**
 * UserDetailState is one user's on-demand detail — UserDetail.vue's own
 * data (timeline series, per-model totals), fetched only when that
 * user's row is actually opened, keyed by user id so switching between
 * two already-opened users never re-fetches.
 *
 * `reqError`/`costError`/`modelError` are THREE independent fields
 * (verify-ui-states.md #4 fix — previously one shared `error`, joined
 * from all three requests): a failure in only ONE of the three
 * Promise.allSettled calls (say, the usermodel totals) used to set that
 * one shared field, and UserDetail.vue's template read it for BOTH the
 * "Requests over time" and "Cost over time" cards — an unrelated
 * modelTotals failure replaced two perfectly valid, already-loaded
 * charts with ErrorState. Each chart/table now reads only its own field.
 */
export interface UserDetailState {
  loading: boolean
  reqError: string
  costError: string
  /** '' whenever modelTotalsUnavailable is true (a 404 is an expected, named state — modelTotalsUnavailable below, not a generic error) or the fetch was skipped (userModelStatsEnabled false). */
  modelError: string
  reqSeries: AdminSeriesResponse | null
  costSeries: AdminSeriesResponse | null
  /** null while userModelStats is off (no fetch attempted), the 404 case (see modelTotalsUnavailable), or before the first fetch resolves. */
  modelTotals: AdminTotalsResponse | null
  /** True once the usermodel totals call itself came back 404 (admin.go: disabled feature gate) — checked INDEPENDENTLY of the caller's own userModelStatsEnabled flag, since that flag reads the polled overview, which may not have loaded yet on a fresh deep link/reload, or may be stale relative to a live config change (see UserDetail.vue's own featuresUserModelStats watch). */
  modelTotalsUnavailable: boolean
  fetchedAt: number | null
  /** reqId guards against an out-of-order response overwriting a newer one (latest-request-wins) — two ranges in flight for the same user can otherwise land out of order. */
  reqId: number
  /** The (window, span) this state's OWN reqSeries/costSeries/modelTotals were fetched for, or null before the first fetch ever starts — fetchUserDetail's own "is this a genuine range change, not just a same-range refetch" check (verify-ui-states.md #4 fix), see that action's own doc comment. */
  window: HistoryWindow | null
  span: number | null
}

function emptyDetail(): UserDetailState {
  return {
    loading: false,
    reqError: '',
    costError: '',
    modelError: '',
    reqSeries: null,
    costSeries: null,
    modelTotals: null,
    modelTotalsUnavailable: false,
    fetchedAt: null,
    reqId: 0,
    window: null,
    span: null,
  }
}

/** settledError reads one Error message off a rejected PromiseSettledResult, or '' for a fulfilled one. */
function settledError(result: PromiseSettledResult<unknown>): string {
  if (result.status !== 'rejected') return ''
  return messageOf(result.reason)
}

/**
 * useConsumersStore backs the Consumers page (redesign-plan.md section
 * 3.3/3.4): the user/group directory + access lists from GET /admin/api/
 * consumers (fetched on demand, not on the 5s dashboard poll — this data
 * is config-shaped, not live traffic), plus each opened user's own detail
 * (timeline series, usermodel totals) fetched lazily per user id.
 */
export const useConsumersStore = defineStore('consumers', {
  state: () => ({
    data: null as AdminConsumersResponse | null,
    loading: false,
    error: '',
    fetchedAt: null as number | null,
    detail: {} as Record<string, UserDetailState>,
  }),
  actions: {
    /**
     * fetchConsumers fetches GET /admin/api/consumers unconditionally —
     * callers that want the staleness guard use ensureConsumers instead.
     * The `this.loading` in-flight guard (verify-ui-states.md #11 fix)
     * matches stores/dashboard.ts's/stores/events.ts's own identical
     * guard on their refresh() — without it, AccessMatrix.vue's own
     * ErrorState Retry button (wired directly to this action) could fire
     * a second overlapping request on a rapid double-click, free to
     * resolve in either order.
     */
    async fetchConsumers(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      if (this.loading) return
      this.loading = true
      try {
        this.data = await adminFetch<AdminConsumersResponse>('/admin/api/consumers')
        this.error = ''
        this.fetchedAt = Date.now()
      } catch (err) {
        if (isAuthRejection(err)) return
        this.error = messageOf(err)
      } finally {
        this.loading = false
      }
    },
    /** ensureConsumers fetches only when there is no data yet, or the last fetch is older than REFRESH_STALE_MS — ConsumersPage.vue's own onMounted call, so navigating back to the page within a minute does not re-fetch. */
    async ensureConsumers(): Promise<void> {
      if (this.loading) return
      if (this.data && this.fetchedAt !== null && Date.now() - this.fetchedAt < REFRESH_STALE_MS) return
      await this.fetchConsumers()
    },
    /**
     * fetchUserDetail loads one user's timeline (req + cost series) and,
     * only when `userModelStatsEnabled` is true (AdminFeaturesView.
     * userModelStats — GET /admin/api/usage/totals?kind=usermodel 404s
     * otherwise, admin.go), their per-model usermodel totals. Always
     * re-fetches when called (UserDetail.vue calls this on mount and on
     * every global range/span/features change) — the caller decides
     * staleness, this action does not guess.
     *
     * The three requests run via Promise.allSettled, not Promise.all: a
     * 400/404/503 on the usermodel call (or a transient failure on either
     * series call) must never wipe out data from the OTHER two calls that
     * did succeed — each field below keeps its previous value when its own
     * request failed, rather than the whole detail view going blank over
     * one failing endpoint (P1). The usermodel window/span is clamped to
     * day/month (lib/range.ts's dayOrMonthWindow, shared with stores/
     * spend.ts's drilldown and lib/target-columns.ts's targetcaller calls)
     * — that endpoint has no hour bucket at all, so an hour-resolution
     * global range (24h/48h) would otherwise 400 before the server's own
     * 404-when-disabled feature gate even runs (stats_read.go).
     */
    async fetchUserDetail(userId: string, window: HistoryWindow, span: number, userModelStatsEnabled: boolean): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      const existing = this.detail[userId] ?? emptyDetail()
      const requestId = existing.reqId + 1
      // A genuine RANGE change for this same user (verify-ui-states.md #4
      // fix) clears the previous range's own series/totals before the
      // fetch starts — UserDetail.vue's own skeleton condition (`loading
      // && !reqSeries`) then shows a skeleton instead of the WRONG
      // range's numbers sitting under the new heading until the fresh
      // response lands. A SAME-range refetch (re-opening this cached
      // user, a retry, or the featuresUserModelStats-triggered follow-up
      // UserDetail.vue's own watch fires) is NOT a selection change —
      // `existing.window === null` (never fetched before) also takes this
      // branch, which is harmless since there is nothing to clear yet.
      const rangeChanged = existing.window !== null && (existing.window !== window || existing.span !== span)
      this.detail[userId] = {
        ...existing,
        loading: true,
        reqId: requestId,
        reqSeries: rangeChanged ? null : existing.reqSeries,
        costSeries: rangeChanged ? null : existing.costSeries,
        modelTotals: rangeChanged ? null : existing.modelTotals,
        // verify-ui-states-2.md #7: a genuine range change also clears the
        // PREVIOUS range's own per-section errors — without this, an old
        // range's ErrorState (loadState's own "error beats loading"
        // precedence) stayed on screen under the new range's heading until
        // the fresh response landed, instead of the skeleton a cleared
        // reqSeries/costSeries/modelTotals is meant to produce.
        reqError: rangeChanged ? '' : existing.reqError,
        costError: rangeChanged ? '' : existing.costError,
        modelError: rangeChanged ? '' : existing.modelError,
      }

      const clamped = dayOrMonthWindow(window, span)
      const [reqResult, costResult, modelResult] = await Promise.allSettled([
        adminFetch<AdminSeriesResponse>(seriesUrl({ scope: [`user:${userId}`], metric: 'req', window, span })),
        adminFetch<AdminSeriesResponse>(seriesUrl({ scope: [`user:${userId}`], metric: 'cost', window, span })),
        userModelStatsEnabled
          ? adminFetch<AdminTotalsResponse>(
              totalsUrl({ kind: 'usermodel', user: userId, window: clamped.window, span: clamped.span, metrics: ['req', 'cost'] }),
            )
          : Promise.resolve(null as AdminTotalsResponse | null),
      ])

      // A newer fetchUserDetail call for this same user already
      // superseded this one (latest-request-wins) — drop this result.
      if (this.detail[userId]?.reqId !== requestId) return

      const authRejected = [reqResult, costResult, modelResult].some((r) => r.status === 'rejected' && isAuthRejection(r.reason))
      if (authRejected) return

      const modelTotalsUnavailable =
        modelResult.status === 'rejected' && modelResult.reason instanceof AdminApiError && modelResult.reason.status === 404

      const current = this.detail[userId] ?? emptyDetail()
      this.detail[userId] = {
        loading: false,
        // Per-section errors (verify-ui-states.md #4 fix) — see this
        // state's own doc comment. A 404 on the usermodel call is a
        // known, EXPECTED state (the feature is off server-side),
        // surfaced via modelTotalsUnavailable instead, never as an error.
        reqError: settledError(reqResult),
        costError: settledError(costResult),
        modelError: modelTotalsUnavailable ? '' : settledError(modelResult),
        reqSeries: reqResult.status === 'fulfilled' ? reqResult.value : current.reqSeries,
        costSeries: costResult.status === 'fulfilled' ? costResult.value : current.costSeries,
        modelTotals: modelResult.status === 'fulfilled' ? modelResult.value : modelTotalsUnavailable ? null : current.modelTotals,
        modelTotalsUnavailable,
        fetchedAt: Date.now(),
        reqId: requestId,
        window,
        span,
      }
    },
  },
})
