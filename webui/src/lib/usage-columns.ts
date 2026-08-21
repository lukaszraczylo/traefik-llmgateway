import type { ColumnDef } from '@tanstack/vue-table'
import { h } from 'vue'

import { formatCost, formatLimits } from '@/lib/format'
import type { AdminUsageEntryView } from '@/types/api'

/**
 * numericColumn builds one right-aligned, sortable numeric column that
 * reads '?' instead of a stale number while the entry's own scope has a
 * down store — DataTable.vue's cell renderer, not the accessor, does this
 * check, so sorting itself still uses the entry's real underlying value
 * (a storeDown row sorts by its last-known number, same as the rest of
 * the table; only the DISPLAYED text is masked). Mirrors the pre-DataTable
 * UsageTable.vue template's own `entry.storeDown ? '?' : ...` convention.
 */
function numericColumn(
  id: string,
  header: string,
  read: (entry: AdminUsageEntryView) => number,
  format: (n: number) => string = String,
): ColumnDef<AdminUsageEntryView, unknown> {
  return {
    id,
    header,
    accessorFn: read,
    meta: { align: 'right' },
    cell: ({ row }) => (row.original.storeDown ? '?' : format(read(row.original))),
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
 * group's member count).
 */
export function usageColumns(
  idLabel: string,
  secondaryColumnLabel: string,
  secondaryValue: (entry: AdminUsageEntryView) => string,
): ColumnDef<AdminUsageEntryView, unknown>[] {
  return [
    {
      id: 'id',
      header: idLabel,
      accessorFn: (entry) => entry.id,
      cell: ({ row }) => h('span', { class: 'font-medium' }, row.original.id),
    },
    {
      id: 'secondary',
      header: secondaryColumnLabel,
      accessorFn: secondaryValue,
      cell: ({ row }) => h('span', { class: 'text-muted-foreground' }, secondaryValue(row.original)),
    },
    {
      id: 'limits',
      header: 'Limits',
      accessorFn: (entry) => formatLimits(entry.limits),
      cell: ({ row }) => h('span', { class: 'text-muted-foreground' }, formatLimits(row.original.limits)),
    },
    numericColumn('reqMin', 'req/min', (e) => e.requestsPerMinute),
    numericColumn('reqDay', 'req/day', (e) => e.requestsPerDay),
    numericColumn('tokInDay', 'tokIn/day', (e) => e.tokensInPerDay),
    numericColumn('tokOutDay', 'tokOut/day', (e) => e.tokensOutPerDay),
    numericColumn('tokInMonth', 'tokIn/month', (e) => e.tokensInPerMonth),
    numericColumn('tokOutMonth', 'tokOut/month', (e) => e.tokensOutPerMonth),
    numericColumn('costDay', 'cost/day', (e) => e.costPerDayMicroUsd, formatCost),
    numericColumn('costMonth', 'cost/month', (e) => e.costPerMonthMicroUsd, formatCost),
  ]
}
