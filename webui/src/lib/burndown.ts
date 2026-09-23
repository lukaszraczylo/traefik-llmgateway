// Pure math behind BurnDownChart.vue (redesign-plan.md section 3.4): a
// cumulative month-to-date spend line against a flat budget reference, and
// the date that spend would cross the budget at the current pace. Kept out
// of the component the same way lib/forecast.ts keeps CostForecastCard.vue's
// own month-end projection math out of it — every function here takes `now`
// as an explicit argument (never reads Date.now() itself), so vitest's
// node-environment config exercises the real calendar-boundary cases
// deterministically.
//
// All month-boundary math is UTC, mirroring lib/forecast.ts's own
// monthProgress (costPerMonthMicroUsd/AdminSeriesResponse's day buckets
// both accumulate against a UTC calendar month — limits.go's windowKey
// "200601"/"20060102" — so projecting against the LOCAL calendar month
// would silently misalign on the first/last day of a month in any
// non-UTC timezone).
import { monthProgress } from '@/lib/forecast'

const MS_PER_DAY = 24 * 60 * 60 * 1000

/**
 * daysInMonthUTC returns how many days are in `now`'s own UTC calendar
 * month (28-31) — correct for every month length, including a leap
 * February, via the same Date.UTC month-rollover normalization
 * monthProgress (lib/forecast.ts) already relies on, never a hardcoded
 * days-per-month table.
 */
function daysInMonthUTC(now: Date): number {
  const year = now.getUTCFullYear()
  const month = now.getUTCMonth()
  return (Date.UTC(year, month + 1, 1) - Date.UTC(year, month, 1)) / MS_PER_DAY
}

/**
 * currentMonthDaySpan is how many day buckets (1..31) `now`'s own UTC
 * calendar month has elapsed so far, INCLUDING today — the `span` the
 * spend store requests from GET /admin/api/usage/series for the burn-down
 * line (redesign-plan.md section 3.3: "burn-down current-month day series
 * span=dayOfMonth"), so a day-3-of-the-month poll asks for exactly 3
 * buckets, never the whole month's worth of not-yet-existing future ones.
 */
export function currentMonthDaySpan(now: Date): number {
  return now.getUTCDate()
}

/**
 * cumulative turns a bucket-per-day points array (day-window
 * AdminSeriesResponse series, oldest first) into a running total —
 * BurnDownChart.vue's own "spend so far" line, since the series endpoint
 * itself reports each day's OWN cost, not a running sum.
 */
export function cumulative(points: number[]): number[] {
  let sum = 0
  return points.map((p) => (sum += p))
}

/**
 * MIN_ELAPSED_DAYS is runOutDate's own extrapolation floor, mirroring
 * lib/forecast.ts's MIN_PROJECTION_FRACTION for the identical reason: the
 * first day of a month is too little data for a daily-average run rate to
 * mean anything, so runOutDate reports "not enough data yet" (null) below
 * it rather than a wildly overstated projection from a single day's spend.
 */
export const MIN_ELAPSED_DAYS = 1

/**
 * runOutDate projects the date month-to-date spend would cross
 * `limitMicros`, extrapolating from the MTD average daily rate
 * (mtdMicros / elapsed days so far) — redesign-plan.md section 3.4:
 * "run-out date lib/burndown.ts runOutDate(mtdMicros, limitMicros, now) at
 * MTD average daily rate". Mirrors lib/forecast.ts's headroom() null
 * conventions: null when there is no budget to compare against
 * (`limitMicros <= 0`), and null when there is too little of the month
 * elapsed to extrapolate from (MIN_ELAPSED_DAYS) or the rate so far is 0
 * (spend has not started — "never" is not representable as a date, so
 * this reports "no forecast" rather than a fabricated far-future one).
 * Already-exceeded budgets return `now` itself (the run-out already
 * happened, as of this read) rather than a nonsensical past-vs-future date
 * from continuing the division.
 */
export function runOutDate(mtdMicros: number, limitMicros: number, now: Date): Date | null {
  if (limitMicros <= 0) return null
  if (mtdMicros >= limitMicros) return new Date(now.getTime())

  const elapsedDays = monthProgress(now) * daysInMonthUTC(now)
  if (elapsedDays < MIN_ELAPSED_DAYS) return null

  const dailyRateMicros = mtdMicros / elapsedDays
  if (dailyRateMicros <= 0) return null

  const remainingMicros = limitMicros - mtdMicros
  const daysToExhaust = remainingMicros / dailyRateMicros
  return new Date(now.getTime() + daysToExhaust * MS_PER_DAY)
}
