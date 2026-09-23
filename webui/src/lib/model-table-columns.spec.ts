import type { VNode } from 'vue'
import { isVNode } from 'vue'
import { describe, expect, it } from 'vitest'

import ModelChip from '@/components/ModelChip.vue'
import ScopeLink from '@/components/ScopeLink.vue'
import { Badge } from '@/components/ui/badge'
import { modelTableColumns } from './model-table-columns'
import type { AdminUsageModelEntry } from '@/types/api'

function testEntry(overrides: Partial<AdminUsageModelEntry> = {}): AdminUsageModelEntry {
  return {
    id: 'openai/gpt-5',
    value: 42,
    requests: 10,
    tokensIn: 100,
    tokensOut: 200,
    costMicroUsd: 1_234_500,
    free: false,
    ...overrides,
  }
}

function accessorFnOf(col: unknown): (row: AdminUsageModelEntry, index: number) => unknown {
  return (col as { accessorFn: (row: AdminUsageModelEntry, index: number) => unknown }).accessorFn
}

function cellOf(col: unknown): (ctx: { row: { original: AdminUsageModelEntry } }) => unknown {
  return (col as { cell: (ctx: { row: { original: AdminUsageModelEntry } }) => unknown }).cell
}

describe('modelTableColumns: Model column', () => {
  const columns = modelTableColumns()
  const col = columns.find((c) => c.id === 'model')

  it('accessorFn reads the raw canonical id (no routableModelId join — id is already provider/model)', () => {
    expect(accessorFnOf(col)(testEntry({ id: 'anthropic/claude' }), 0)).toBe('anthropic/claude')
  })

  it('cell renders a ModelChip with the id, no context/cost props (this table has no resolved metadata to pass)', () => {
    const rendered = cellOf(col)({ row: { original: testEntry({ id: 'anthropic/claude' }) } }) as VNode
    const children = rendered.children as VNode[]
    const chip = children.find((c) => c.type === ModelChip)
    expect(chip).toBeDefined()
    expect(chip?.props?.id).toBe('anthropic/claude')
  })

  it('cell renders a ScopeLink pointed at "model:{id}"', () => {
    const rendered = cellOf(col)({ row: { original: testEntry({ id: 'anthropic/claude' }) } }) as VNode
    const children = rendered.children as VNode[]
    const link = children.find((c) => c.type === ScopeLink)
    expect(link).toBeDefined()
    expect(link?.props?.scope).toBe('model:anthropic/claude')
    expect(link?.props?.label).toBe('anthropic/claude')
  })
})

describe('modelTableColumns: detail count columns sort on raw numbers, render CompactNumber', () => {
  const columns = modelTableColumns()

  it.each([
    ['requests', (e: AdminUsageModelEntry) => e.requests],
    ['tokensIn', (e: AdminUsageModelEntry) => e.tokensIn],
    ['tokensOut', (e: AdminUsageModelEntry) => e.tokensOut],
  ] as const)('%s accessorFn returns the raw detail number, not a compact string', (id, read) => {
    const col = columns.find((c) => c.id === id)
    const entry = testEntry({ requests: 15_000_000, tokensIn: 999, tokensOut: 12_000 })
    expect(accessorFnOf(col)(entry, 0)).toBe(read(entry))
  })

  it.each(['requests', 'tokensIn', 'tokensOut'] as const)('%s cell renders a CompactNumber vnode', (id) => {
    const col = columns.find((c) => c.id === id)
    const rendered = cellOf(col)({ row: { original: testEntry() } })
    expect(isVNode(rendered)).toBe(true)
  })

  it.each(['requests', 'tokensIn', 'tokensOut'] as const)(
    '%s falls back to 0 (never undefined) when detail was not requested, defensively',
    (id) => {
      const col = columns.find((c) => c.id === id)
      const entry = testEntry({ requests: undefined, tokensIn: undefined, tokensOut: undefined })
      expect(accessorFnOf(col)(entry, 0)).toBe(0)
    },
  )

  it('sorting by requests orders numerically, not lexicographically', () => {
    const col = columns.find((c) => c.id === 'requests')
    const accessor = accessorFnOf(col)
    const entries = [
      testEntry({ id: 'a', requests: 15_000_000 }),
      testEntry({ id: 'b', requests: 999 }),
      testEntry({ id: 'c', requests: 12_000 }),
    ]
    const sorted = [...entries].sort((x, y) => (accessor(x, 0) as number) - (accessor(y, 0) as number))
    expect(sorted.map((e) => e.id)).toEqual(['b', 'c', 'a'])
  })
})

describe('modelTableColumns: Cost column', () => {
  const columns = modelTableColumns()
  const col = columns.find((c) => c.id === 'cost')

  it('accessorFn reads the raw micro-USD cost, even for a free model (real 0, not omitted)', () => {
    expect(accessorFnOf(col)(testEntry({ costMicroUsd: 1_234_500, free: false }), 0)).toBe(1_234_500)
    expect(accessorFnOf(col)(testEntry({ costMicroUsd: 0, free: true }), 0)).toBe(0)
  })

  it('renders plain formatCost text for a priced (non-free) entry', () => {
    const rendered = cellOf(col)({ row: { original: testEntry({ costMicroUsd: 1_234_500, free: false }) } })
    expect(isVNode(rendered)).toBe(false)
    expect(rendered).toBe('$1.2345')
  })

  it('shows the real recorded cost, not the badge, for a model marked free after it accrued cost', () => {
    const rendered = cellOf(col)({ row: { original: testEntry({ costMicroUsd: 1_234_500, free: true }) } })
    expect(isVNode(rendered)).toBe(false)
    expect(rendered).toBe('$1.2345')
  })

  it('renders a Badge with the accessible text "free" for a free entry, instead of $0', () => {
    const rendered = cellOf(col)({ row: { original: testEntry({ costMicroUsd: 0, free: true }) } }) as VNode
    expect(rendered.type).toBe(Badge)
    expect(typeof rendered.children).toBe('object')
    const slotDefault = (rendered.children as { default: () => unknown }).default
    expect(slotDefault()).toBe('free')
  })

  it('a free entry still sorts by its real (0) cost, not excluded or reordered by the badge', () => {
    const entries = [testEntry({ id: 'paid', costMicroUsd: 5_000_000, free: false }), testEntry({ id: 'free', costMicroUsd: 0, free: true })]
    const accessor = accessorFnOf(col)
    const sorted = [...entries].sort((x, y) => (accessor(y, 0) as number) - (accessor(x, 0) as number)) // desc
    expect(sorted.map((e) => e.id)).toEqual(['paid', 'free'])
  })

  it('falls back to formatCost($0) when free is false/undefined and costMicroUsd is undefined (detail not requested)', () => {
    const rendered = cellOf(col)({ row: { original: testEntry({ costMicroUsd: undefined, free: undefined }) } })
    expect(rendered).toBe('$0.0000')
  })
})
