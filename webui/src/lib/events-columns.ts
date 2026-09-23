import type { ColumnDef } from '@tanstack/vue-table'
import { h } from 'vue'

import { Badge } from '@/components/ui/badge'
import { EVENT_KIND_LABEL, eventKindVariant } from '@/lib/events-filter'
import { formatTimestamp } from '@/lib/format'
import type { AdminEventKind, AdminEventView } from '@/types/api'

/** kindLabel falls back to the raw kind string for a version-skewed server build emitting a kind EVENT_KIND_LABEL does not know — same "still render it" convention AdminEventView.kind's own `| string` union documents. */
function kindLabel(kind: string): string {
  return EVENT_KIND_LABEL[kind as AdminEventKind] ?? kind
}

/** scopeText renders one event's user/group — a user-scoped event never carries a group and vice versa (AdminEventView's own doc comment), so at most one of the two is ever present; an event with neither (a total/all-scope event, e.g. a capacity rejection recorded user-less — DECISIONS Q4) renders an em dash. */
function scopeText(e: AdminEventView): string {
  if (e.user) return `user:${e.user}`
  if (e.group) return `group:${e.group}`
  return '—'
}

/** targetText renders one event's model/provider pair — joined when both are known (an upstream/timeout event names both), just the one that is known otherwise, and an em dash when neither applies (a rate_limit/budget event never carries either). */
function targetText(e: AdminEventView): string {
  const parts = [e.model, e.provider].filter((v): v is string => Boolean(v))
  return parts.length ? parts.join(' → ') : '—'
}

/**
 * eventsColumns builds the Events tab's DataTable ColumnDef set
 * (admin.go/events.go: gatewayEvent — F3). Mirrors target-columns.ts's own
 * shape: a Badge for the classified `kind` column (eventKindVariant), plain
 * muted text for everything free-form (route/message), an em dash for a
 * field this row does not carry rather than a blank cell.
 */
export function eventsColumns(): ColumnDef<AdminEventView, unknown>[] {
  return [
    {
      id: 'time',
      header: 'Time',
      accessorFn: (e) => e.time,
      cell: ({ row }) => h('span', { class: 'text-muted-foreground tabular-nums' }, formatTimestamp(row.original.time)),
    },
    {
      id: 'kind',
      header: 'Kind',
      accessorFn: (e) => e.kind,
      cell: ({ row }) =>
        h(
          Badge,
          { as: 'span', variant: eventKindVariant(row.original.kind), class: 'font-normal' },
          () => kindLabel(row.original.kind),
        ),
    },
    {
      id: 'route',
      header: 'Route',
      accessorFn: (e) => e.route,
      cell: ({ row }) => h('span', { class: 'font-mono text-xs' }, row.original.route),
    },
    {
      id: 'scope',
      header: 'User / group',
      enableSorting: false,
      accessorFn: (e) => e.user ?? e.group ?? '',
      cell: ({ row }) => {
        const text = scopeText(row.original)
        return h('span', { class: text === '—' ? 'text-muted-foreground' : undefined }, text)
      },
    },
    {
      id: 'target',
      header: 'Model / provider',
      enableSorting: false,
      accessorFn: (e) => `${e.model ?? ''} ${e.provider ?? ''}`,
      cell: ({ row }) => {
        const text = targetText(row.original)
        return h('span', { class: text === '—' ? 'text-muted-foreground' : 'font-mono text-xs' }, text)
      },
    },
    {
      id: 'replica',
      header: 'Replica',
      accessorFn: (e) => e.replica,
      cell: ({ row }) => h('span', { class: 'text-muted-foreground' }, row.original.replica),
    },
    {
      id: 'status',
      header: 'Status',
      meta: { align: 'right' },
      accessorFn: (e) => e.status ?? 0,
      cell: ({ row }) =>
        h('span', { class: 'tabular-nums' }, row.original.status !== undefined ? String(row.original.status) : '—'),
    },
    {
      id: 'message',
      header: 'Message',
      enableSorting: false,
      accessorFn: (e) => e.message,
      cell: ({ row }) => h('span', { class: 'text-muted-foreground' }, row.original.message),
    },
  ]
}
