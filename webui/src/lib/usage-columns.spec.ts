import { isVNode } from 'vue'
import { describe, expect, it } from 'vitest'

import { usageColumns } from './usage-columns'
import type { AdminUsageEntryView } from '@/types/api'

/**
 * Compact-number rendering (feature v0.23 addendum) must land ONLY in
 * each numeric column's cell renderer, never in its accessorFn — the
 * accessorFn is what getSortedRowModel actually sorts by, so a compacted
 * STRING there ("15.0M") would sort lexicographically instead of
 * numerically. These tests exercise the ColumnDef objects directly
 * (no table mount needed) to pin that separation.
 */
function testEntry(overrides: Partial<AdminUsageEntryView> = {}): AdminUsageEntryView {
  return {
    kind: 'user',
    id: 'alice',
    requestsPerMinute: 5,
    requestsPerDay: 15_000_000,
    tokensInPerDay: 999,
    tokensOutPerDay: 0,
    tokensInPerMonth: 0,
    tokensOutPerMonth: 0,
    costPerDayMicroUsd: 1_234_500,
    costPerMonthMicroUsd: 0,
    ...overrides,
  }
}

/**
 * accessorFnOf narrows a plain `ColumnDef` (a union of accessor/display
 * column shapes TypeScript does not narrow automatically) down to the
 * `accessorFn` every numericColumn-built column in this test file
 * actually carries — a thin, test-local cast, not a production type.
 */
function accessorFnOf(col: unknown): (row: AdminUsageEntryView, index: number) => unknown {
  return (col as { accessorFn: (row: AdminUsageEntryView, index: number) => unknown }).accessorFn
}

describe('usageColumns: sorting stays on raw numeric values', () => {
  const columns = usageColumns('Name', 'Group', () => 'g')

  it('a count column (reqDay) accessorFn returns the raw number, not a compact string', () => {
    const col = columns.find((c) => c.id === 'reqDay')
    expect(accessorFnOf(col)(testEntry(), 0)).toBe(15_000_000)
  })

  it('a count column cell renders a CompactNumber vnode, not the raw number as text', () => {
    const col = columns.find((c) => c.id === 'reqDay')
    // ColumnDef's cell can be a string or a render function; every
    // numericColumn call here uses the function form.
    const cell = col?.cell as (ctx: { row: { original: AdminUsageEntryView } }) => unknown
    const rendered = cell({ row: { original: testEntry() } })
    expect(isVNode(rendered)).toBe(true)
  })

  it('a cost column cell still renders plain formatted text (compact:false), not a CompactNumber vnode', () => {
    const col = columns.find((c) => c.id === 'costDay')
    const cell = col?.cell as (ctx: { row: { original: AdminUsageEntryView } }) => unknown
    const rendered = cell({ row: { original: testEntry() } })
    expect(isVNode(rendered)).toBe(false)
    expect(rendered).toBe('$1.2345')
  })

  it('a storeDown row still masks every count cell as "?" regardless of compact rendering', () => {
    const col = columns.find((c) => c.id === 'tokInDay')
    const cell = col?.cell as (ctx: { row: { original: AdminUsageEntryView } }) => unknown
    const rendered = cell({ row: { original: testEntry({ storeDown: true }) } })
    expect(rendered).toBe('?')
  })

  it('sorting a set of entries by reqDay orders numerically, not lexicographically (the bug this separation prevents)', () => {
    const col = columns.find((c) => c.id === 'reqDay')
    const accessor = accessorFnOf(col)
    const entries = [testEntry({ id: 'a', requestsPerDay: 15_000_000 }), testEntry({ id: 'b', requestsPerDay: 999 }), testEntry({ id: 'c', requestsPerDay: 12_000 })]
    const sorted = [...entries].sort((x, y) => (accessor(x, 0) as number) - (accessor(y, 0) as number))
    expect(sorted.map((e) => e.id)).toEqual(['b', 'c', 'a'])
    // A lexicographic sort on the COMPACT TEXT would instead order
    // "1,000+" digits, "12k", "15.0M" as strings ("12k" < "15.0M" <
    // "999" alphabetically) — the wrong order, which is exactly why
    // accessorFn must stay numeric.
  })
})
