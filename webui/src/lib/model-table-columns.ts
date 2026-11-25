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
  /** AdminCatalogModel.kind — '' when the model has none. */
  kind: string
  /** AdminCatalogModel.endpoint — a pathBaseUrls pseudo-model: no price, no context. */
  endpoint: boolean
  contextTokens?: number
  inputPerMTokUsd?: number
  outputPerMTokUsd?: number
  priceSource: PriceSource
  /** True for a ':free' suffix model — display-only; priceSource carries the real billing truth regardless (types/api.ts's own AdminCatalogModel doc comment). */
  displayFree?: boolean
  aliases: string[]
  /** Smart routers that target this model. */
  routers?: string[]
  /** False when the range ranking (capped by the server) has no entry: requests is then 0 by absence, not a measured zero. */
  ranked?: boolean
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
        kind: m.kind ?? '',
        endpoint: m.endpoint ?? false,
        contextTokens: m.contextTokens,
        inputPerMTokUsd: m.inputPerMTokUsd,
        outputPerMTokUsd: m.outputPerMTokUsd,
        priceSource: m.priceSource,
        displayFree: m.displayFree,
        aliases: m.aliases ?? [],
        routers: m.routers ?? [],
        ranked: usage !== undefined,
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
