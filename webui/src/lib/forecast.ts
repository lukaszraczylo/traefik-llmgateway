// F7 (cost forecast): pure month-to-date -> projected-month-end math for
// CostForecastCard.vue and usage-columns.ts's costMonthProj column. Kept
// out of the component/column the same way lib/usage-bars.ts and lib/
// provider-rate.ts keep their own math out of their components — vitest's
// node-environment config (vite.config.ts's `test` block) exercises it
// directly, no component mount or fake timers required (every function
// here takes `now`/`elapsedFraction` as an explicit argument, never reads
// Date.now() itself).
//
// All month-boundary math is UTC (coordinator brief): costPerMonthMicroUsd
// (admin.go, adminUsageEntryView) accumulates against a UTC calendar
// month bucket (limits.go's windowKey "200601" — same convention
// lib/format.ts's formatBucketLabel documents for the hour/day buckets),
// so projecting against the LOCAL calendar month would silently misalign
// with the figure actually being projected on the last/first day of a
// month in any timezone west or east of UTC.

// usdToMicros (P11 review fix, DRY; reuse-audit.md F8): imported from
// lib/usage-bars.ts, which re-exports lib/format.ts's own one shared
// definition — this module used to declare its own identical MICROS_PER_USD
// copy, one of three (usage-bars.ts, usage-columns.ts) an earlier review
// flagged, then hand-rolled `Math.round(x * MICROS_PER_USD)` here even
// after that consolidation (F8's own gap) instead of calling the shared
// conversion function.
import { usdToMicros } from '@/lib/usage-bars'

/**
 * MIN_PROJECTION_FRACTION is the smallest elapsed-month fraction
 * projectMonthEnd will extrapolate from — below it (coordinator: "null
 * below MIN_PROJECTION_FRACTION"), too little of the month has elapsed for
 * a mtd/elapsed extrapolation to mean anything: a single early request
 * would otherwise project to a wildly overstated month-end figure. 0.02 of
 * a 30-day month is 0.6 days — 14.4 hours; of a 31-day month, 0.62 days —
 * 14.88 hours. Both round to "the first ~14-15 hours of the month",
 * comfortably past the first request of any real deployment's day but
 * still early enough that a genuinely idle scope reports no projection at
 * all rather than a fabricated one.
 */
export const MIN_PROJECTION_FRACTION = 0.02

/**
 * monthProgress returns how far `now` has advanced through ITS OWN UTC
 * calendar month, as a fraction in [0, 1): 0 at the first instant of the
 * month (UTC midnight on the 1st), approaching 1 as the month's last
 * instant nears. Correct across every month length (28/29/30/31 days,
 * including a leap February) because it measures against the ACTUAL
 * elapsed span from this month's start to next month's start, computed
 * with Date.UTC (which itself normalizes an out-of-range month index —
 * `Date.UTC(y, 12, 1)` for December's "next month" already resolves to
 * January 1 of `y + 1`), never a hardcoded days-per-month table.
 */
export function monthProgress(now: Date): number {
  const year = now.getUTCFullYear()
  const month = now.getUTCMonth()
  const monthStart = Date.UTC(year, month, 1)
  const nextMonthStart = Date.UTC(year, month + 1, 1)
  const elapsedMs = now.getTime() - monthStart
  const totalMs = nextMonthStart - monthStart
  return elapsedMs / totalMs
}

/**
 * projectMonthEnd linearly extrapolates a month-to-date micro-USD total to
 * a projected month-end total: `mtdMicros / elapsedFraction`, rounded to
 * the nearest whole micro-USD (matching metrics.go's own usdToMicros
 * round convention, dashboard-plan.md section 0 ground truth). Returns
 * null below MIN_PROJECTION_FRACTION (coordinator decision) — including
 * for `elapsedFraction === 0`, which would otherwise divide by zero — so
 * a caller renders "not enough data yet" instead of a meaningless or
 * infinite figure for the first sliver of a month.
 */
