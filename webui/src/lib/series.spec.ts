import { describe, expect, it } from 'vitest'

import { other, ratioSeries, stackTopN, sumByProvider } from './series'

describe('stackTopN', () => {
  it('returns the n series with the largest total, descending', () => {
    const series = [
      { scope: 'model:a', points: [1, 1] }, // total 2
      { scope: 'model:b', points: [10, 10] }, // total 20
      { scope: 'model:c', points: [5, 5] }, // total 10
    ]
    expect(stackTopN(series, 2)).toEqual([
      { scope: 'model:b', points: [10, 10] },
      { scope: 'model:c', points: [5, 5] },
    ])
  })

  it('returns every series, sorted, when n exceeds the array length', () => {
    const series = [
      { scope: 'model:a', points: [1] },
      { scope: 'model:b', points: [2] },
    ]
    expect(stackTopN(series, 10)).toHaveLength(2)
  })

  it('returns an empty array for n <= 0', () => {
    const series = [{ scope: 'model:a', points: [1] }]
    expect(stackTopN(series, 0)).toEqual([])
    expect(stackTopN(series, -1)).toEqual([])
  })

  it('breaks ties on original array order (stable sort)', () => {
    const series = [
      { scope: 'model:a', points: [5] },
      { scope: 'model:b', points: [5] },
    ]
    expect(stackTopN(series, 2).map((s) => s.scope)).toEqual(['model:a', 'model:b'])
  })

  it('does not mutate the input array', () => {
    const series = [
      { scope: 'model:a', points: [1] },
      { scope: 'model:b', points: [2] },
    ]
    const copy = [...series]
    stackTopN(series, 1)
    expect(series).toEqual(copy)
  })
})

describe('other', () => {
  it('subtracts the sum of charted series from the total, per bucket', () => {
    const total = [100, 200]
    const charted = [
      { scope: 'model:a', points: [30, 50] },
      { scope: 'model:b', points: [20, 40] },
    ]
    expect(other(total, charted)).toEqual([50, 110])
  })

  it('returns the total verbatim when charted is empty', () => {
    expect(other([1, 2, 3], [])).toEqual([1, 2, 3])
  })

  it('floors a bucket at 0 rather than going negative', () => {
    const total = [10]
    const charted = [{ scope: 'model:a', points: [15] }]
    expect(other(total, charted)).toEqual([0])
  })

  it('treats a missing point in a shorter charted series as 0', () => {
    const total = [10, 10]
    const charted = [{ scope: 'model:a', points: [4] }]
    expect(other(total, charted)).toEqual([6, 10])
  })
})

describe('sumByProvider', () => {
  it('groups model-scoped series by their provider prefix and sums per bucket', () => {
    const series = [
      { scope: 'model:openai/gpt-5', points: [10, 20] },
      { scope: 'model:openai/gpt-5-mini', points: [5, 5] },
      { scope: 'model:anthropic/claude', points: [1, 1] },
    ]
    const result = sumByProvider(series)
    expect(result).toEqual([
      { scope: 'openai', points: [15, 25] },
      { scope: 'anthropic', points: [1, 1] },
    ])
  })

  it('handles a bare (unprefixed) scope without a "model:" prefix', () => {
    const series = [{ scope: 'openai/gpt-5', points: [3] }]
    expect(sumByProvider(series)).toEqual([{ scope: 'openai', points: [3] }])
  })

  it('handles a discovered id with an extra internal slash — only the first segment is the provider', () => {
    const series = [{ scope: 'model:real/uni/deepseek-v4-flash-0731', points: [7] }]
    expect(sumByProvider(series)).toEqual([{ scope: 'real', points: [7] }])
  })

  it('sorts the grouped result by descending total', () => {
    const series = [
      { scope: 'model:small/x', points: [1] },
      { scope: 'model:big/y', points: [100] },
    ]
    expect(sumByProvider(series).map((s) => s.scope)).toEqual(['big', 'small'])
  })

  it('returns an empty array for an empty input', () => {
    expect(sumByProvider([])).toEqual([])
  })
})

describe('ratioSeries', () => {
  it('divides numerator by denominator per bucket', () => {
    expect(ratioSeries([1, 5, 10], [10, 10, 10])).toEqual([0.1, 0.5, 1])
  })

  it('returns null for a bucket with 0 attempts rather than NaN or a fabricated 0', () => {
    expect(ratioSeries([0, 3], [0, 10])).toEqual([null, 0.3])
  })

  it('treats a missing numerator point as 0', () => {
    expect(ratioSeries([], [5])).toEqual([0])
  })

  it('returns an empty array for empty input', () => {
    expect(ratioSeries([], [])).toEqual([])
  })
})
