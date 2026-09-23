<script setup lang="ts">
import type { ColumnDef } from '@tanstack/vue-table'
import { computed, h } from 'vue'

import CompactNumber from '@/components/CompactNumber.vue'
import CsvExportButton from '@/components/CsvExportButton.vue'
import DataTable from '@/components/DataTable.vue'
import EmptyState from '@/components/EmptyState.vue'
import EntityLink from '@/components/EntityLink.vue'
import ErrorState from '@/components/ErrorState.vue'
import SkeletonTable from '@/components/SkeletonTable.vue'
import TryLongerRangeButton from '@/components/TryLongerRangeButton.vue'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { toCsv } from '@/lib/csv'
import { formatContextWindow, formatCost, formatModelCostHover } from '@/lib/format'
import { loadState } from '@/lib/load-state'
import { joinPricingHealth, pricingHealthOrder } from '@/lib/pricing-health'
import type { PricingHealthRow } from '@/lib/pricing-health'
import type { AdminCatalogModel, AdminUsageModelEntry, PriceSource } from '@/types/api'

/**
 * PricingHealthTable (redesign-plan.md section 3.4) lists every model
 * actually served in the current range, joined with its catalog entry
 * (lib/pricing-health.ts's joinPricingHealth), unpriced first with a
 * destructive badge (pricingHealthOrder) — the row an operator most needs
 * to act on (a served model billing at $0 because no pricing rule
 * matched) is always the first thing they see, before any manual sort.
 *
 * `loading`/`error` (verify-ui-states.md #3/#7 fix): this card had NO
 * loading or error input at all before — `spend.pricingHealthRanking` /
 * `catalog.modelsById` both start empty, so the very first Spend load, and
 * any failed Spend/catalog fetch, rendered the identical "No traffic in
 * this range." a genuinely empty fleet gets, with no way to tell the three
 * apart.
 *
 * `loaded` (verify-ui-states-2.md #2 fix): `hasData` below is "has a fetch
 * completed for the current range" (spend.pricingHealthLoaded), not
 * `rows.length > 0` — the 60s poll (stores/spend.ts's own refresh())
 * flips `loading` true on every tick, and a genuinely empty pricing-health
 * table used to flash a skeleton back on for each of those ticks. The
 * template below still checks `rows.length` directly for EmptyState vs.
 * the real table, the same way UserDetail.vue's own "Models used" table
 * does.
 */
const props = defineProps<{
  models: AdminUsageModelEntry[]
  catalog: Map<string, AdminCatalogModel>
  loading: boolean
  loaded: boolean
  error: string
  /** Renders ErrorState's own "Retry" button — SpendPage.vue's own spend.fetchPricingHealthRanking. */
  onRetry?: () => void | Promise<void>
}>()

const rows = computed<PricingHealthRow[]>(() => pricingHealthOrder(joinPricingHealth(props.models, props.catalog)))

const state = computed(() => loadState({ loading: props.loading, hasData: props.loaded, error: props.error }))

const unpricedCount = computed<number>(() => rows.value.filter((r) => r.priceSource === 'unpriced').length)

/** SOURCE_BADGE_VARIANT mirrors lib/model-table-columns.ts's own priceCell badge treatment (WP-F, ModelCatalogTable.vue) so the two tables read consistently: destructive for unpriced (needs attention), secondary for a deliberate free model, outline for an explicit rule. */
const SOURCE_BADGE_VARIANT: Record<PriceSource, 'destructive' | 'secondary' | 'outline'> = {
  unpriced: 'destructive',
  free: 'secondary',
  override: 'outline',
  builtin: 'outline',
  litellm: 'outline',
}

function sourceCell(row: PricingHealthRow) {
  return h(Badge, { as: 'span', variant: SOURCE_BADGE_VARIANT[row.priceSource], class: 'font-normal' }, () => row.priceSource)
}

function priceCell(row: PricingHealthRow) {
  if (row.inputPerMTokUsd === undefined || row.outputPerMTokUsd === undefined) return h('span', { class: 'text-muted-foreground' }, '—')
  return h('span', {}, formatModelCostHover(row.inputPerMTokUsd, row.outputPerMTokUsd))
}

