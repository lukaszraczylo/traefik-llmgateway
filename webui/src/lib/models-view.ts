import { filterModelRows } from '@/lib/model-filter'
import { pageWindow } from '@/lib/paging'
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

export interface SortSpec {
  key: string
  direction: 'asc' | 'desc'
}

function compareCells(a: unknown, b: unknown): number {
  if (typeof a === 'number' && typeof b === 'number') return a - b
  return String(a).localeCompare(String(b))
}

/** Sorts the whole list by `sort` (missing values last either way), then returns page `pageIndex` of it. */
export function sortThenSlice<T extends object>(rows: readonly T[], sort: SortSpec | null, pageSize: number, pageIndex: number): T[] {
  let ordered = [...rows]
  if (sort) {
    const sign = sort.direction === 'asc' ? 1 : -1
    const cell = (r: T): unknown => (r as Record<string, unknown>)[sort.key]
    const missing = (v: unknown): boolean => v === null || v === undefined || v === ''
    ordered = ordered.sort((a, b) => {
      const x = cell(a)
      const y = cell(b)
      if (missing(x) || missing(y)) return Number(missing(x)) - Number(missing(y))
      return sign * compareCells(x, y)
    })
  }
  const w = pageWindow(ordered.length, pageSize, pageIndex)
  return ordered.slice(w.from, w.to)
}
