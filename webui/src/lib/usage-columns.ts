import type { ColumnDef } from '@tanstack/vue-table'
import { h } from 'vue'

import CompactNumber from '@/components/CompactNumber.vue'
import EntityLink from '@/components/EntityLink.vue'
import UsageBar from '@/components/UsageBar.vue'
import { EMPTY_CELL } from '@/lib/columns'
import { monthCostProjection, NOT_ENOUGH_DATA, WILL_EXCEED_TITLE } from '@/lib/forecast'
import { formatCost, formatExactInt, formatLimits } from '@/lib/format'
import { formatLastSeen } from '@/lib/last-seen'
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
 * to blow its own cost/month limit, not just the fleet-wide total. Reuses
 * lib/forecast.ts's own monthCostProjection (F7's one projection+headroom
 * pairing) rather than calling projectMonthEnd/headroom separately here —
 * the same helper ConsumerDirectory.vue's groupCostProjection and lib/
 * usage-csv.ts's own CSV row call.
 *
 * `now` is threaded in from the caller (coordinator brief: "now passed as
 * prop for determinism") rather than read via `new Date()` inside this
 * module — usageColumns() takes it as an optional parameter, defaulting
 * to the real current time for every production call site, so a test can
 * pin it instead.
 */
function costMonthProjColumn(now: Date): ColumnDef<AdminUsageEntryView, unknown> {
  return {
    id: 'costMonthProj',
    header: 'cost/month (proj.)',
    meta: { align: 'right' },
    // Sorting: a not-enough-data-yet row (null projection) has no real
    // figure to compare — it sorts as if projected to 0, the smallest
    // possible spend, rather than being excluded from sorting entirely.
    accessorFn: (entry) => monthCostProjection(entry, now).micros ?? 0,
    cell: ({ row }) => {
      if (row.original.storeDown) return '?'
      const projection = monthCostProjection(row.original, now)
      if (projection.micros === null) return h('span', { class: 'text-muted-foreground' }, NOT_ENOUGH_DATA)
      return h(
        'span',
        {
          class: projection.willExceed ? 'text-destructive' : undefined,
          title: projection.willExceed ? WILL_EXCEED_TITLE : undefined,
        },
        formatCost(projection.micros),
      )
    },
  }
}

/**
 * sourceColumn (Consumers redesign, redesign-plan.md section 3.4) renders
 * a user's directory source — 'inline' (defined directly in the plugin
 * config) or 'file' (loaded from users.file, admin.go: adminConsumerUser.
 * Source) — via the caller-supplied `sourceValue` lookup (ConsumerDirectory.
 * vue joins this row's id against the /admin/api/consumers response, which
 * this module has no fetch access to itself). Only ever added when a
 * caller passes `sourceValue` to usageColumns (see its own doc comment) —
 * a group row (no such concept) and the headless Groups sort toolbar never
 * get this column at all. `sourceValue` returning undefined (a group's
 * nested member row whose name has no matching /consumers entry yet, e.g.
 * mid-fetch) renders an em dash rather than blank, so the column never
 * looks like a rendering bug.
 */
function sourceColumn(sourceValue: (entry: AdminUsageEntryView) => string | undefined): ColumnDef<AdminUsageEntryView, unknown> {
  return {
    id: 'source',
    header: 'Source',
    accessorFn: (entry) => sourceValue(entry) ?? '',
    cell: ({ row }) => h('span', { class: 'text-muted-foreground' }, sourceValue(row.original) ?? EMPTY_CELL),
  }
}

/**
 * lastSeenColumn (Consumers redesign) renders AdminUsageEntryView.lastSeen
 * via lib/last-seen.ts's formatLastSeen — added unconditionally (unlike
 * sourceColumn above): lastSeen is a fleet-wide counter written for BOTH
 * user and group scopes (redesign-plan.md section 1.2's last-seen family:
 * "user + every group scope"), so it is meaningful in every context
 * usageColumns() is used in, not just the flat Users table. A storeDown
 * row is NOT masked as "?" here (unlike every numericColumn) — lastSeen is
 * its own separate counter family (a SET, not the same INCRBY scope
 * counters storeDown actually guards), so a down usage store does not
 * make this value stale/unknown the way it does for req/tok/cost.
 */
function lastSeenColumn(now: Date): ColumnDef<AdminUsageEntryView, unknown> {
  return {
    id: 'lastSeen',
    header: 'Last seen',
    accessorFn: (entry) => entry.lastSeen ?? 0,
    cell: ({ row }) => formatLastSeen(row.original.lastSeen, now),
  }
}

/**
 * usageColumns builds the shared TanStack `ColumnDef` set for one
 * AdminUsageEntryView table. Used by UsageTable.vue (the flat Users list,
 * and — reused unmodified — a group's nested member-user list inside
 * ConsumerDirectory.vue's Groups accordion) and, headlessly (no `<Table>`
 * render, see that component's own doc comment for why), by its Groups
 * sort bar, so both surfaces sort by the exact same column semantics.
 * idLabel/secondaryColumnLabel name the first two columns; secondaryValue
 * reads the second column's per-row value (a user's group name, or a
 * group's member count). `now` (F7) defaults to the real current time —
 * see costMonthProjColumn's own doc comment for why a caller can override
 * it. `sourceValue`, when supplied, inserts sourceColumn right after the
 * secondary column — ConsumerDirectory.vue's flat Users table is the only
 * caller that passes it (see sourceColumn's own doc comment).
 */
export function usageColumns(
  idLabel: string,
  secondaryColumnLabel: string,
  secondaryValue: (entry: AdminUsageEntryView) => string,
  now: Date = new Date(),
  sourceValue?: (entry: AdminUsageEntryView) => string | undefined,
): ColumnDef<AdminUsageEntryView, unknown>[] {
  const columns: ColumnDef<AdminUsageEntryView, unknown>[] = [
    {
      id: 'id',
      header: idLabel,
      accessorFn: (entry) => entry.id,
      // EntityLink (redesign-plan.md section 3.5, replacing ScopeLink):
      // this column only ever renders 'user' kind entries in practice —
      // UsageTable.vue's own doc comment ("users, or a group's member
      // users") — so it links straight to Consumers?user={id}, opening
      // that row's own UserDetail, rather than a Charts scope jump.
      cell: ({ row }) => h(EntityLink, { class: 'font-medium', label: row.original.id, kind: 'user', id: row.original.id }),
    },
    {
      id: 'secondary',
      header: secondaryColumnLabel,
      accessorFn: secondaryValue,
      // 'alphanumeric' (not the default auto-detected sortingFn): this
      // column's value is a plain string everywhere EXCEPT the Groups
      // toolbar, where ConsumerDirectory.vue passes memberCountOf — a
      // numeric count rendered as a string. TanStack's getAutoSortingFn
      // only inspects rows 11+ (flatRows.slice(10)) to decide numeric vs
      // basic, so a groups table with 10 or fewer rows silently fell back
      // to `basic` (plain a > b string comparison), sorting "10" before
      // "9". 'alphanumeric' handles both cases correctly regardless of row
      // count: numeric substrings compare numerically, and it degrades
      // gracefully for the plain-text group-name case (secondaryValue on
      // the Users table).
      sortingFn: 'alphanumeric',
      cell: ({ row }) => h('span', { class: 'text-muted-foreground' }, secondaryValue(row.original)),
    },
  ]
  if (sourceValue) columns.push(sourceColumn(sourceValue))
  columns.push(
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
    costMonthProjColumn(now),
    lastSeenColumn(now),
  )
  return columns
}
