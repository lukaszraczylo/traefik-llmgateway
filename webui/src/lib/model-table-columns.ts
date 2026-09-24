import type { ColumnDef } from '@tanstack/vue-table'
import { h } from 'vue'

import EntityLink from '@/components/EntityLink.vue'
import ModelChip from '@/components/ModelChip.vue'
import { Badge } from '@/components/ui/badge'
import { EMPTY_CELL, optionalColumn, usageMetricColumns } from '@/lib/columns'
import { formatContextWindow, formatCost, formatLatencyMs, formatModelCostHover } from '@/lib/format'
import { formatErrorRatePercent, providerErrorRate } from '@/lib/provider-rate'
import type { AdminCatalogResponse, AdminPerfResponse, AdminUsageModelsResponse, PriceSource } from '@/types/api'

/**
 * ModelCatalogRow (redesign-plan.md section 3.4: "join catalog + ranking +
 * performance") is the Models page's one row shape, built by
 * buildModelCatalogRows below from THREE independent responses: GET
 * /admin/api/catalog (stores/catalog.ts, WP-D — every configured model,
 * regardless of traffic), GET /admin/api/usage/models?detail=1 (the
 * current global range's ranking — zero for a model with no traffic in
 * range), and GET /admin/api/performance?kind=model (fleet p50/p95/
 * attempts/failures — top 50 by req in span, server default; a model
 * outside that top 50 simply carries zero attempts/failures here, same as
 * one with no ranking entry).
 *
 * The catalog is the DRIVING list (one row per catalog model, not per
 * ranking/performance row) — this is a catalog table first, matching
 * AdminCatalogModel's own doc comment ("computed for EVERY catalog model,
 * not just ones with observed traffic").
 */
export interface ModelCatalogRow {
  /** Canonical "provider/model" — the join key across all three source responses. */
  id: string
  /** The bare upstream model id, without the provider prefix (AdminCatalogModel.model). */
  model: string
  providerName: string
  contextTokens?: number
  inputPerMTokUsd?: number
  outputPerMTokUsd?: number
  priceSource: PriceSource
  /** True for a ':free' suffix model — display-only; priceSource carries the real billing truth regardless (types/api.ts's own AdminCatalogModel doc comment). */
  displayFree?: boolean
  aliases: string[]
  requests: number
  tokensIn: number
  tokensOut: number
  costMicroUsd: number
  p50Ms?: number
  p95Ms?: number
  attempts: number
  failures: number
}

/**
 * buildModelCatalogRows joins the three source responses by canonical id.
 * Pure and Vue-free — testable directly, and reusable by stores/models.ts
 * without that store needing to know the join's own internals.
 */
export function buildModelCatalogRows(
  catalog: AdminCatalogResponse,
  ranking: AdminUsageModelsResponse,
  perf: AdminPerfResponse,
): ModelCatalogRow[] {
  const rankingById = new Map(ranking.models.map((m) => [m.id, m]))
  const perfById = new Map((perf.rows ?? []).map((r) => [r.id, r]))

  const rows: ModelCatalogRow[] = []
  for (const provider of catalog.providers) {
    for (const m of provider.models) {
      const usage = rankingById.get(m.id)
      const perfRow = perfById.get(m.id)
      rows.push({
        id: m.id,
        model: m.model,
        providerName: provider.name,
        contextTokens: m.contextTokens,
        inputPerMTokUsd: m.inputPerMTokUsd,
        outputPerMTokUsd: m.outputPerMTokUsd,
        priceSource: m.priceSource,
        displayFree: m.displayFree,
        aliases: m.aliases ?? [],
        requests: usage?.requests ?? 0,
        tokensIn: usage?.tokensIn ?? 0,
        tokensOut: usage?.tokensOut ?? 0,
        costMicroUsd: usage?.costMicroUsd ?? 0,
        p50Ms: perfRow?.p50Ms,
        p95Ms: perfRow?.p95Ms,
        attempts: perfRow?.attempts ?? 0,
        failures: perfRow?.failures ?? 0,
      })
    }
  }
  return rows
}

/** PRICE_SOURCE_LABEL is the price-source Badge's visible text for the three "priced by a rule" sources — 'free'/'unpriced' get their own dedicated badge treatment in priceCell below, never this generic label. */
const PRICE_SOURCE_LABEL: Record<Extract<PriceSource, 'override' | 'builtin' | 'litellm'>, string> = {
  override: 'override',
  builtin: 'builtin',
  litellm: 'litellm',
}

function priceCell(row: ModelCatalogRow) {
  if (row.priceSource === 'unpriced') {
    return h(
      Badge,
      { as: 'span', variant: 'destructive', class: 'font-normal', title: 'No pricing rule matched — billed as $0.' },
      () => 'unpriced',
    )
  }
  if (row.priceSource === 'free') {
    return h(Badge, { as: 'span', variant: 'secondary', class: 'font-normal', title: 'Configured free (modelMeta.free) — priced at $0.' }, () => 'free')
  }
  const known = row.inputPerMTokUsd !== undefined && row.outputPerMTokUsd !== undefined
  // Narrowed to a local BEFORE the h() callback below — TS control-flow
  // narrowing on `row.priceSource` (from the two early returns above)
  // does not cross a closure boundary, so the callback itself still sees
  // the full PriceSource union unless the already-narrowed value is
  // captured into its own variable first.
  const source = row.priceSource as Exclude<PriceSource, 'unpriced' | 'free'>
  return h('span', { class: 'inline-flex flex-wrap items-center gap-1.5' }, [
    h('span', {}, known ? formatModelCostHover(row.inputPerMTokUsd as number, row.outputPerMTokUsd as number) : EMPTY_CELL),
    h(Badge, { as: 'span', variant: 'outline', class: 'font-normal' }, () => PRICE_SOURCE_LABEL[source]),
  ])
}

