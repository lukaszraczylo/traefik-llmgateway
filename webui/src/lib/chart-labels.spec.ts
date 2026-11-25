import { describe, expect, it } from 'vitest'

import { thinLabels } from './chart-labels'

const nums = (n: number) => Array.from({ length: n }, (_, i) => String(i))

describe('thinLabels', () => {
  it('keeps every label when within max', () => {
    expect(thinLabels(nums(6))).toEqual(nums(6))
  })

  it('keeps every Nth label and blanks the rest', () => {
    // 10 labels, max 6 -> N = 2
    expect(thinLabels(nums(10))).toEqual(['0', '', '2', '', '4', '', '6', '', '8', '9'])
  })

  it('always keeps the last label', () => {
    const out = thinLabels(nums(25), 6)
    expect(out.at(-1)).toBe('24')
    expect(out).toHaveLength(25)
  })

  it('keeps the output length and handles empty input', () => {
    expect(thinLabels([])).toEqual([])
    expect(thinLabels(['a'])).toEqual(['a'])
  })
})
