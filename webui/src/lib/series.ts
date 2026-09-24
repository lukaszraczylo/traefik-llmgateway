// Pure series math shared by every page that renders a GET
// /admin/api/usage/series response through TimeSeriesChart.vue: Spend's
// cost breakdown (by=model/provider/group, redesign-plan.md section 3.4)
// and, per that same section, WP-F's Reliability page ("TimeSeriesChart
// for error rate per provider (lib/series.ts ratioSeries fail/attempt per
// bucket)"). Owned by WP-D (section 4) precisely because Spend needs it
// first, but kept generic — no metric/scope-kind-specific logic — so a
// second page building its own breakdown never grows a duplicate copy
// (vue.md: "if you've written it twice, you owe an abstraction"). Kept out
// of any component/store the same way lib/forecast.ts and lib/usage-bars.ts
// keep their own math out of theirs — vitest's node-environment config
// exercises every function here directly, no component mount required.

import { parseScope } from '@/lib/scope'

/** One AdminSeriesResponse series entry: a scope id and its bucket-aligned points (types/api.ts: AdminSeriesResponse['series'][number]). */
export interface Series {
  scope: string
  points: number[]
}

/**
 * sumSeries totals one series' own points — the ranking key stackTopN
 * sorts by, and the per-bucket contribution `other` subtracts. A missing
 * point (an index beyond `points.length`, which should not happen for a
 * well-formed AdminSeriesResponse where every series is exactly
 * `buckets.length` long) reads as 0 rather than throwing.
 */
function sumSeries(points: number[]): number {
  let total = 0
  for (const p of points) total += p
  return total
}

/**
 * stackTopN returns the N series with the largest total value (sumSeries,
 * descending), for stacking individually in a breakdown chart — Spend's
 * by=model view fetches series for every model the ranking endpoint
 * already reported as non-idle, then charts only the biggest N of them,
 * folding the rest into an "other" bucket via `other` below. Ties break on
 * the ORIGINAL array order (Array.prototype.sort is stable), so two
 * series with an identical total never reorder from one call to the next.
 * `n` beyond `series.length` simply returns every series, sorted; `n <= 0`
 * returns an empty array.
 */
export function stackTopN(series: Series[], n: number): Series[] {
  if (n <= 0) return []
  return [...series].sort((a, b) => sumSeries(b.points) - sumSeries(a.points)).slice(0, n)
}

/**
 * other computes the per-bucket remainder of `totalPoints` once every
 * series in `charted` (Spend's top-N breakdown) is subtracted — the
 * "other" bucket redesign-plan.md section 3.3 describes ("other = total -
 * sum(top)"). Floored at 0 per bucket: a genuine remainder is never
 * negative (every charted series is a strict subset of the total), so a
 * small negative reading can only be a rounding/timing artifact between
 * the two separate HTTP requests that produced `totalPoints` and
 * `charted` — the same "never render a fabricated crossed boundary"
 * discipline lib/format.ts's formatCompactCount already documents for an
 * unrelated flooring case. `charted` may be empty (no breakdown at all —
 * `other` then equals `totalPoints` verbatim, floored).
 */
export function other(totalPoints: number[], charted: Series[]): number[] {
  return totalPoints.map((total, i) => {
    let chartedSum = 0
    for (const s of charted) chartedSum += s.points[i] ?? 0
    return Math.max(0, total - chartedSum)
  })
}

/**
 * sumByProvider groups a set of `model:{canonical}` (or bare canonical)
 * scoped series by their provider prefix (routableModelId's own inverse:
 * the FIRST path segment before the first "/", lib/format.ts's own doc
 * comment on routableModelId spells out why only the first segment counts
 * even when a discovered model id itself contains further slashes) and
 * sums their points per bucket — Spend's by=provider view (redesign-
 * plan.md section 3.3): the /usage/series endpoint has no cost/req/tokin/
 * tokout metric for a `provider:{name}` scope directly (section 1.3.iv's
 * own metric x kind table — provider scope only carries attempt/fail/
 * timeout/fover), so a provider-level cost breakdown can only be DERIVED
 * by summing its own models' series client-side. Returned in descending
 * total order (stackTopN's own convention) — the biggest provider first,
 * both for a stable, useful stacking order and so a caller never has to
 * re-sort.
 */
export function sumByProvider(series: Series[]): Series[] {
  const byProvider = new Map<string, number[]>()
  for (const s of series) {
    const parsed = parseScope(s.scope)
    const id = parsed?.kind === 'model' ? parsed.id : s.scope
    const slash = id.indexOf('/')
    const provider = slash === -1 ? id : id.slice(0, slash)
    const acc = byProvider.get(provider) ?? new Array<number>(s.points.length).fill(0)
    for (let i = 0; i < s.points.length; i++) acc[i] = (acc[i] ?? 0) + (s.points[i] ?? 0)
    byProvider.set(provider, acc)
  }
  const grouped = Array.from(byProvider.entries()).map(([scope, points]) => ({ scope, points }))
  return grouped.sort((a, b) => sumSeries(b.points) - sumSeries(a.points))
}

/**
 * ratioSeries divides `numerator` by `denominator` bucket-for-bucket
 * (Reliability's per-provider error rate, lib/provider-rate.ts's own
 * fleet-wide equivalent for a single snapshot) — null for any bucket
 * where `denominator` is 0 (no attempts that bucket), never a fabricated
 * 0% or a NaN from dividing by zero. Both arrays must already be aligned
 * to the SAME buckets (the caller's responsibility, matching
 * AdminSeriesResponse's own "every series is buckets.length long, index-
 * for-index" contract).
 */
export function ratioSeries(numerator: number[], denominator: number[]): (number | null)[] {
  return denominator.map((d, i) => (d > 0 ? (numerator[i] ?? 0) / d : null))
}
