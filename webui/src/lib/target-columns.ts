import type { ColumnDef } from '@tanstack/vue-table'
import { h } from 'vue'

import CompactNumber from '@/components/CompactNumber.vue'
import { Badge } from '@/components/ui/badge'
import type { AdminTargetView } from '@/types/api'

/**
 * numericColumn builds one right-aligned, sortable request-counter
 * column, rendered via CompactNumber (SI-style, exact value on title/
 * aria-label — usage-columns.ts's own numericColumn applies the
 * identical treatment). Mirrors usage-columns.ts's own helper of the
 * same name and purpose, but without that one's storeDown '?' masking: a
 * target's counters carry no storeDown flag (limiter.targetUsage,
 * limits.go — a target scope has no limit to protect from a
 * misleadingly-confident zero, unlike a limited user/group scope), so
 * there is nothing to mask here. accessorFn (and therefore sorting)
 * always reads the true raw number.
 */
function numericColumn(id: string, header: string, read: (t: AdminTargetView) => number): ColumnDef<AdminTargetView, unknown> {
  return {
    id,
    header,
    accessorFn: read,
    meta: { align: 'right' },
    cell: ({ row }) => h(CompactNumber, { value: read(row.original) }),
  }
}

/**
 * targetColumns builds the shared TanStack `ColumnDef` set both the MCP
 * servers and the Agents DataTable in TargetsView.vue use (admin.go:
 * adminTargetView — the two tables share one identical row shape). name is
 * plain text (not the ModelChip copy treatment — a target name is not a
 * routable model id); url is muted and truncated with a title attribute
 * for the full value; access renders as a Badge per allowed group, or a
 * muted "All groups" when unrestricted — the exact idiom UsageView.vue's
 * own group Access block already established (Providers/Models/MCP
 * servers/Agents dt/dd pairs there).
 */
export function targetColumns(): ColumnDef<AdminTargetView, unknown>[] {
  return [
    {
      id: 'name',
      header: 'Name',
      accessorFn: (t) => t.name,
      cell: ({ row }) => h('span', { class: 'font-medium' }, row.original.name),
    },
    {
      id: 'url',
      header: 'URL',
      accessorFn: (t) => t.url,
      cell: ({ row }) =>
        h(
          'span',
          { class: 'block max-w-xs truncate text-muted-foreground', title: row.original.url },
          row.original.url,
        ),
    },
    {
      id: 'access',
      header: 'Access',
      enableSorting: false,
      accessorFn: (t) => (t.access ?? []).join(', '),
      cell: ({ row }) => {
        const access = row.original.access
        if (!access?.length) return h('span', { class: 'text-muted-foreground' }, 'All groups')
        return h(
          'div',
          { class: 'flex flex-wrap gap-1.5' },
          access.map((name) => h(Badge, { key: name, as: 'span', variant: 'secondary', class: 'font-mono font-normal' }, () => name)),
        )
      },
    },
    numericColumn('reqMin', 'req/min', (t) => t.counters.requestsPerMinute),
    numericColumn('reqDay', 'req/day', (t) => t.counters.requestsPerDay),
    numericColumn('reqMonth', 'req/month', (t) => t.counters.requestsPerMonth),
  ]
}
