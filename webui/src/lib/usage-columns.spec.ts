import type { VNode } from 'vue'
import { isVNode } from 'vue'
import { describe, expect, it } from 'vitest'

import ScopeLink from '@/components/ScopeLink.vue'
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
    rejectionsPerDay: 0,
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

function cellOf(col: unknown): (ctx: { row: { original: AdminUsageEntryView } }) => unknown {
  return (col as { cell: (ctx: { row: { original: AdminUsageEntryView } }) => unknown }).cell
}

describe('usageColumns: sorting stays on raw numeric values', () => {
  const columns = usageColumns('Name', 'Group', () => 'g')

  it('a count column (reqDay) accessorFn returns the raw number, not a compact string', () => {
    const col = columns.find((c) => c.id === 'reqDay')
    expect(accessorFnOf(col)(testEntry(), 0)).toBe(15_000_000)
  })

  it('a count column cell renders a CompactNumber vnode, not the raw number as text, when no limit is configured', () => {
    const col = columns.find((c) => c.id === 'reqDay')
    const rendered = cellOf(col)({ row: { original: testEntry() } })
    expect(isVNode(rendered)).toBe(true)
  })

  it('a cost column cell still renders plain formatted text (compact:false), not a CompactNumber vnode, when no limit is configured', () => {
    const col = columns.find((c) => c.id === 'costDay')
    const rendered = cellOf(col)({ row: { original: testEntry() } })
    expect(isVNode(rendered)).toBe(false)
    expect(rendered).toBe('$1.2345')
  })

  it('a storeDown row still masks every count cell as "?" regardless of compact rendering', () => {
    const col = columns.find((c) => c.id === 'tokInDay')
    const rendered = cellOf(col)({ row: { original: testEntry({ storeDown: true }) } })
    expect(rendered).toBe('?')
  })

  // Review finding: TanStack's getAutoSortingFn only inspects rows 11+
  // (RowSorting.js's own `flatRows.slice(10)`) to auto-detect numeric vs.
  // string sorting, so a Groups table with 10 or fewer rows silently fell
  // back to `basic` (plain string a > b comparison), sorting "10" before "9".
  // An explicit sortingFn sidesteps that row-count-dependent auto-detection
  // entirely.
  it('the secondary (Members/Group) column declares an explicit sortingFn, not left to row-count-dependent auto-detection', () => {
    const col = columns.find((c) => c.id === 'secondary')
    expect((col as { sortingFn?: string }).sortingFn).toBe('alphanumeric')
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

// F1 (usage-vs-limit bars): a column built with limitOf renders a UsageBar
// beside its number ONLY when the row's own limit is actually configured
// and the row is not storeDown — no fabricated bar for an unlimited scope.
describe('usageColumns: F1 usage-vs-limit bars', () => {
  const columns = usageColumns('Name', 'Group', () => 'g')

  it('reqDay renders a bare CompactNumber vnode, no bar, when the row has no limit configured', () => {
    const col = columns.find((c) => c.id === 'reqDay')
    const rendered = cellOf(col)({ row: { original: testEntry() } }) as VNode
    expect(rendered.type).not.toBe('span')
  })

  it('reqDay wraps the number with a UsageBar when the row has that limit configured', () => {
    const col = columns.find((c) => c.id === 'reqDay')
    const entry = testEntry({ limits: { requestsPerDay: 100 }, requestsPerDay: 40 })
    const rendered = cellOf(col)({ row: { original: entry } }) as VNode
    expect(isVNode(rendered)).toBe(true)
    expect(rendered.type).toBe('span')
    const children = rendered.children as VNode[]
    expect(children).toHaveLength(2)
    expect(children[1].props?.ratio).toBeCloseTo(0.4)
  })

  it('the token-day columns measure the bar against the COMBINED in+out total, not each column\'s own individual count', () => {
    const col = columns.find((c) => c.id === 'tokInDay')
    const entry = testEntry({ limits: { tokensPerDay: 1000 }, tokensInPerDay: 300, tokensOutPerDay: 400 })
    const rendered = cellOf(col)({ row: { original: entry } }) as VNode
    const children = rendered.children as VNode[]
    expect(children[1].props?.ratio).toBeCloseTo(0.7)
  })

  it('a cost column converts its USD limit to micro-USD before computing the ratio', () => {
    const col = columns.find((c) => c.id === 'costDay')
    const entry = testEntry({ limits: { costPerDayUSD: 1 }, costPerDayMicroUsd: 250_000 })
    const rendered = cellOf(col)({ row: { original: entry } }) as VNode
    const children = rendered.children as VNode[]
    expect(children[1].props?.ratio).toBeCloseTo(0.25)
  })

  it('renders no bar for a storeDown row even when a limit is configured', () => {
    const col = columns.find((c) => c.id === 'reqDay')
    const entry = testEntry({ limits: { requestsPerDay: 100 }, storeDown: true })
    expect(cellOf(col)({ row: { original: entry } })).toBe('?')
  })

  // P10 review fix: the token columns' bar must announce the COMBINED
  // in+out measurement it actually shows, not the column's own single-
  // direction header text — a screen-reader user has no other column
  // beside it to infer that from, unlike a sighted reader.
  it('the tokIn/day column\'s bar label spells out "tokens (in+out)/day", not the column header "tokIn/day"', () => {
    const col = columns.find((c) => c.id === 'tokInDay')
    const entry = testEntry({ limits: { tokensPerDay: 1000 }, tokensInPerDay: 300, tokensOutPerDay: 400 })
    const rendered = cellOf(col)({ row: { original: entry } }) as VNode
    const children = rendered.children as VNode[]
    expect(children[1].props?.label).toBe('tokens (in+out)/day')
  })

  it('the tokOut/month column\'s bar label spells out "tokens (in+out)/month"', () => {
    const col = columns.find((c) => c.id === 'tokOutMonth')
    const entry = testEntry({ limits: { tokensPerMonth: 1000 }, tokensInPerMonth: 300, tokensOutPerMonth: 400 })
    const rendered = cellOf(col)({ row: { original: entry } }) as VNode
    const children = rendered.children as VNode[]
    expect(children[1].props?.label).toBe('tokens (in+out)/month')
  })

  it('the req/day column\'s bar label is "req/day" (matches its own header — no in+out ambiguity there)', () => {
    const col = columns.find((c) => c.id === 'reqDay')
    const entry = testEntry({ limits: { requestsPerDay: 100 }, requestsPerDay: 40 })
    const rendered = cellOf(col)({ row: { original: entry } }) as VNode
    const children = rendered.children as VNode[]
    expect(children[1].props?.label).toBe('req/day')
  })

  // P11 review fix: numericColumn now reuses budgetRatios (lib/usage-bars.ts)
  // instead of its own dropped limitOf/usdLimitMicros — a cost limit that
  // rounds to 0 micro-USD is skipped (no bar, no Infinity/NaN ratio),
  // exactly like Go, rather than the column's old independent conversion
  // disagreeing with budgetRatios about it.
  it('costDay renders no bar (skips, does not divide by 0) when the configured limit rounds to 0 micro-USD', () => {
    const col = columns.find((c) => c.id === 'costDay')
    const entry = testEntry({ limits: { costPerDayUSD: 0.0000001 }, costPerDayMicroUsd: 5 })
    const rendered = cellOf(col)({ row: { original: entry } })
    expect(isVNode(rendered)).toBe(false) // plain formatted text, not a UsageBar-wrapping span
  })
})

// F6 (ScopeLink id cell): the id column renders a ScopeLink pointed at
// this row's own "kind:id" Charts scope, never a plain span — the click-
// through-to-Charts affordance every Users/Groups/Members row gets.
describe('usageColumns: F6 id cell is a ScopeLink', () => {
  const columns = usageColumns('Name', 'Group', () => 'g')
  const col = columns.find((c) => c.id === 'id')

  it('renders a ScopeLink vnode, not a plain span', () => {
    const rendered = cellOf(col)({ row: { original: testEntry({ id: 'alice', kind: 'user' }) } }) as VNode
    expect(rendered.type).toBe(ScopeLink)
  })

  it('builds the scope as "user:{id}" for a user row', () => {
    const rendered = cellOf(col)({ row: { original: testEntry({ id: 'alice', kind: 'user' }) } }) as VNode
    expect(rendered.props?.scope).toBe('user:alice')
    expect(rendered.props?.label).toBe('alice')
  })

  it('builds the scope as "group:{id}" for a group row', () => {
    const rendered = cellOf(col)({ row: { original: testEntry({ id: 'eng', kind: 'group' }) } }) as VNode
    expect(rendered.props?.scope).toBe('group:eng')
  })

  it('is not masked by storeDown — a scope link stays clickable even for a row with stale/unknown counters', () => {
    const rendered = cellOf(col)({ row: { original: testEntry({ id: 'alice', kind: 'user', storeDown: true }) } }) as VNode
    expect(rendered.type).toBe(ScopeLink)
  })
})

// F4 (rejected/day column): destructive tint applies only once the count
// is actually positive — a healthy scope (0 rejections) renders plain,
// matching every other "nothing to show reads as no flag" indicator in
// this panel (ProvidersView.vue's badges).
describe('usageColumns: F4 rejected/day column', () => {
  const columns = usageColumns('Name', 'Group', () => 'g')
  const col = columns.find((c) => c.id === 'rejDay')

  it('accessorFn reads the raw rejectionsPerDay number', () => {
    expect(accessorFnOf(col)(testEntry({ rejectionsPerDay: 12 }), 0)).toBe(12)
  })

  it('renders no destructive class when rejectionsPerDay is 0', () => {
    const rendered = cellOf(col)({ row: { original: testEntry({ rejectionsPerDay: 0 }) } }) as VNode
    expect(rendered.props?.class).toBeUndefined()
  })

  it('renders a destructive class when rejectionsPerDay is positive', () => {
    const rendered = cellOf(col)({ row: { original: testEntry({ rejectionsPerDay: 3 }) } }) as VNode
    expect(rendered.props?.class).toBe('text-destructive')
  })

  it('masks as "?" for a storeDown row', () => {
    expect(cellOf(col)({ row: { original: testEntry({ storeDown: true }) } })).toBe('?')
  })
})

// F7 (cost forecast per row): costMonthProjColumn extrapolates each row's
// own costPerMonthMicroUsd against the SAME `now` every row in one
// usageColumns() call shares — passed in explicitly so this stays
// deterministic under test (coordinator brief: "now passed as prop for
// determinism").
describe('usageColumns: F7 cost/month (proj.) column', () => {
  // Fixed at exactly the midpoint of a UTC 30-day month (April 2026) so
  // elapsedFraction is a clean 0.5 — projectMonthEnd(mtd, 0.5) == mtd*2.
  const now = new Date(Date.UTC(2026, 3, 16, 0, 0, 0))
  const columns = usageColumns('Name', 'Group', () => 'g', now)
  const col = columns.find((c) => c.id === 'costMonthProj')

  it('renders the projected month-end cost, formatted', () => {
    const entry = testEntry({ costPerMonthMicroUsd: 5_000_000 })
    const rendered = cellOf(col)({ row: { original: entry } }) as VNode
    expect(rendered.children).toBe('$10.0000')
  })

  it('renders a destructive class + title when the projection exceeds the configured cost/month limit', () => {
    const entry = testEntry({ costPerMonthMicroUsd: 5_000_000, limits: { costPerMonthUSD: 8 } })
    const rendered = cellOf(col)({ row: { original: entry } }) as VNode
    expect(rendered.props?.class).toBe('text-destructive')
    expect(rendered.props?.title).toMatch(/exceed/)
  })

  it('renders no destructive class when the projection stays under the limit', () => {
    const entry = testEntry({ costPerMonthMicroUsd: 5_000_000, limits: { costPerMonthUSD: 20 } })
    const rendered = cellOf(col)({ row: { original: entry } }) as VNode
    expect(rendered.props?.class).toBeUndefined()
  })

  it('masks as "?" for a storeDown row', () => {
    expect(cellOf(col)({ row: { original: testEntry({ storeDown: true }) } })).toBe('?')
  })
})
