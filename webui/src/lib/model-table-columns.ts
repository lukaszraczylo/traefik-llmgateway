import type { ColumnDef } from '@tanstack/vue-table'
import { h } from 'vue'

import CompactNumber from '@/components/CompactNumber.vue'
import ModelChip from '@/components/ModelChip.vue'
import ScopeLink from '@/components/ScopeLink.vue'
import { Badge } from '@/components/ui/badge'
import { formatCost } from '@/lib/format'
import type { AdminUsageModelEntry } from '@/types/api'

/**
 * modelColumn renders a ranked row's id as a ModelChip (the same copyable
 * chip ProvidersView.vue's own per-model list renders) beside a ScopeLink
 * that jumps to Charts scoped to this exact model — the identical pair
 * ProvidersView.vue already builds from `routableModelId(provider, model)`,
 * reused here rather than a third hand-rolled copy (vue.md: "if you've
 * written it twice, you owe an abstraction"). No routableModelId join is
 * needed: `id` is already the canonical "provider/model" form
 * (AdminUsageModelEntry's own doc comment, types/api.ts).
 */
function modelColumn(): ColumnDef<AdminUsageModelEntry, unknown> {
  return {
    id: 'model',
    header: 'Model',
    accessorFn: (entry) => entry.id,
    cell: ({ row }) =>
      h('span', { class: 'inline-flex flex-wrap items-center gap-1.5' }, [
        h(ModelChip, { id: row.original.id }),
        h(ScopeLink, { label: row.original.id, scope: `model:${row.original.id}` }),
      ]),
  }
}

/**
 * numericDetailColumn builds one right-aligned, sortable count column
 * reading a `detail=1`-only field (requests/tokensIn/tokensOut —
 * AdminUsageModelEntry's own doc comment): undefined only when the caller
 * fetched without `detail=1`, which stores/history.ts's fetchModelRanking
 * never does any more (free-models-plan.md) — `?? 0` covers that
 * theoretical case defensively, the same fallback numericColumn
 * (usage-columns.ts) applies for its own optional fields, never a silent
 * mis-sort of a genuine 0 count. Rendered via CompactNumber, mirroring
 * every other count column in this panel (usage-columns.ts,
 * target-columns.ts).
 */
function numericDetailColumn(
  id: string,
  header: string,
  read: (entry: AdminUsageModelEntry) => number | undefined,
): ColumnDef<AdminUsageModelEntry, unknown> {
  return {
    id,
    header,
    accessorFn: (entry) => read(entry) ?? 0,
    meta: { align: 'right' },
    cell: ({ row }) => h(CompactNumber, { value: read(row.original) ?? 0 }),
  }
}

/**
 * costColumn renders a "free" Badge (accessible text "free" —
 * free-models-plan.md's own explicit requirement) in place of the dollar
 * figure whenever `entry.free` is true and no cost was recorded, plain
 * formatCost otherwise.
 * Sorting is unaffected by the badge: the accessorFn always reads the raw
 * costMicroUsd number (0 for a free model, same as any other zero cost),
 * so a cost-descending sort still puts every free model at the bottom
 * exactly where a real $0 belongs — only the DISPLAYED cell changes.
 */
function costColumn(): ColumnDef<AdminUsageModelEntry, unknown> {
  return {
    id: 'cost',
    header: 'Cost',
    accessorFn: (entry) => entry.costMicroUsd ?? 0,
    meta: { align: 'right' },
    cell: ({ row }) => {
      const entry = row.original
      // A model marked free now may still carry cost recorded before it was
      // marked free: show the real dollars then, not the badge.
      if (entry.free && (entry.costMicroUsd ?? 0) === 0) {
        return h(Badge, { as: 'span', variant: 'secondary', class: 'font-normal', title: 'priced at $0 per token' }, () => 'free')
      }
      return formatCost(entry.costMicroUsd ?? 0)
    },
  }
}

/**
 * modelTableColumns builds the Models tab's ranking table (free-models-
 * plan.md, UI section): Model, Requests, Tokens in, Tokens out, Cost. Every
 * numeric column reads its OWN `detail=1` field independently of whichever
 * metric the ranking is currently sorted by (stores/history.ts's
 * modelMetric) — the table always shows all four figures per row, so a
 * reader ranking by cost can still see a model's request count (and vice
 * versa) without switching the ranking metric. The server already returns
 * the ranking sorted by the ranked metric descending (ModelUsageChart.vue's
 * own doc comment), and DataTable.vue's uncontrolled sort state starts
 * empty, so that server order — "current metric desc" — is exactly what
 * renders before any header is clicked; no explicit initial `sorting` prop
 * is needed here.
 */
export function modelTableColumns(): ColumnDef<AdminUsageModelEntry, unknown>[] {
  return [
    modelColumn(),
    numericDetailColumn('requests', 'Requests', (e) => e.requests),
    numericDetailColumn('tokensIn', 'Tokens in', (e) => e.tokensIn),
    numericDetailColumn('tokensOut', 'Tokens out', (e) => e.tokensOut),
    costColumn(),
  ]
}
