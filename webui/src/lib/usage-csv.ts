// F8 (CSV export): builds the actual CSV text for the Usage tab's Users/
// Groups exports (usageCsv) and, imported unmodified by WP-B2's Models
// tab, the model-ranking export (modelRankingCsv). Both funnel through
// lib/csv.ts's toCsv, so quoting/line-ending behavior is defined in
// exactly one place (that module's own doc comment).
import { toCsv } from '@/lib/csv'
import { monthProgress, projectMonthEnd } from '@/lib/forecast'
import { formatLimits } from '@/lib/format'
import { MICROS_PER_USD } from '@/lib/usage-bars'
import type { AdminUsageEntryView, AdminUsageModelEntry } from '@/types/api'

/** UsageCsvKind picks the second column's label — "Group" for a Users export (each row's own group membership), "Members" for a Groups export (each row's member count) — mirroring usage-columns.ts's own usageColumns(idLabel, secondaryColumnLabel, ...) call sites in UsageTable.vue/UsageView.vue exactly. */
export type UsageCsvKind = 'users' | 'groups'

const USAGE_CSV_SECONDARY_LABEL: Record<UsageCsvKind, string> = {
  users: 'Group',
  groups: 'Members',
}

/**
 * csvCostUsd (P6 review fix) renders a micro-USD integer as a PLAIN
 * decimal USD number: 6 decimal places, no "$" prefix, no thousands
 * separator. The on-screen table's own formatCost ("$1.2345", 4 decimals,
 * `en-US`-locale-shaped) is a DISPLAY convention, not a machine-readable
 * one — a spreadsheet importer in a non-en-US locale can misparse a
 * currency-symbol-prefixed string entirely, and 4 decimals silently drops
 * any sub-$0.0001 precision the underlying micro-USD integer actually
 * carries. 6 decimals is exactly the precision 1 micro-USD (the smallest
 * unit every cost field on this view is denominated in) can express:
 * 1 micro-USD = $0.000001.
 */
function csvCostUsd(micros: number): string {
  return (micros / MICROS_PER_USD).toFixed(6)
}

/**
 * usageCsv renders one AdminUsageEntryView[] table (Users, or the Groups
 * list) as CSV text, mirroring usage-columns.ts's own column set exactly
 * so the exported file matches what the on-screen table shows: Name, the
 * kind-dependent secondary column, Limits, then every numeric column in
 * the same left-to-right order the table renders them in, including
 * rejected/day (F4) between req/day and the token columns, and the
 * projected month-end cost (F7, P6 review fix — this used to omit it
 * despite claiming to mirror the table's column set exactly) last.
 *
 * `secondaryValue` is the same per-row accessor usageColumns() itself
 * takes (groupNameOf for Users, memberCountOf for Groups, both
 * UsageView.vue-local) — this module has no opinion on what a group's
 * member count is, only how to render the column once given the value.
 *
 * Every numeric cell is a PLAIN machine-readable number (csvCostUsd above
 * for the three cost columns, the raw integer — no formatExactInt
 * thousands-grouping — for every count column; `toCsv`'s own `csvField`
 * stringifies a raw number for us): P6 review fix, the SAME reasoning
 * csvCostUsd's own doc comment gives for cost.
 *
 * A storeDown row exports "?" for every counter, matching the table's own
 * masking convention (usage-columns.ts's numericColumn) — an exported
 * spreadsheet must never present a stale/unknown number as if it were
 * real.
 *
 * `now` (F7, default the real current time) is threaded in rather than
 * read via `new Date()` inside this module — the same "caller can pin it
 * under test" convention costMonthProjColumn/CostForecastCard already
 * follow.
 */
export function usageCsv(
  entries: AdminUsageEntryView[],
  kind: UsageCsvKind,
  secondaryValue: (entry: AdminUsageEntryView) => string,
  now: Date = new Date(),
): string {
  const elapsedFraction = monthProgress(now)
  const headers = [
    'Name',
    USAGE_CSV_SECONDARY_LABEL[kind],
    'Limits',
    'req/min',
    'req/day',
    'rejected/day',
    'tokIn/day',
    'tokOut/day',
    'tokIn/month',
    'tokOut/month',
    'cost/day (USD)',
    'cost/month (USD)',
    'cost/month (proj., USD)',
  ]
  const rows = entries.map((entry) => {
    const masked = entry.storeDown
    const projected = masked ? null : projectMonthEnd(entry.costPerMonthMicroUsd, elapsedFraction)
    return [
      entry.id,
      secondaryValue(entry),
      formatLimits(entry.limits),
      masked ? '?' : entry.requestsPerMinute,
      masked ? '?' : entry.requestsPerDay,
      masked ? '?' : entry.rejectionsPerDay,
      masked ? '?' : entry.tokensInPerDay,
      masked ? '?' : entry.tokensOutPerDay,
      masked ? '?' : entry.tokensInPerMonth,
      masked ? '?' : entry.tokensOutPerMonth,
      masked ? '?' : csvCostUsd(entry.costPerDayMicroUsd),
      masked ? '?' : csvCostUsd(entry.costPerMonthMicroUsd),
      masked ? '?' : projected === null ? 'not enough data yet' : csvCostUsd(projected),
    ]
  })
  return toCsv(headers, rows)
}

/**
 * modelRankingCsv renders the Models tab's current ranking (WP-B2,
 * ChartsView.vue) as CSV text: one row per ranked model, `id` (the
 * canonical "provider/model" that served the traffic — AdminUsageModelEntry's
 * own doc comment, types/api.ts) and its `value` for the ranked metric.
 * `metricLabel`/`windowLabel`/`span` are the caller's own already-resolved
 * DISPLAY strings/number (e.g. MODEL_METRIC_LABEL[modelMetric],
 * WINDOW_LABEL[window], WINDOW_SPAN[window] — stores/history.ts, WP-B2),
 * not re-derived here: this module stays independent of WP-B2's store, and
 * the second header column names exactly what was ranked and over what
 * span, so the exported file is self-describing without a caller needing
 * to cross-reference the filename.
 *
 * `isCostMetric` (P6 review fix): the cost-metric ranking's `value` is raw
 * micro-USD — the on-screen chart/CSV both used to export it as-is, with
 * no unit stated anywhere in the header. The caller states whether the
 * CURRENTLY ranked metric is cost (`history.modelMetric === 'cost'`); when
 * true, the header gains an explicit " (USD)" unit suffix and every value
 * converts to a plain decimal USD number via the same csvCostUsd
 * convention usageCsv uses above. Every OTHER metric (requests, tokens in/
 * out) is already a plain raw integer with no unit ambiguity, so this
 * defaults to false and changes nothing for them.
 */
export function modelRankingCsv(
  models: AdminUsageModelEntry[],
  metricLabel: string,
  windowLabel: string,
  span: number,
  isCostMetric = false,
): string {
  const unit = isCostMetric ? ' (USD)' : ''
  const headers = ['Model', `${metricLabel}${unit} (${windowLabel}, span ${span})`]
  const rows = models.map((m) => [m.id, isCostMetric ? csvCostUsd(m.value) : m.value])
  return toCsv(headers, rows)
}
