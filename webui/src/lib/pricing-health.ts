// Pure join/ordering behind PricingHealthTable.vue (redesign-plan.md
// section 3.4): "served models in range joined with catalog; unpriced
// first with destructive badge". Kept out of the component the same way
// every other lib/*.ts module in this feature keeps its own math/ordering
// out of its component.
import type { AdminCatalogModel, AdminUsageModelEntry, PriceSource } from '@/types/api'

/**
 * PricingHealthRow is one PricingHealthTable.vue row: a model actually
 * served in the current range (GET /admin/api/usage/models, detail=1),
 * joined with its catalog entry (GET /admin/api/catalog) for the pricing
 * fields the ranking alone does not carry. `priceSource`/`displayFree`
 * come from the catalog join — the BILLING truth (types/api.ts's
 * PriceSource doc comment) — never re-derived from the ranking's own
 * `free` flag, which reflects display-only relabeling for a ':free'
 * suffix, not necessarily the same thing.
 */
export interface PricingHealthRow {
  id: string
  priceSource: PriceSource
  displayFree: boolean
  requests: number
  tokensIn: number
  tokensOut: number
  costMicroUsd: number
  contextTokens?: number
  inputPerMTokUsd?: number
  outputPerMTokUsd?: number
  aliases?: string[]
}

/**
 * joinPricingHealth joins every served model (`models`, a detail=1 ranking
 * — only entries the server already knows had non-zero usage in range) against
 * its catalog entry by canonical id. A served id absent from `catalog`
 * entirely (a model removed from configuration since it was last used, or
 * the catalog simply has not loaded yet) is SKIPPED — there is no pricing
 * fact to report for it, and fabricating an "unpriced" row for a model
 * that may no longer even exist would be misleading rather than merely
 * incomplete.
 */
export function joinPricingHealth(models: AdminUsageModelEntry[], catalog: Map<string, AdminCatalogModel>): PricingHealthRow[] {
  const rows: PricingHealthRow[] = []
  for (const m of models) {
    const cat = catalog.get(m.id)
    if (!cat) continue
    rows.push({
      id: m.id,
      priceSource: cat.priceSource,
      displayFree: cat.displayFree ?? false,
      requests: m.requests ?? 0,
      tokensIn: m.tokensIn ?? 0,
      tokensOut: m.tokensOut ?? 0,
      costMicroUsd: m.costMicroUsd ?? 0,
      contextTokens: cat.contextTokens,
      inputPerMTokUsd: cat.inputPerMTokUsd,
      outputPerMTokUsd: cat.outputPerMTokUsd,
      aliases: cat.aliases,
    })
  }
  return rows
}

/**
 * PRICING_HEALTH_SOURCE_RANK orders pricingHealthOrder's primary sort key
 * — 'unpriced' first (redesign-plan.md section 3.4: "unpriced first with
 * destructive badge", the row that most needs an operator's attention),
 * then the two explicit/deliberate sources (override, builtin — an
 * operator or the built-in table already priced these correctly), then
 * litellm (a live, less-pinned lookup), then 'free' last (a deliberately
 * zero-cost model — nothing to flag at all).
 */
const PRICING_HEALTH_SOURCE_RANK: Record<PriceSource, number> = {
  unpriced: 0,
  override: 1,
  builtin: 2,
  litellm: 3,
  free: 4,
}

/**
 * pricingHealthOrder sorts joined rows: unpriced-first by priceSource
 * (PRICING_HEALTH_SOURCE_RANK), then by descending requests within each
 * source group — the biggest-traffic unpriced model surfaces first, since
 * that is the one costing the operator the most in unbilled requests.
 * Returns a NEW array; never mutates `rows`.
 */
export function pricingHealthOrder(rows: PricingHealthRow[]): PricingHealthRow[] {
  return [...rows].sort((a, b) => {
    const bySource = PRICING_HEALTH_SOURCE_RANK[a.priceSource] - PRICING_HEALTH_SOURCE_RANK[b.priceSource]
    if (bySource !== 0) return bySource
    return b.requests - a.requests
  })
}
