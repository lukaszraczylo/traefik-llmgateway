import { defineStore } from 'pinia'

import { AdminApiError, adminFetch } from '@/lib/api'
import { seriesUrl } from '@/lib/range'
import { ratioSeries } from '@/lib/series'
import { useDashboardStore } from '@/stores/dashboard'
import { useFiltersStore } from '@/stores/filters'
import type { AdminPerfResponse, AdminPerfRow, AdminSeriesResponse, HistoryWindow } from '@/types/api'

/**
 * hourOrDayWindow clamps an arbitrary HistoryWindow/span pair (usually the
 * global filters store's current range) down to the hour/day-only window
 * every Reliability metric accepts (redesign-plan.md section 1.3.iv's
 * metric x kind matrix: provider attempt/fail/timeout/fover and total
 * rej/r402 are BOTH "(h/d)" only — neither family has a month bucket at
 * all, matching their own 48h/35d TTLs, section 1.2). 'hour' and 'day' pass
 * through unchanged; 'month' (the 3mo/6mo/12mo global ranges) falls back to
 * a fixed 30-day day-window default — a deliberate, documented choice
 * rather than an arbitrary derived span, and comfortably within
 * HISTORY_MAX_SPAN.day (35, lib/range.ts).
 */
export function hourOrDayWindow(window: HistoryWindow, span: number): { window: Extract<HistoryWindow, 'hour' | 'day'>; span: number } {
  if (window === 'hour') return { window: 'hour', span }
  if (window === 'day') return { window: 'day', span }
  return { window: 'day', span: 30 }
}

/** One provider's own error-rate series — a 0-1 ratio per bucket, or null where that bucket had zero attempts (never a fabricated 0%, mirroring lib/provider-rate.ts's own providerSuccessRate convention). */
export interface ProviderRatioSeries {
  scope: string
  points: (number | null)[]
}

/**
 * errorRateSeries joins two SAME-SHAPE AdminSeriesResponse results
 * (attempt and fail, fetched with the identical scope list/window/span/
 * offset) by scope, then delegates the actual per-bucket division to
 * WP-D's shared lib/series.ts ratioSeries (redesign-plan.md section 3.4:
 * "TimeSeriesChart for error rate per provider (lib/series.ts ratioSeries
 * fail/attempt per bucket)") — this function's own job is only the
 * scope-matching join AdminSeriesResponse's two independent responses
 * need, not the division itself, so there is exactly one place the ratio
 * math lives. A scope present in `attempts` but missing from `fails`
 * (should not happen — both requests share the same scope list — but
 * defensively handled) is treated as all-zero failures, matching
 * ratioSeries' own "missing point reads as 0" convention.
 */
export function errorRateSeries(attempts: AdminSeriesResponse, fails: AdminSeriesResponse): ProviderRatioSeries[] {
  const failByScope = new Map(fails.series.map((s) => [s.scope, s.points]))
  return attempts.series.map((a) => ({
    scope: a.scope,
    points: ratioSeries(failByScope.get(a.scope) ?? new Array<number>(a.points.length).fill(0), a.points),
  }))
}

/**
 * RELIABILITY_SERIES_COLORS cycles through main.css's four existing
 * --chart-* tokens (assets/main.css is owned by WP-E — this store does not
 * add a fifth) to color the per-provider error-rate/timeout/failover
 * overlay charts: with more than four configured providers, colors repeat
 * — a known, documented limitation rather than a silent one, since adding
 * a genuinely distinct fifth hue is out of this work package's file scope.
 */
export const RELIABILITY_SERIES_COLORS = ['--chart-requests', '--chart-tokens-in', '--chart-tokens-out', '--chart-cost'] as const

/** seriesColor picks one of RELIABILITY_SERIES_COLORS by index, cycling. */
export function seriesColor(index: number): string {
  return RELIABILITY_SERIES_COLORS[index % RELIABILITY_SERIES_COLORS.length]
}

/** One plain (non-ratio) metric series, e.g. failovers/timeouts per provider, or the fleet-wide rej/r402 total. */
export interface CountSeries {
  scope: string
  points: number[]
}

