import type { ModelCatalogRow } from '@/lib/model-table-columns'

/**
 * filterModelRows narrows the Models page's already-fetched catalog rows
 * (lib/model-table-columns.ts's ModelCatalogRow, built from GET
 * /admin/api/catalog + usage/models + performance) to whichever match a
 * free-text query (redesign-plan.md section 3.1's Models page `q` param) —
 * against the canonical id (ModelChip's own copyable text) OR any of the
 * model's own aliases, case-insensitively. An empty/whitespace-only query
 * is "no filter", returning `rows` unchanged (the pre-redesign
 * filterModelsByPrefix this function replaces made the identical choice).
 */
export function filterModelRows(rows: ModelCatalogRow[], query: string): ModelCatalogRow[] {
  const q = query.trim().toLowerCase()
  if (!q) return rows
  return rows.filter((r) => r.id.toLowerCase().includes(q) || r.aliases.some((a) => a.toLowerCase().includes(q)))
}
