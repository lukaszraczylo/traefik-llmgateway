import { describe, expect, it } from 'vitest'

import { headroom, MIN_PROJECTION_FRACTION, monthProgress, projectMonthEnd } from './forecast'

describe('monthProgress', () => {
  it('reads 0 at the first instant of a UTC month', () => {
    expect(monthProgress(new Date(Date.UTC(2026, 2, 1, 0, 0, 0)))).toBe(0)
  })

  it('reads exactly 0.5 at the midpoint of a 30-day month (April)', () => {
    expect(monthProgress(new Date(Date.UTC(2026, 3, 16, 0, 0, 0)))).toBe(0.5)
  })

  it('reads approaching 1 near the end of the month, never reaching or exceeding it', () => {
    const p = monthProgress(new Date(Date.UTC(2026, 2, 31, 23, 59, 59)))
    expect(p).toBeLessThan(1)
    expect(p).toBeGreaterThan(0.99)
  })

  it('handles a leap February (2028 has 29 days) — Feb 15 sits near the midpoint of 29, not 28, days', () => {
    const p = monthProgress(new Date(Date.UTC(2028, 1, 15, 12, 0, 0)))
    // 14.5 days elapsed of 29 total = exactly 0.5.
    expect(p).toBeCloseTo(0.5, 10)
  })

  it('handles December -> January rollover without a hardcoded month-length table', () => {
    const p = monthProgress(new Date(Date.UTC(2026, 11, 16, 0, 0, 0)))
    expect(p).toBeCloseTo(15 / 31, 10)
  })
})

describe('projectMonthEnd', () => {
  it('returns null below MIN_PROJECTION_FRACTION — too early in the month to extrapolate', () => {
    expect(projectMonthEnd(1_000_000, MIN_PROJECTION_FRACTION - 0.001)).toBeNull()
  })

  it('returns null at elapsedFraction 0 (the very first instant) rather than dividing by zero', () => {
    expect(projectMonthEnd(0, 0)).toBeNull()
  })

  it('returns null at exactly the threshold boundary minus epsilon, but projects at the threshold itself', () => {
    expect(projectMonthEnd(1000, MIN_PROJECTION_FRACTION)).toBe(Math.round(1000 / MIN_PROJECTION_FRACTION))
  })

  it('linearly extrapolates mtd/elapsedFraction, rounded to the nearest whole micro-USD', () => {
    expect(projectMonthEnd(15_000_000, 0.5)).toBe(30_000_000)
  })

  it('rounds a non-exact division to the nearest integer', () => {
    expect(projectMonthEnd(1, 0.3)).toBe(Math.round(1 / 0.3))
  })

  it('projects a full-month reading (elapsedFraction 1) back to itself', () => {
    expect(projectMonthEnd(7_500_000, 1)).toBe(7_500_000)
  })
})

// End-to-end monthProgress -> projectMonthEnd against real UTC clock times,
// not just a bare elapsedFraction — pins MIN_PROJECTION_FRACTION's real-
// world meaning (0.02 of a 30-day month is 14.4 hours, NOT minutes) against
// an actual point in a month.
describe('monthProgress -> projectMonthEnd (real clock times)', () => {
  it('the first hour of a 30-day month is well under MIN_PROJECTION_FRACTION — no projection yet', () => {
    const oneHourIn = new Date(Date.UTC(2026, 3, 1, 1, 0, 0))
    expect(projectMonthEnd(100_000, monthProgress(oneHourIn))).toBeNull()
  })

  it('13 hours into a 30-day month is still under the threshold', () => {
    const thirteenHoursIn = new Date(Date.UTC(2026, 3, 1, 13, 0, 0))
    expect(projectMonthEnd(100_000, monthProgress(thirteenHoursIn))).toBeNull()
  })

  it('16 hours into a 30-day month has cleared the threshold and projects', () => {
    const sixteenHoursIn = new Date(Date.UTC(2026, 3, 1, 16, 0, 0))
    expect(projectMonthEnd(100_000, monthProgress(sixteenHoursIn))).not.toBeNull()
  })
})

describe('headroom', () => {
  it('returns all-null, willExceed false when projectedMicros is null (too early to project)', () => {
    expect(headroom(null, 100)).toEqual({ limitMicros: null, remainingMicros: null, willExceed: false })
  })

  it('returns all-null, willExceed false when limitUsd is undefined (unlimited)', () => {
    expect(headroom(5_000_000, undefined)).toEqual({ limitMicros: null, remainingMicros: null, willExceed: false })
  })

  it('returns all-null, willExceed false when limitUsd is 0 (LimitsConfig "unlimited" convention)', () => {
    expect(headroom(5_000_000, 0)).toEqual({ limitMicros: null, remainingMicros: null, willExceed: false })
  })

  it('returns all-null, willExceed false for a negative limitUsd (defensive)', () => {
    expect(headroom(5_000_000, -10)).toEqual({ limitMicros: null, remainingMicros: null, willExceed: false })
  })

  it('reports positive remainingMicros and willExceed false when the projection stays under the limit', () => {
    expect(headroom(4_000_000, 5)).toEqual({ limitMicros: 5_000_000, remainingMicros: 1_000_000, willExceed: false })
  })

  it('reports negative remainingMicros and willExceed true once the projection crosses the limit', () => {
    expect(headroom(6_000_000, 5)).toEqual({ limitMicros: 5_000_000, remainingMicros: -1_000_000, willExceed: true })
  })

  it('treats an exact match (projection == limit) as not exceeding (remainingMicros 0)', () => {
    expect(headroom(5_000_000, 5)).toEqual({ limitMicros: 5_000_000, remainingMicros: 0, willExceed: false })
  })
})
