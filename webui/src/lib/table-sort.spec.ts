import { describe, expect, it } from 'vitest'

import { sortThenSlice } from '@/lib/table-sort'

describe('sortThenSlice', () => {
  const rows = [
    { id: 'a', requests: 5, name: 'b' },
    { id: 'b', requests: null, name: 'a' },
    { id: 'c', requests: 9, name: 'c' },
    { id: 'd', requests: 1, name: 'd' },
  ]
  it('orders the whole list before taking the page', () => {
    const p0 = sortThenSlice(rows, { key: 'requests', direction: 'asc' }, 2, 0)
    const p1 = sortThenSlice(rows, { key: 'requests', direction: 'asc' }, 2, 1)
    expect(p0.map((r) => r.id)).toEqual(['d', 'a'])
    expect(p1.map((r) => r.id)).toEqual(['c', 'b'])
  })
  it('keeps missing values last in both directions', () => {
    expect(sortThenSlice(rows, { key: 'requests', direction: 'desc' }, 4, 0).map((r) => r.id)).toEqual(['c', 'a', 'd', 'b'])
  })
  it('sorts text and keeps input order without a sort', () => {
    expect(sortThenSlice(rows, { key: 'name', direction: 'asc' }, 4, 0).map((r) => r.id)).toEqual(['b', 'a', 'c', 'd'])
    expect(sortThenSlice(rows, null, 2, 1).map((r) => r.id)).toEqual(['c', 'd'])
  })
  it('clamps an out-of-range page and does not mutate the input', () => {
    const copy = [...rows]
    expect(sortThenSlice(rows, null, 3, 9).map((r) => r.id)).toEqual(['d'])
    expect(rows).toEqual(copy)
  })
})
