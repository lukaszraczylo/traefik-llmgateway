import { describe, expect, it } from 'vitest'

import { countThrottled, deltaOf, rangeMs, sumPoints, trafficSeries } from './overview-metrics'

describe('sumPoints', () => {
  it('sums and ignores non-finite values', () => {
    expect(sumPoints([1, 2, 3])).toBe(6)
    expect(sumPoints([1, Number.NaN, 2])).toBe(3)
    expect(sumPoints([])).toBe(0)
  })
})

describe('deltaOf', () => {
  it('is null without a previous value to compare against', () => {
    expect(deltaOf(10, 0)).toBeNull()
    expect(deltaOf(10, null)).toBeNull()
  })
  it('formats a signed whole percent with its trend', () => {
    expect(deltaOf(112, 100)).toEqual({ text: '+12%', trend: 'up' })
    expect(deltaOf(50, 100)).toEqual({ text: '-50%', trend: 'down' })
  })
  it('reads under half a percent as flat', () => {
    expect(deltaOf(100.2, 100)).toEqual({ text: '0%', trend: 'flat' })
  })
  it('caps absurd swings so the tile never overflows', () => {
    expect(deltaOf(100000, 1)?.text).toBe('+999%')
  })
})

describe('rangeMs', () => {
  it('converts each window to milliseconds', () => {
    expect(rangeMs('hour', 24)).toBe(24 * 3_600_000)
    expect(rangeMs('day', 7)).toBe(7 * 86_400_000)
    expect(rangeMs('month', 3)).toBe(3 * 30 * 86_400_000)
  })
})

describe('countThrottled', () => {
  const now = Date.parse('2026-11-25T12:00:00Z')
  const at = (minsAgo: number) => new Date(now - minsAgo * 60_000).toISOString()
  it('counts rate_limit and paced events inside the range only', () => {
    const events = [
      { kind: 'rate_limit', time: at(5) },
      { kind: 'rate_limit', time: at(10) },
      { kind: 'paced', time: at(20) },
      { kind: 'rate_limit', time: at(500) },
      { kind: 'upstream', time: at(1) },
    ]
    expect(countThrottled(events, now, 60 * 60_000)).toEqual({ rateLimited: 2, paced: 1 })
  })
  it('skips events with an unparseable time', () => {
    expect(countThrottled([{ kind: 'rate_limit', time: 'nope' }], now, 60_000)).toEqual({ rateLimited: 0, paced: 0 })
  })
})

describe('trafficSeries', () => {
  const top = [
    { scope: 'model:a/x', points: [1, 2] },
    { scope: 'model:b/y', points: [3, 4] },
  ]
  it('stacks the top models, then the remainder as Other', () => {
    const s = trafficSeries(top, [10, 10], null)
    expect(s.map((x) => x.name)).toEqual(['a/x', 'b/y', 'Other'])
    expect(s[2]?.data).toEqual([6, 4])
    expect(s.every((x) => x.stack === undefined)).toBe(true)
  })
  it('omits Other when the top models cover all traffic', () => {
    expect(trafficSeries(top, [4, 6], null).map((x) => x.name)).toEqual(['a/x', 'b/y'])
  })
  it('adds the previous period as an unstacked series when compare is on', () => {
    const s = trafficSeries(top, [10, 10], [7, 8])
    expect(s.at(-1)).toMatchObject({ name: 'Previous period', data: [7, 8], stack: false })
  })
  it('is empty without any traffic', () => {
    expect(trafficSeries([], [0, 0], null)).toEqual([])
  })
})
