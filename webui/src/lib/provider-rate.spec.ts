import { describe, expect, it } from 'vitest'

import {
  dayRateTitle,
  formatRatePercent,
  isModelDegraded,
  minuteRateTitle,
  PROVIDER_RATE_AMBER_THRESHOLD,
  PROVIDER_RATE_DESTRUCTIVE_THRESHOLD,
  providerRateStatus,
  providerSuccessRate,
} from './provider-rate'

describe('providerSuccessRate', () => {
  it('computes (attempts-failures)/attempts for a healthy provider', () => {
    expect(providerSuccessRate(100, 1)).toBeCloseTo(0.99)
  })

  it('computes 1 (100%) for zero failures', () => {
    expect(providerSuccessRate(50, 0)).toBe(1)
  })

  it('computes 0 for every attempt failing', () => {
    expect(providerSuccessRate(10, 10)).toBe(0)
  })

  it('returns null for zero attempts (no traffic, not a 0/0 NaN)', () => {
    expect(providerSuccessRate(0, 0)).toBeNull()
  })

  it('returns null for a negative attempts count (defensive edge case)', () => {
    expect(providerSuccessRate(-1, 0)).toBeNull()
  })

  it('returns null for a non-finite attempts count (folded review minor: NaN-guard the badge props)', () => {
    expect(providerSuccessRate(Number.NaN, 0)).toBeNull()
    expect(providerSuccessRate(Number.POSITIVE_INFINITY, 0)).toBeNull()
  })

  it('treats a non-finite failures count as 0 rather than propagating NaN', () => {
    expect(providerSuccessRate(10, Number.NaN)).toBe(1)
  })
})

describe('providerRateStatus', () => {
  it('is no-traffic for a null rate', () => {
    expect(providerRateStatus(null)).toBe('no-traffic')
  })

  it('is healthy at exactly the amber threshold (boundary is inclusive on the healthy side)', () => {
    expect(providerRateStatus(PROVIDER_RATE_AMBER_THRESHOLD)).toBe('healthy')
  })

  it('is healthy at a perfect 100% rate', () => {
    expect(providerRateStatus(1)).toBe('healthy')
  })

  it('is degraded just under the amber threshold', () => {
    expect(providerRateStatus(PROVIDER_RATE_AMBER_THRESHOLD - 0.001)).toBe('degraded')
  })

  it('is degraded at exactly the destructive threshold (boundary is inclusive on the degraded side)', () => {
    expect(providerRateStatus(PROVIDER_RATE_DESTRUCTIVE_THRESHOLD)).toBe('degraded')
  })

  it('is severe just under the destructive threshold', () => {
    expect(providerRateStatus(PROVIDER_RATE_DESTRUCTIVE_THRESHOLD - 0.001)).toBe('severe')
  })

  it('is severe at a rate of 0', () => {
    expect(providerRateStatus(0)).toBe('severe')
  })
})

describe('isModelDegraded', () => {
  it('is false with no traffic (attempts 0)', () => {
    expect(isModelDegraded(0, 0)).toBe(false)
  })

  it('is false for a healthy model (100%)', () => {
    expect(isModelDegraded(20, 0)).toBe(false)
  })

  it('is true for a model below the amber threshold', () => {
    expect(isModelDegraded(100, 5)).toBe(true)
  })

  it('is true for a severely degraded model too (severe is a subset of degraded-or-worse)', () => {
    expect(isModelDegraded(10, 10)).toBe(true)
  })
})

describe('formatRatePercent', () => {
  it('floors rather than rounds, so only a true 100% rate ever shows 100%', () => {
    expect(formatRatePercent(0.994)).toBe('99%')
    // Round would have bumped this to "100%" (99.5 rounds up) — floor
    // correctly keeps it "99%": the rate is not actually a perfect 100%.
    expect(formatRatePercent(0.995)).toBe('99%')
    expect(formatRatePercent(0.999)).toBe('99%')
  })

  it('shows 100% only for an exact rate of 1', () => {
    expect(formatRatePercent(1)).toBe('100%')
  })

  it('floors 0 to "0%"', () => {
    expect(formatRatePercent(0)).toBe('0%')
  })
})

describe('minuteRateTitle', () => {
  it('reports no traffic distinctly when attemptsMinute is 0', () => {
    expect(minuteRateTitle(0, 0)).toBe('no traffic in the last minute')
  })

  it('uses singular attempt/failure wording for a count of exactly 1', () => {
    expect(minuteRateTitle(1, 1)).toBe('1 attempt, 1 failure in the last minute (0%)')
  })

  it('pluralizes attempts independently of a singular failure count, with the floored percent', () => {
    expect(minuteRateTitle(12, 1)).toBe('12 attempts, 1 failure in the last minute (91%)')
  })

  it('omits failures from wording only via the count itself (0 failures still says "0 failures")', () => {
    expect(minuteRateTitle(5, 0)).toBe('5 attempts, 0 failures in the last minute (100%)')
  })
})

describe('dayRateTitle', () => {
  it('reports no traffic distinctly when attemptsDay is 0 (per-model badge fallback, SHOULD-2)', () => {
    expect(dayRateTitle(0, 0)).toBe('no traffic today')
  })

  it('uses singular attempt/failure wording for a count of exactly 1', () => {
    expect(dayRateTitle(1, 1)).toBe('1 attempt, 1 failure today (0%)')
  })

  it('pluralizes independently, with the floored percent', () => {
    expect(dayRateTitle(12, 1)).toBe('12 attempts, 1 failure today (91%)')
  })

  it('says "today" rather than "in the last minute" — the only wording difference from minuteRateTitle', () => {
    expect(dayRateTitle(5, 0)).toBe('5 attempts, 0 failures today (100%)')
  })
})
