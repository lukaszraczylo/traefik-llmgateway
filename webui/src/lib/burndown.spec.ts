import { describe, expect, it } from 'vitest'

import { cumulative, currentMonthDaySpan, MIN_ELAPSED_DAYS, runOutDate } from './burndown'

describe('currentMonthDaySpan', () => {
  it('returns the UTC day-of-month', () => {
    expect(currentMonthDaySpan(new Date(Date.UTC(2026, 8, 23, 12, 0, 0)))).toBe(23)
  })

  it('returns 1 on the first day of the month', () => {
    expect(currentMonthDaySpan(new Date(Date.UTC(2026, 8, 1, 0, 0, 0)))).toBe(1)
  })
})

describe('cumulative', () => {
  it('returns a running total, oldest first', () => {
    expect(cumulative([1, 2, 3])).toEqual([1, 3, 6])
  })

  it('returns an empty array for empty input', () => {
    expect(cumulative([])).toEqual([])
  })

  it('handles a single point', () => {
    expect(cumulative([5])).toEqual([5])
  })
})

describe('runOutDate', () => {
  it('returns null when no budget is configured (limitMicros <= 0)', () => {
    expect(runOutDate(1_000_000, 0, new Date(Date.UTC(2026, 3, 15)))).toBeNull()
    expect(runOutDate(1_000_000, -10, new Date(Date.UTC(2026, 3, 15)))).toBeNull()
  })

  it('returns now itself once spend has already met or exceeded the limit', () => {
    const now = new Date(Date.UTC(2026, 3, 15, 8, 0, 0))
    expect(runOutDate(5_000_000, 5_000_000, now)).toEqual(now)
    expect(runOutDate(6_000_000, 5_000_000, now)).toEqual(now)
  })

  it('returns null before MIN_ELAPSED_DAYS have elapsed in the month', () => {
    // A few hours into day 1 — well under one full elapsed day.
    const earlyMonth = new Date(Date.UTC(2026, 3, 1, 2, 0, 0))
    expect(runOutDate(1000, 1_000_000, earlyMonth)).toBeNull()
  })

  it('returns null when the daily rate so far is 0 — spend has not started', () => {
    const now = new Date(Date.UTC(2026, 3, 16, 0, 0, 0))
    expect(runOutDate(0, 1_000_000, now)).toBeNull()
  })

  it('projects forward at the MTD average daily rate', () => {
    // April 2026 (30 days): 15 full days elapsed, 15,000,000 micros spent
    // -> 1,000,000 micros/day. A 20,000,000 limit has 5,000,000 remaining
    // -> 5 more days to exhaust.
    const now = new Date(Date.UTC(2026, 3, 16, 0, 0, 0))
    const result = runOutDate(15_000_000, 20_000_000, now)
    expect(result).not.toBeNull()
    expect(result!.getTime()).toBe(now.getTime() + 5 * 24 * 60 * 60 * 1000)
  })

  it('exposes MIN_ELAPSED_DAYS as a stable, importable constant', () => {
    expect(MIN_ELAPSED_DAYS).toBe(1)
  })
})