export function projectMonthEnd(mtdMicros: number, elapsedFraction: number): number | null {
  if (elapsedFraction < MIN_PROJECTION_FRACTION) return null
  return Math.round(mtdMicros / elapsedFraction)
}

/**
 * CostHeadroom is projectMonthEnd's projected total compared against a
 * scope's configured costPerMonthUSD limit (LimitsConfig — 0/undefined
 * means unlimited, formatLimits' own convention). `limitMicros`/
 * `remainingMicros` are both null whenever there is nothing to compare
 * against (no limit configured, or no projection yet); `willExceed` is
 * true only when a real projection crosses a real, positive limit.
 */
export interface CostHeadroom {
  limitMicros: number | null
  /** limitMicros - projectedMicros — negative once the projection has crossed the limit. Null under the same conditions limitMicros is null. */
  remainingMicros: number | null
  willExceed: boolean
}

/**
 * headroom compares a projected month-end micro-USD total against an
 * optional costPerMonthUSD limit. `projectedMicros` is null whenever
 * projectMonthEnd itself returned null (too early in the month) —
 * headroom then has nothing to report either, regardless of whether a
 * limit is configured, since "will it exceed" cannot be answered yet.
 * `limitUsd` undefined, zero, or negative all read as "unlimited"
 * (LimitsConfig's own convention — formatLimits' identical `if (limits.x)`
 * gate), never divide-by-zero or a fabricated 0% headroom.
 */
export function headroom(projectedMicros: number | null, limitUsd: number | undefined): CostHeadroom {
  if (projectedMicros === null || limitUsd === undefined || limitUsd <= 0) {
    return { limitMicros: null, remainingMicros: null, willExceed: false }
  }
  const limitMicros = usdToMicros(limitUsd)
  const remainingMicros = limitMicros - projectedMicros
  return { limitMicros, remainingMicros, willExceed: remainingMicros < 0 }
}

/** NOT_ENOUGH_DATA (reuse-audit.md F7) is the ONE "too early in the month to project" message — projectMonthEnd's own null case, shown wherever a projected/run-out figure has nothing to report yet. */
export const NOT_ENOUGH_DATA = 'not enough data yet'

/** WILL_EXCEED_TITLE (reuse-audit.md F7) is the ONE tooltip text for a projection that crosses its own configured limit (headroom's own willExceed). */
export const WILL_EXCEED_TITLE = 'Projected to exceed the configured cost/month limit'

/** MonthCostProjection is monthCostProjection's own result: the projected month-end micro-USD total (null below MIN_PROJECTION_FRACTION, projectMonthEnd's own convention), and whether it crosses the entry's own configured cost/month limit. */
export interface MonthCostProjection {
  micros: number | null
  willExceed: boolean
}

/**
 * monthCostProjection projects one usage entry's month-end cost
 * (projectMonthEnd) and checks it against that SAME entry's own configured
 * cost/month limit (headroom) in one call — the ONE per-row month-end
 * projection every table/detail-grid/CSV export in this panel used to
 * re-derive separately (ConsumerDirectory.vue's groupCostProjection,
 * lib/usage-columns.ts's costMonthProjColumn, lib/usage-csv.ts's own CSV
 * row), each pairing projectMonthEnd + headroom by hand. Takes only the
 * two fields it actually reads, not a full AdminUsageEntryView, so a
 * caller building one from a narrower shape (or a test) never needs to
 * fill in fields this has no use for.
 */
export function monthCostProjection(entry: { costPerMonthMicroUsd: number; limits?: { costPerMonthUSD?: number } }, now: Date): MonthCostProjection {
  const projected = projectMonthEnd(entry.costPerMonthMicroUsd, monthProgress(now))
  if (projected === null) return { micros: null, willExceed: false }
  return { micros: projected, willExceed: headroom(projected, entry.limits?.costPerMonthUSD).willExceed }
}
