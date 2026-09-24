// F8 (CSV export): builds the actual CSV text for ConsumerDirectory.vue's
// Users/Groups exports (usageCsv, CsvExportButton.vue). Every export
// funnels through lib/csv.ts's toCsv, so quoting/line-ending (and CSV-
// formula-injection neutralisation) behavior is defined in exactly one
// place (that module's own doc comment).
import { toCsv } from '@/lib/csv'
import { monthCostProjection, NOT_ENOUGH_DATA } from '@/lib/forecast'
import { formatLimits } from '@/lib/format'
import { microsToUsd } from '@/lib/usage-bars'
import type { AdminUsageEntryView } from '@/types/api'

/** UsageCsvKind picks the second column's label — "Group" for a Users export (each row's own group membership), "Members" for a Groups export (each row's member count) — mirroring usage-columns.ts's own usageColumns(idLabel, secondaryColumnLabel, ...) call sites in UsageTable.vue/ConsumerDirectory.vue exactly. */
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
  return microsToUsd(micros).toFixed(6)
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
 * ConsumerDirectory.vue-local) — this module has no opinion on what a
 * group's member count is, only how to render the column once given the
 * value.
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
 * follow. The projected month-end cost column reuses lib/forecast.ts's
 * own monthCostProjection (the same helper costMonthProjColumn and
 * ConsumerDirectory.vue's groupCostProjection call) rather than pairing
 * projectMonthEnd by hand — this export only reads its `.micros`, never
 * `.willExceed` (a CSV cell has no styling to apply).
 */
export function usageCsv(
  entries: AdminUsageEntryView[],
  kind: UsageCsvKind,
  secondaryValue: (entry: AdminUsageEntryView) => string,
  now: Date = new Date(),
): string {
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
    'last seen (unix seconds)',
  ]
  const rows = entries.map((entry) => {
    const masked = entry.storeDown
    const projected = masked ? null : monthCostProjection(entry, now).micros
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
      masked ? '?' : projected === null ? NOT_ENOUGH_DATA : csvCostUsd(projected),
      // Never masked — lastSeen is its own SET-based counter family
      // (redesign-plan.md section 1.2), independent of the INCRBY usage
      // counters `storeDown` actually guards (see usage-columns.ts's
      // lastSeenColumn's own doc comment). 0 means never seen, matching
      // AdminUsageEntryView.lastSeen's own "0/omitted" convention.
      entry.lastSeen ?? 0,
    ]
  })
  return toCsv(headers, rows)
}

// modelRankingCsv/modelDetailCsv were removed (P3 item 25, dead code):
// both were the pre-redesign Models tab's own CSV export (ChartsView.vue,
// stores/history.ts), and neither has a caller anywhere in the
// redesigned shell — grep confirms only this file and its own now-deleted
// spec section ever referenced them. The redesigned Models page
// (ModelCatalogTable.vue) has no CSV export of its own.
