import { describe, expect, it } from 'vitest'

import { pageWindow } from './paging'

describe('pageWindow', () => {
  it.each([
    { name: 'empty', total: 0, size: 25, idx: 0, want: { index: 0, count: 1, from: 0, to: 0 } },
    { name: 'fits one page', total: 25, size: 25, idx: 0, want: { index: 0, count: 1, from: 0, to: 25 } },
    { name: 'first page', total: 31, size: 25, idx: 0, want: { index: 0, count: 2, from: 0, to: 25 } },
    { name: 'last partial page', total: 31, size: 25, idx: 1, want: { index: 1, count: 2, from: 25, to: 31 } },
    { name: 'index past the end clamps', total: 31, size: 25, idx: 9, want: { index: 1, count: 2, from: 25, to: 31 } },
    { name: 'negative index clamps', total: 31, size: 25, idx: -3, want: { index: 0, count: 2, from: 0, to: 25 } },
    { name: 'filter shrank the data', total: 3, size: 25, idx: 4, want: { index: 0, count: 1, from: 0, to: 3 } },
    { name: 'zero page size is treated as 1', total: 2, size: 0, idx: 1, want: { index: 1, count: 2, from: 1, to: 2 } },
  ])('$name', ({ total, size, idx, want }) => {
    expect(pageWindow(total, size, idx)).toEqual(want)
  })

  it('every reachable page of a non-empty table shows at least one row', () => {
    for (let total = 1; total < 80; total++) {
      for (let idx = 0; idx < 6; idx++) {
        const w = pageWindow(total, 25, idx)
        expect(w.to).toBeGreaterThan(w.from)
        expect(w.to).toBeLessThanOrEqual(total)
      }
    }
  })
})