/**
 * priceSortValue orders unpriced first (nothing to report, worth an
 * operator's attention), then by ascending input price — accessorFn's
 * numeric return, not the rendered text, is what DataTable actually sorts
 * on.
 */
function priceSortValue(row: ModelCatalogRow): number {
  if (row.priceSource === 'unpriced') return -1
  return row.inputPerMTokUsd ?? 0
}

/**
 * modelCatalogColumns builds the Models page's catalog table (redesign-
 * plan.md section 3.4): id, context, price + source badge, free (the
 * ':free' suffix flag — distinct from the price column's own 'free'
 * priceSource badge, see ModelCatalogRow's own doc comment), aliases,
 * req/tokens/cost for the current global range, p50/p95, and error rate.
 * p50/p95 read "—" whenever undefined, regardless of WHY (no observations
 * yet, or admin.stats.latency off entirely) — ModelCatalogTable.vue itself
 * carries the admin.stats.latency hint at the table level (task
 * instruction: "handle latencyEnabled=false with a hint naming
 * admin.stats.latency"), not repeated on every row.
 */
export function modelCatalogColumns(): ColumnDef<ModelCatalogRow, unknown>[] {
  return [
    {
      id: 'id',
      header: 'Model',
      accessorFn: (r) => r.id,
      cell: ({ row }) =>
        h('span', { class: 'inline-flex flex-wrap items-center gap-1.5' }, [
          h(ModelChip, {
            id: row.original.id,
            contextTokens: row.original.contextTokens,
            inputPerMTokUsd: row.original.inputPerMTokUsd,
            outputPerMTokUsd: row.original.outputPerMTokUsd,
          }),
          h(EntityLink, { label: row.original.id, kind: 'model', id: row.original.id }),
        ]),
    },
    optionalColumn<ModelCatalogRow>('context', 'Context', (r) => r.contextTokens, formatContextWindow),
    {
      id: 'price',
      header: 'Price',
      accessorFn: (r) => priceSortValue(r),
      cell: ({ row }) => priceCell(row.original),
    },
    {
      id: 'free',
      header: 'Free',
      accessorFn: (r) => (r.displayFree ? 1 : 0),
      cell: ({ row }) =>
        row.original.displayFree
          ? h(
              Badge,
              { as: 'span', variant: 'secondary', class: 'font-normal', title: "':free' suffix model — display-only, billing follows the Price column's own source" },
              () => 'free',
            )
          : h('span', { class: 'text-muted-foreground' }, EMPTY_CELL),
    },
    {
      id: 'aliases',
      header: 'Aliases',
      enableSorting: false,
      accessorFn: (r) => r.aliases.join(', '),
      cell: ({ row }) => {
        const aliases = row.original.aliases
        if (aliases.length === 0) return h('span', { class: 'text-muted-foreground' }, EMPTY_CELL)
        return h(
          'div',
          { class: 'flex flex-wrap gap-1' },
          aliases.map((a) => h(Badge, { key: a, as: 'span', variant: 'outline', class: 'font-mono font-normal' }, () => a)),
        )
      },
    },
    // reuse-audit.md F9: requests/tokensIn/tokensOut share the shared
    // usageMetricColumns factory; 'cost' stays hand-written below since
    // this table's cost cell has an extra free-priced-model badge case
    // usageMetricColumns' own plain costColumn does not (and should not —
    // PricingHealthTable.vue's identical-shaped cost column has no such
    // badge) carry.
    ...usageMetricColumns<ModelCatalogRow>().filter((c) => c.id !== 'cost'),
    {
      id: 'cost',
      header: 'Cost',
      meta: { align: 'right' },
      accessorFn: (r) => r.costMicroUsd,
      cell: ({ row }) => {
        const r = row.original
        if ((r.priceSource === 'free' || r.displayFree) && r.costMicroUsd === 0) {
          return h(Badge, { as: 'span', variant: 'secondary', class: 'font-normal', title: 'priced at $0 per token' }, () => 'free')
        }
        return formatCost(r.costMicroUsd)
      },
    },
    optionalColumn<ModelCatalogRow>('p50', 'p50', (r) => r.p50Ms, formatLatencyMs, { sortSentinel: -1 }),
    optionalColumn<ModelCatalogRow>('p95', 'p95', (r) => r.p95Ms, formatLatencyMs, { sortSentinel: -1 }),
    {
      id: 'errorRate',
      header: 'Error rate',
      meta: { align: 'right' },
      accessorFn: (r) => providerErrorRate(r.attempts, r.failures) ?? -1,
      cell: ({ row }) => h('span', { class: 'tabular-nums' }, formatErrorRatePercent(providerErrorRate(row.original.attempts, row.original.failures))),
    },
  ]
}