/**
 * useReliabilityStore fetches the Reliability page's own on-demand series
 * (redesign-plan.md section 3.3/3.4) — NOT part of the 5s dashboard poll
 * (Q12 DECISIONS: "new endpoints on demand or 60s"), refetched by the page
 * itself on mount and whenever the global filters range changes. Provider
 * scopes are read from useDashboardStore's already-polled overview
 * (overview.providers) rather than a second providers listing fetch.
 */
export const useReliabilityStore = defineStore('reliability', {
  state: () => ({
    buckets: [] as string[],
    /** The hour/day window hourOrDayWindow actually resolved for the current buckets/series — ReliabilityPage.vue's formatBucketLabel calls need this, not filters.window, whenever a month-range clamp was applied. */
    windowUsed: 'day' as Extract<HistoryWindow, 'hour' | 'day'>,
    /** Per-provider fail/attempt ratio, one entry per configured provider. */
    errorRate: [] as ProviderRatioSeries[],
    /** Per-provider timeout count series (metric `timeout`). */
    timeouts: [] as CountSeries[],
    /** Per-provider failover count series (metric `fover`) — a provider's OWN candidates that were failed away FROM, per accountExtras.failoverFrom's own doc comment (redesign-plan.md section 2b). */
    failovers: [] as CountSeries[],
    /** Fleet-wide total rejections/minute-window-denied series (metric `rej`, scope `total`). */
    rejections: [] as number[],
    /** Fleet-wide total 402 unpriced-refusal series (metric `r402`, scope `total`). */
    unpriced: [] as number[],
    loading: false,
    lastUpdated: null as Date | null,
    error: '',
    /** reqId guards against an out-of-order response overwriting a newer one (latest-request-wins, stores/catalog.ts's own convention) — a range change mid-flight no longer gets silently dropped by an `if (this.loading) return` guard (P2 item 6). */
    reqId: 0,

    // --- selected-provider performance series (N3/redesign-plan.md
    // section 3.3: "performance series=1 for the selected provider") ---
    /** GET /admin/api/performance?kind=provider&series=1's own bucket labels for the currently selected provider — separate from `buckets`/`windowUsed` above since fetchProviderPerf runs independently of fetch() and could in principle resolve against a different window (a fast provider switch mid-range-change). */
    providerPerfBuckets: [] as string[],
    providerPerfWindowUsed: 'day' as Extract<HistoryWindow, 'hour' | 'day'>,
    /** One AdminPerfRow per bucket (its own `id` is the bucket string, not the provider name — AdminPerfResponse's own doc comment) — p50Ms/p95Ms/failures per bucket for ReliabilityPage.vue's selected-provider chart. */
    providerPerfPoints: [] as AdminPerfRow[],
    /** AdminFeaturesView.latencyStats, as this SPECIFIC response reported it — ReliabilityPage.vue already reads dashboard.overview.features.latencyStats to decide whether to fetch at all, but keeping the response's own copy here avoids a stale read if the config toggles between fetches. */
    providerPerfLatencyEnabled: false,
    providerPerfLoading: false,
    providerPerfError: '',
    /** Same latest-request-wins guard as `reqId` above, scoped to fetchProviderPerf's own independent fetch lifecycle. */
    providerPerfReqId: 0,
  }),
  actions: {
    async fetch(): Promise<void> {
      const requestId = ++this.reqId
      this.loading = true
      try {
        const filters = useFiltersStore()
        const dashboard = useDashboardStore()
        const providerNames = (dashboard.overview?.providers ?? []).map((p) => p.name)

        if (providerNames.length === 0) {
          if (requestId !== this.reqId) return
          this.buckets = []
          this.windowUsed = hourOrDayWindow(filters.window, filters.span).window
          this.errorRate = []
          this.timeouts = []
          this.failovers = []
          this.rejections = []
          this.unpriced = []
          this.lastUpdated = new Date()
          this.error = ''
          return
        }

        const { window, span } = hourOrDayWindow(filters.window, filters.span)
        const scopes = providerNames.map((name) => `provider:${name}`)

        const [attemptRes, failRes, timeoutRes, foverRes, rejRes, r402Res] = await Promise.all([
          adminFetch<AdminSeriesResponse>(seriesUrl({ scope: scopes, metric: 'attempt', window, span })),
          adminFetch<AdminSeriesResponse>(seriesUrl({ scope: scopes, metric: 'fail', window, span })),
          adminFetch<AdminSeriesResponse>(seriesUrl({ scope: scopes, metric: 'timeout', window, span })),
          adminFetch<AdminSeriesResponse>(seriesUrl({ scope: scopes, metric: 'fover', window, span })),
          adminFetch<AdminSeriesResponse>(seriesUrl({ scope: ['total'], metric: 'rej', window, span })),
          adminFetch<AdminSeriesResponse>(seriesUrl({ scope: ['total'], metric: 'r402', window, span })),
        ])
        if (requestId !== this.reqId) return

        this.buckets = attemptRes.buckets
        this.windowUsed = window
        this.errorRate = errorRateSeries(attemptRes, failRes)
        this.timeouts = timeoutRes.series
        this.failovers = foverRes.series
        this.rejections = rejRes.series[0]?.points ?? []
        this.unpriced = r402Res.series[0]?.points ?? []
        this.lastUpdated = new Date()
        this.error = ''
      } catch (err) {
        if (requestId !== this.reqId) return
        if (err instanceof AdminApiError && (err.status === 401 || err.status === 403)) return
        this.error = err instanceof Error ? err.message : String(err)
      } finally {
        if (requestId === this.reqId) this.loading = false
      }
    },

    /**
     * fetchProviderPerf (N3 fix, verify-redesign-final.md — plan §3.3's
     * "performance series=1 for the selected provider" was never wired
     * up) reads GET /admin/api/performance?kind=provider&series=1 for
     * ONE provider — per-bucket p50/p95 latency and failure counts,
     * matching this same store's hourOrDayWindow clamp. Only fetches
     * when BOTH `provider` is non-empty (ReliabilityPage.vue passes ''
     * for "All providers") AND admin.stats.latency is on
     * (dashboard.overview.features.latencyStats) — series=1 payload is
     * genuinely useless without latency (attempts/failures alone do not
     * justify a second on-demand round trip Q12 would rather avoid), so
     * this short-circuits to the empty state instead of firing a request
     * neither the page nor the reader will use, mirroring fetch()'s own
     * zero-providers short-circuit above.
     */
    async fetchProviderPerf(provider: string): Promise<void> {
      const requestId = ++this.providerPerfReqId
      const dashboard = useDashboardStore()
      const latencyStatsEnabled = dashboard.overview?.features.latencyStats ?? false

      if (!provider || !latencyStatsEnabled) {
        if (requestId !== this.providerPerfReqId) return
        this.providerPerfBuckets = []
        this.providerPerfPoints = []
        this.providerPerfLatencyEnabled = false
        this.providerPerfError = ''
        this.providerPerfLoading = false
        return
      }

      this.providerPerfLoading = true
      try {
        const filters = useFiltersStore()
        const { window, span } = hourOrDayWindow(filters.window, filters.span)
        const q = new URLSearchParams({ kind: 'provider', window, span: String(span), series: '1' })
        q.append('id', provider)
        const res = await adminFetch<AdminPerfResponse>(`/admin/api/performance?${q.toString()}`)
        if (requestId !== this.providerPerfReqId) return

        this.providerPerfBuckets = res.buckets ?? []
        this.providerPerfWindowUsed = window
        this.providerPerfPoints = res.points ?? []
        this.providerPerfLatencyEnabled = res.latencyEnabled
        this.providerPerfError = ''
      } catch (err) {
        if (requestId !== this.providerPerfReqId) return
        if (err instanceof AdminApiError && (err.status === 401 || err.status === 403)) return
        this.providerPerfError = err instanceof Error ? err.message : String(err)
      } finally {
        if (requestId === this.providerPerfReqId) this.providerPerfLoading = false
      }
    },
  },
})
