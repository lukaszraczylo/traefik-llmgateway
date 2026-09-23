import type { ColumnDef } from '@tanstack/vue-table'
import { h } from 'vue'

import CompactNumber from '@/components/CompactNumber.vue'
import ScopeLink from '@/components/ScopeLink.vue'
import UsageBar from '@/components/UsageBar.vue'
import { headroom, monthProgress, projectMonthEnd } from '@/lib/forecast'
import { formatCost, formatExactInt, formatLimits } from '@/lib/format'
import { BUDGET_RATIO_LABEL, budgetRatios } from '@/lib/usage-bars'
import type { AdminUsageEntryView } from '@/types/api'

/**
 * numericColumn builds one right-aligned, sortable numeric column that
 * reads '?' instead of a stale number while the entry's own scope has a
 * down store — DataTable.vue's cell renderer, not the accessor, does this
 * check, so sorting itself still uses the entry's real underlying value
 * (a storeDown row sorts by its last-known number, same as the rest of
 * the table; only the DISPLAYED text is masked). Mirrors the pre-DataTable
 * UsageTable.vue template's own `entry.storeDown ? '?' : ...` convention.
 *
 * compact (default true) renders the value through CompactNumber (SI-
 * style, with the exact figure on title/aria-label) instead of the plain
 * `format`/String path — used for every raw token/request COUNT column
 * below; the two cost columns pass compact:false, since formatCost
 * already shows full precision and compacting a dollar figure is not
 * this feature's concern. The accessorFn (and therefore sorting) always
 * reads the true raw number either way — only the cell's rendered text
 * changes.
 *
 * F1 (usage-vs-limit bars): `budgetId` optionally names WHICH
 * lib/usage-bars.ts BudgetRatio this column illustrates (one of
 * budgetRatios' own ids — "reqMin", "reqDay", "tokDay", "tokMonth",
 * "costDay", "costMonth"). When the row is not storeDown and
 * budgetRatios(row) actually contains that id (i.e. the scope has that
 * limit configured), the cell renders a UsageBar beside the number.
 *
 * P11 review fix (DRY + correctness): this used to take its own
 * `limitOf`/`usedOf` accessor pair and re-derive the USD->micro-USD
 * conversion locally (a dropped usdLimitMicros helper) — a second,
 * independent implementation of exactly what budgetRatios already computes,
 * and one that disagreed with it: budgetRatios gated on the RAW USD value
 * being truthy before rounding (a limit rounding to 0 micro-USD produced an
 * Infinity/NaN ratio there), while this column's own usdLimitMicros rounded
 * first — the two could show a bar in one place, mask it in the other, for
 * the identical scope. Reusing budgetRatios here (now fixed to round-then-
 * skip, matching Go — see that module's own doc comment) makes both paths
 * agree by construction: there is only one ratio computation left, not two.
 *
 * BUDGET_RATIO_LABEL[budgetId] (P10 review fix), not the column's own
 * `header`, backs the bar's accessible label: the token columns'
 * "tokDay"/"tokMonth" ratio combines BOTH directions
 * (budgetRatios' own doc comment), so the bar's aria-label spells out
 * "tokens (in+out)/day" — the column HEADER stays "tokIn/day"/"tokOut/day"
 * (that identity is real and worth keeping visually), but a screen-reader
 * user hearing just "tokIn/day, 1,500 / 2,000" beside a cell showing 500
 * has no other cue that the meter measures the combined total, not that
 * column's own count.
 */
function numericColumn(
  id: string,
  header: string,
  read: (entry: AdminUsageEntryView) => number,
  format: (n: number) => string = String,
  compact = true,
  budgetId?: string,
): ColumnDef<AdminUsageEntryView, unknown> {
  return {
    id,
    header,
    accessorFn: read,
    meta: { align: 'right' },
    cell: ({ row }) => {
      if (row.original.storeDown) return '?'
      const value = read(row.original)
      const valueNode = compact ? h(CompactNumber, { value }) : format(value)
      if (!budgetId) return valueNode
      const ratio = budgetRatios(row.original).find((r) => r.id === budgetId)
      if (!ratio) return valueNode
      const valueText = compact
        ? `${formatExactInt(ratio.used)} / ${formatExactInt(ratio.limit)}`
        : `${format(ratio.used)} / ${format(ratio.limit)}`
      return h('span', { class: 'inline-flex items-center justify-end gap-1.5' }, [
        valueNode,
        h(UsageBar, { ratio: ratio.ratio, label: BUDGET_RATIO_LABEL[budgetId] ?? header, valueText }),
      ])
    },
  }
}

/**
 * rejDayColumn (F4) renders rejectionsPerDay — fleet-wide requests denied
 * by a limit check today (types/api.ts's own doc comment). Not built via
 * numericColumn: it needs a destructive tint whenever the count is
 * positive at all (coordinator brief: "destructive tint when > 0"),
 * independent of any configured limit/bar — a rejection count itself IS
 * the signal, there is nothing to compare it against.
 */
function rejDayColumn(): ColumnDef<AdminUsageEntryView, unknown> {
  return {
    id: 'rejDay',
    header: 'rejected/day',
    accessorFn: (entry) => entry.rejectionsPerDay,
    meta: { align: 'right' },
    cell: ({ row }) => {
      if (row.original.storeDown) return '?'
      const value = row.original.rejectionsPerDay
      return h(CompactNumber, { value, class: value > 0 ? 'text-destructive' : undefined })
    },
  }
}

