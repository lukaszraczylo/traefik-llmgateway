import { filterModelRows } from '@/lib/model-filter'
import type { ModelCatalogRow } from '@/lib/model-table-columns'

export type PriceStatus = 'priced' | 'unpriced' | 'free' | 'none'

/** Filter value for models with no kind label. */
export const NO_KIND = 'none'

export interface ModelSelection {
  q?: string
  provider?: string
  kind?: string
  price?: string
}

export interface ProviderGroup {
  provider: string
  rows: ModelCatalogRow[]
  requests: number
}

/** Endpoint pseudo-models carry no price, so they are neither priced nor unpriced. */
export function priceStatus(r: ModelCatalogRow): PriceStatus {
  if (r.endpoint) return 'none'
  if (r.priceSource === 'free') return 'free'
  return r.priceSource === 'unpriced' ? 'unpriced' : 'priced'
}

export function selectModels(rows: ModelCatalogRow[], sel: ModelSelection): ModelCatalogRow[] {
  const byText = filterModelRows(rows, sel.q ?? '')
  return byText.filter(
    (r) =>
      (!sel.provider || r.providerName === sel.provider) &&
      (!sel.kind || (sel.kind === NO_KIND ? r.kind === '' : r.kind === sel.kind)) &&
      (!sel.price || priceStatus(r) === sel.price),
  )
}

const byTraffic = (a: ModelCatalogRow, b: ModelCatalogRow): number => b.requests - a.requests || a.id.localeCompare(b.id)

/** One group per provider in first-seen order; rows busiest first. */
export function groupByProvider(rows: ModelCatalogRow[]): ProviderGroup[] {
  const groups = new Map<string, ProviderGroup>()
  for (const r of rows) {
    let g = groups.get(r.providerName)
    if (!g) groups.set(r.providerName, (g = { provider: r.providerName, rows: [], requests: 0 }))
    g.rows.push(r)
    g.requests += r.requests
  }
  for (const g of groups.values()) g.rows.sort(byTraffic)
  return [...groups.values()]
}
