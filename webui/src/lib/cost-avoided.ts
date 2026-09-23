// Pure math behind CostAvoidedCard.vue (redesign-plan.md section 3.4): how
// much a fleet's FREE-tier traffic would have cost against a real, priced
// reference model, plus which model to default that reference to. Kept out
// of the component the same way lib/forecast.ts/lib/burndown.ts keep their
// own math out of theirs.
import type { AdminCatalogModel, AdminUsageModelEntry } from '@/types/api'

/**
 * FreeUsageRow is the subset of a GET /admin/api/usage/models `detail=1`
 * entry costAvoidedMicros needs — deliberately narrower than
 * AdminUsageModelEntry so the function is testable against plain literals
 * without constructing a full entry.
 */
export interface FreeUsageRow {
  tokensIn: number
  tokensOut: number
}

/**
 * freeRows filters a model ranking down to its FREE rows (detail=1's own
 * `free` flag — a model billed as $0, whether a configured modelMeta.free
 * override or a genuinely unpriced model, types/api.ts's PriceSource doc
 * comment) and reads out just the token counts costAvoidedMicros needs.
 * `tokensIn`/`tokensOut` default to 0 for a detail-less entry (should not
 * happen once the caller has requested detail=1, but this keeps the
 * function total rather than reading `undefined` into arithmetic).
 */
export function freeRows(models: AdminUsageModelEntry[]): FreeUsageRow[] {
  return models.filter((m) => m.free).map((m) => ({ tokensIn: m.tokensIn ?? 0, tokensOut: m.tokensOut ?? 0 }))
}

/**
 * costAvoidedMicros sums, over every free-tier row, what that traffic
 * would have cost at a REAL reference model's own per-token-million rates
 * — the fleet's "cost avoided by routing to free models" figure
 * (redesign-plan.md section 3.4).
 *
 * Unit derivation (why no explicit /1e6 appears despite `refInputPerMTokUsd`
 * being USD PER MILLION tokens, types/api.ts's AdminCatalogModel doc
 * comment): cost_usd = tokensIn * (refInputPerMTokUsd / 1_000_000);
 * cost_micros = cost_usd * 1_000_000 (MICROS_PER_USD, lib/usage-bars.ts) =
 * tokensIn * refInputPerMTokUsd. The two factors of 1_000_000 cancel
 * exactly, so multiplying token counts directly by the $-per-million-token
 * rate already yields micro-USD — the same unit AdminUsageEntryView's own
 * costPerDayMicroUsd/costPerMonthMicroUsd fields use, with no separate
 * scaling step. Rounded once, over the summed total, not per-row, so a sum
 * of many sub-micro rows never underflows to a visibly wrong 0 before
 * accumulating.
 */
export function costAvoidedMicros(rows: FreeUsageRow[], refInputPerMTokUsd: number, refOutputPerMTokUsd: number): number {
  let total = 0
  for (const row of rows) total += row.tokensIn * refInputPerMTokUsd + row.tokensOut * refOutputPerMTokUsd
  return Math.round(total)
}

/** REFERENCE_INELIGIBLE_SOURCES are the catalog priceSource values pickReferenceModel refuses to default to — a "what would this have cost at a real price" comparison is meaningless against another $0 model. */
const REFERENCE_INELIGIBLE_SOURCES = new Set(['free', 'unpriced'])

/**
 * pickReferenceModel defaults the "compare free traffic against" model to
 * the MOST-REQUESTED genuinely-priced model actually served in the current
 * range (redesign-plan.md section 3.4: "reference model picker from priced
 * catalog models, default most-requested paid model in range"). Ranked by
 * `requests` specifically — NOT by whichever metric the Spend page's own
 * breakdown happens to be sorted by — since a cost-ranked or token-ranked
 * list would bias the default toward an expensive or verbose model rather
 * than the one readers actually reach for most. A model missing from
 * `catalog` (not currently in the live registry — a stale ranking entry
 * for a since-removed model) or lacking a `requests` count (a detail-less
 * entry) is skipped. Returns null when nothing in the range qualifies —
 * every served model was itself free/unpriced, or usage/models was empty
 * — the caller then falls back to an explicit picker with no preselection
 * rather than a fabricated default.
 */
export function pickReferenceModel(models: AdminUsageModelEntry[], catalog: Map<string, AdminCatalogModel>): string | null {
  let best: AdminUsageModelEntry | null = null
  for (const m of models) {
    if (m.requests === undefined) continue
    const cat = catalog.get(m.id)
    if (!cat || REFERENCE_INELIGIBLE_SOURCES.has(cat.priceSource)) continue
    if (best === null || m.requests > (best.requests ?? 0)) best = m
  }
  return best?.id ?? null
}
