import { pageWindow } from '@/lib/paging'

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
