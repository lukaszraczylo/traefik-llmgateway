// reuse-audit.md F9: generic TanStack ColumnDef factories for the numeric-
// metric columns that repeat across every usage-shaped table in this panel
// (lib/model-table-columns.ts, components/PricingHealthTable.vue, lib/
// target-columns.ts) — a right-aligned, sortable count column rendered via
// CompactNumber, a right-aligned cost column rendered via formatCost, and
// an "undefined reads as a placeholder" optional numeric column (p50/p95,
// context window). Each factory is generic over the row type `T`: the
// duplication these replace was never about the ROW SHAPE (every caller
// already has its own row interface), only about the identical id/header/
// accessor/cell wiring built on top of it.
import type { ColumnDef } from '@tanstack/vue-table'
import { h } from 'vue'

import CompactNumber from '@/components/CompactNumber.vue'
import { formatCost } from '@/lib/format'

/** EMPTY_CELL is the ONE "no value to show" placeholder every table cell in this panel renders instead of a blank — model-table-columns.ts, PricingHealthTable.vue, usage-columns.ts, and events-columns.ts each used to redeclare the same '—' literal. */
export const EMPTY_CELL = '—'

/** compactColumn builds one right-aligned, sortable numeric column rendered via CompactNumber (SI-style, exact value on title/aria-label) — the plain case with no storeDown masking or budget-bar overlay (usage-columns.ts's own richer numericColumn keeps those, see its own doc comment for why it is not built on this one). */
export function compactColumn<T>(id: string, header: string, read: (row: T) => number): ColumnDef<T, unknown> {
  return {
    id,
    header,
    meta: { align: 'right' },
    accessorFn: (r) => read(r),
    cell: ({ row }) => h(CompactNumber, { value: read(row.original) }),
  }
}

/** costColumn builds one right-aligned, sortable micro-USD column rendered via formatCost — the plain case (a caller with its own special-case cell, e.g. model-table-columns.ts's free-priced-model badge, keeps its own hand-written column instead). */
export function costColumn<T>(id: string, header: string, read: (row: T) => number): ColumnDef<T, unknown> {
  return {
    id,
    header,
    meta: { align: 'right' },
    accessorFn: (r) => read(r),
    cell: ({ row }) => formatCost(read(row.original)),
  }
}

/**
 * optionalColumn builds one right-aligned, sortable column for a value
 * that may be undefined (no observations yet, a feature switched off) —
 * EMPTY_CELL for the cell, `sortSentinel` (default 0) for the accessor so
 * an "undefined" row still sorts somewhere deterministic (p50/p95 pass -1,
 * so "no data" sorts below a genuine 0ms reading rather than tying with
 * it; context window's own 0 default is fine as-is, nothing sorts below a
 * real context size of 0).
 */
export function optionalColumn<T>(
  id: string,
  header: string,
  read: (row: T) => number | undefined,
  format: (n: number) => string,
  opts?: { sortSentinel?: number },
): ColumnDef<T, unknown> {
  const sentinel = opts?.sortSentinel ?? 0
  return {
    id,
    header,
    meta: { align: 'right' },
    accessorFn: (r) => read(r) ?? sentinel,
    cell: ({ row }) => {
      const value = read(row.original)
      return h('span', { class: 'tabular-nums' }, value !== undefined ? format(value) : EMPTY_CELL)
    },
  }
}

/** UsageMetricRow is the field set usageMetricColumns reads — every row shape in this panel that carries per-model/per-scope requests/tokens/cost (ModelCatalogRow, PricingHealthRow) already has exactly these four fields under these exact names. */
export interface UsageMetricRow {
  requests: number
  tokensIn: number
  tokensOut: number
  costMicroUsd: number
}

/** usageMetricColumns builds the requests/tokensIn/tokensOut/cost column set every usage-ranked model table in this panel repeats (reuse-audit.md F9) — a caller with its own special-case cost cell (e.g. a free-priced-model badge) omits 'cost' here and appends its own instead. */
export function usageMetricColumns<T extends UsageMetricRow>(): ColumnDef<T, unknown>[] {
  return [
    compactColumn<T>('requests', 'Requests', (r) => r.requests),
    compactColumn<T>('tokensIn', 'Tokens in', (r) => r.tokensIn),
    compactColumn<T>('tokensOut', 'Tokens out', (r) => r.tokensOut),
    costColumn<T>('cost', 'Cost', (r) => r.costMicroUsd),
  ]
}