const columns: ColumnDef<PricingHealthRow, unknown>[] = [
  {
    id: 'id',
    header: 'Model',
    accessorFn: (r) => r.id,
    cell: ({ row }) => h(EntityLink, { label: row.original.id, kind: 'model', id: row.original.id }),
  },
  {
    id: 'source',
    header: 'Source',
    accessorFn: (r) => r.priceSource,
    cell: ({ row }) => sourceCell(row.original),
  },
  {
    id: 'price',
    header: 'Price',
    enableSorting: false,
    cell: ({ row }) => priceCell(row.original),
  },
  {
    id: 'context',
    header: 'Context',
    meta: { align: 'right' },
    accessorFn: (r) => r.contextTokens ?? 0,
    cell: ({ row }) => h('span', { class: 'tabular-nums' }, row.original.contextTokens !== undefined ? formatContextWindow(row.original.contextTokens) : '—'),
  },
  {
    id: 'requests',
    header: 'Requests',
    meta: { align: 'right' },
    accessorFn: (r) => r.requests,
    cell: ({ row }) => h(CompactNumber, { value: row.original.requests }),
  },
  {
    id: 'tokensIn',
    header: 'Tokens in',
    meta: { align: 'right' },
    accessorFn: (r) => r.tokensIn,
    cell: ({ row }) => h(CompactNumber, { value: row.original.tokensIn }),
  },
  {
    id: 'tokensOut',
    header: 'Tokens out',
    meta: { align: 'right' },
    accessorFn: (r) => r.tokensOut,
    cell: ({ row }) => h(CompactNumber, { value: row.original.tokensOut }),
  },
  {
    id: 'cost',
    header: 'Cost',
    meta: { align: 'right' },
    accessorFn: (r) => r.costMicroUsd,
    cell: ({ row }) => formatCost(row.original.costMicroUsd),
  },
]

/**
 * pricingHealthCsv exports exactly the rows on screen (unpriced-first
 * order, or whatever the reader has since re-sorted to — DataTable's own
 * `data` prop is passed through unmodified) as raw, unformatted values
 * (CsvExportButton.vue's own doc comment: "always reflects whatever rows
 * are CURRENTLY visible... never a stale snapshot") — costMicroUsd stays
 * micro-USD, not a pre-formatted "$X.XXXX" string, so the export re-opens
 * cleanly as a spreadsheet numeric column.
 */
function pricingHealthCsv(): string {
  return toCsv(
    ['id', 'priceSource', 'displayFree', 'requests', 'tokensIn', 'tokensOut', 'costMicroUsd', 'contextTokens', 'inputPerMTokUsd', 'outputPerMTokUsd'],
    rows.value.map((r) => [
      r.id,
      r.priceSource,
      r.displayFree,
      r.requests,
      r.tokensIn,
      r.tokensOut,
      r.costMicroUsd,
      r.contextTokens ?? '',
      r.inputPerMTokUsd ?? '',
      r.outputPerMTokUsd ?? '',
    ]),
  )
}
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle class="flex items-center gap-2">
        Pricing health
        <Badge v-if="unpricedCount > 0" variant="destructive" class="font-normal">{{ unpricedCount }} unpriced</Badge>
      </CardTitle>
      <CardDescription>Every model served in range, joined with its billing price source.</CardDescription>
    </CardHeader>
    <CardContent class="flex flex-col gap-3">
      <SkeletonTable v-if="state === 'skeleton'" :rows="5" :cols="columns.length" />
      <ErrorState v-else-if="state === 'error'" :message="error" :on-retry="onRetry" />
      <EmptyState v-else-if="rows.length === 0" title="No traffic in this range.">
        <TryLongerRangeButton />
      </EmptyState>
      <template v-else>
        <DataTable :columns="columns" :data="rows" empty-message="none" />
        <div class="flex justify-end">
          <CsvExportButton filename="pricing-health.csv" :build="pricingHealthCsv" />
        </div>
      </template>
    </CardContent>
  </Card>
</template>