/**
 * costMonthProjColumn (F7) renders lib/forecast.ts's projected month-end
 * cost for each row, extrapolated from that row's own costPerMonthMicroUsd
 * — the same math CostForecastCard.vue applies to the Total entry, here
 * applied per user/group so an operator can see WHICH scope is on track
 * to blow its own cost/month limit, not just the fleet-wide total.
 *
 * `now` is threaded in from the caller (coordinator brief: "now passed as
 * prop for determinism") rather than read via `new Date()` inside this
 * module — usageColumns() takes it as an optional parameter, defaulting
 * to the real current time for every production call site, so a test can
 * pin it instead. elapsedFraction is computed ONCE per usageColumns() call
 * (not per row) since every row projects against the identical point in
 * the current UTC month.
 */
function costMonthProjColumn(elapsedFraction: number): ColumnDef<AdminUsageEntryView, unknown> {
  return {
    id: 'costMonthProj',
    header: 'cost/month (proj.)',
    meta: { align: 'right' },
    // Sorting: a not-enough-data-yet row (null projection) has no real
    // figure to compare — it sorts as if projected to 0, the smallest
    // possible spend, rather than being excluded from sorting entirely.
    accessorFn: (entry) => projectMonthEnd(entry.costPerMonthMicroUsd, elapsedFraction) ?? 0,
    cell: ({ row }) => {
      if (row.original.storeDown) return '?'
      const projected = projectMonthEnd(row.original.costPerMonthMicroUsd, elapsedFraction)
      if (projected === null) return h('span', { class: 'text-muted-foreground' }, 'not enough data yet')
      const result = headroom(projected, row.original.limits?.costPerMonthUSD)
      return h(
        'span',
        {
          class: result.willExceed ? 'text-destructive' : undefined,
          title: result.willExceed ? 'Projected to exceed the configured cost/month limit' : undefined,
        },
        formatCost(projected),
      )
    },
  }
}

/**
 * usageColumns builds the shared TanStack `ColumnDef` set for one
 * AdminUsageEntryView table. Used by UsageTable.vue (the flat Users list,
 * and — reused unmodified — a group's nested member-user list inside the
 * Usage view's Groups accordion) and, headlessly (no `<Table>` render, see
 * that view's own doc comment for why), by UsageView.vue's Groups sort
 * bar, so both surfaces sort by the exact same column semantics.
 * idLabel/secondaryColumnLabel name the first two columns; secondaryValue
 * reads the second column's per-row value (a user's group name, or a
 * group's member count). `now` (F7) defaults to the real current time —
 * see costMonthProjColumn's own doc comment for why a caller can override
 * it.
 */
export function usageColumns(
  idLabel: string,
  secondaryColumnLabel: string,
  secondaryValue: (entry: AdminUsageEntryView) => string,
  now: Date = new Date(),
): ColumnDef<AdminUsageEntryView, unknown>[] {
  const elapsedFraction = monthProgress(now)
  return [
    {
      id: 'id',
      header: idLabel,
      accessorFn: (entry) => entry.id,
      // F6: ScopeLink jumps to this row's own Charts scope — "user:{id}"
      // or "group:{id}", matching entry.kind exactly (admin.go only ever
      // sets kind to "user"/"group"/"total"; a table row is always the
      // first two, the "total" entry renders separately in the Total
      // card, never as a table row — see UsageView.vue).
      cell: ({ row }) =>
        h(ScopeLink, { class: 'font-medium', label: row.original.id, scope: `${row.original.kind}:${row.original.id}` }),
    },
    {
      id: 'secondary',
      header: secondaryColumnLabel,
      accessorFn: secondaryValue,
      // 'alphanumeric' (not the default auto-detected sortingFn): this
      // column's value is a plain string everywhere EXCEPT the Groups
      // toolbar, where UsageView.vue passes memberCountOf — a numeric count
      // rendered as a string. TanStack's getAutoSortingFn only inspects
      // rows 11+ (flatRows.slice(10)) to decide numeric vs basic, so a
      // groups table with 10 or fewer rows silently fell back to `basic`
      // (plain a > b string comparison), sorting "10" before "9".
      // 'alphanumeric' handles both cases correctly regardless of row
      // count: numeric substrings compare numerically, and it degrades
      // gracefully for the plain-text group-name case (secondaryValue on
      // the Users table).
      sortingFn: 'alphanumeric',
      cell: ({ row }) => h('span', { class: 'text-muted-foreground' }, secondaryValue(row.original)),
    },
    {
      id: 'limits',
      header: 'Limits',
      accessorFn: (entry) => formatLimits(entry.limits),
      cell: ({ row }) => h('span', { class: 'text-muted-foreground' }, formatLimits(row.original.limits)),
    },
    numericColumn('reqMin', 'req/min', (e) => e.requestsPerMinute, String, true, 'reqMin'),
    numericColumn('reqDay', 'req/day', (e) => e.requestsPerDay, String, true, 'reqDay'),
    rejDayColumn(),
    numericColumn('tokInDay', 'tokIn/day', (e) => e.tokensInPerDay, String, true, 'tokDay'),
    numericColumn('tokOutDay', 'tokOut/day', (e) => e.tokensOutPerDay, String, true, 'tokDay'),
    numericColumn('tokInMonth', 'tokIn/month', (e) => e.tokensInPerMonth, String, true, 'tokMonth'),
    numericColumn('tokOutMonth', 'tokOut/month', (e) => e.tokensOutPerMonth, String, true, 'tokMonth'),
    numericColumn('costDay', 'cost/day', (e) => e.costPerDayMicroUsd, formatCost, false, 'costDay'),
    numericColumn('costMonth', 'cost/month', (e) => e.costPerMonthMicroUsd, formatCost, false, 'costMonth'),
    costMonthProjColumn(elapsedFraction),
  ]
}
