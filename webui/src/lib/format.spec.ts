import { describe, expect, it } from 'vitest'

import { formatCompactCount, formatContextWindow, formatModelCostHover, refreshDetailLabel, refreshLabel } from './format'

const ZERO_TIME = '0001-01-01T00:00:00Z'

describe('refreshLabel', () => {
  it('reads "discovery off" when discovery is disabled, regardless of lastRefresh', () => {
    expect(refreshLabel(false, ZERO_TIME)).toBe('discovery off')
    expect(refreshLabel(false, new Date().toISOString())).toBe('discovery off')
    expect(refreshLabel(false, undefined)).toBe('discovery off')
  })

  it('reads "pending" when discovery is enabled but lastRefresh is still the unset sentinel', () => {
    expect(refreshLabel(true, ZERO_TIME)).toBe('pending')
  })

  it('reads "pending" when discovery is enabled and lastRefresh is undefined (edge case)', () => {
    expect(refreshLabel(true, undefined)).toBe('pending')
  })

  it('reads the relative-time "refreshed (Ns ago)" text once discovery has completed a refresh', () => {
    const oneMinuteAgo = new Date(Date.now() - 60_000).toISOString()
    expect(refreshLabel(true, oneMinuteAgo)).toMatch(/^refreshed \(\d+s ago\)$/)
  })
})

// TestAdminOverview_ProviderLastErrAfterFailedRefresh's webui counterpart:
// the accordion DETAIL row (folded review minor, v0.22 review round) must
// share refreshLabel's own three-state logic instead of calling
// formatTimestamp directly — the bug this closes was a discovery-off
// provider's expanded row still reading the raw "never" right next to a
// trigger row that correctly said "discovery off".
describe('refreshDetailLabel', () => {
  it('reads "discovery off" when discovery is disabled, regardless of lastRefresh', () => {
    expect(refreshDetailLabel(false, ZERO_TIME)).toBe('discovery off')
    expect(refreshDetailLabel(false, undefined)).toBe('discovery off')
  })

  it('reads "pending" when discovery is enabled but lastRefresh is still the unset sentinel', () => {
    expect(refreshDetailLabel(true, ZERO_TIME)).toBe('pending')
  })

  it('reads the full formatTimestamp date/time once discovery has completed a refresh, not a relative "(Ns ago)"', () => {
    const iso = new Date(2026, 7, 21, 12, 0, 0).toISOString()
    const got = refreshDetailLabel(true, iso)
    expect(got).not.toMatch(/ago/)
    expect(got).not.toBe('pending')
    expect(got).not.toBe('discovery off')
  })
})

// Feature v0.23: per-model metadata exposure (context window, per-token
// cost). formatContextWindow is table-driven over good/edge cases;
// formatModelCostHover covers the free/priced split ModelChip's
// hover-detail relies on.
describe('formatContextWindow', () => {
  it.each([
    { tokens: 262144, want: '256k' },
    { tokens: 32768, want: '32k' },
    { tokens: 1_000_000, want: '977k' },
    { tokens: 8192, want: '8k' },
    { tokens: 512, want: '512' }, // under 1024: plain digits, no "k"
    { tokens: 0, want: '0' },
    { tokens: 1024, want: '1k' }, // boundary
  ])('formatContextWindow($tokens) = $want', ({ tokens, want }) => {
    expect(formatContextWindow(tokens)).toBe(want)
  })
})

describe('formatModelCostHover', () => {
  it('renders "free" when both sides are exactly 0', () => {
    expect(formatModelCostHover(0, 0)).toBe('free')
  })

  it('renders "in $X / out $Y per MTok" for a priced model', () => {
    expect(formatModelCostHover(1.25, 10)).toBe('in $1.25 / out $10.00 per MTok')
  })

  it('renders a sub-dollar price with two decimals', () => {
    expect(formatModelCostHover(0.19, 0.51)).toBe('in $0.19 / out $0.51 per MTok')
  })
})

// Usage-count compact display (v0.23 addendum): SI-style, 3 significant
// figures, floor-not-round (the same honesty rule formatRatePercent
// already applies to the success-rate badge — a count must never appear
// to have crossed a boundary it has not actually reached).
describe('formatCompactCount', () => {
  it.each([
    // Below 10,000: exact integer with thousands separators.
    { n: 0, want: '0' },
    { n: 999, want: '999' },
    { n: 1_000, want: '1,000' }, // the 999/1000 boundary
    { n: 9_999, want: '9,999' },
    // k tier.
    { n: 10_000, want: '10k' },
    { n: 12_345, want: '12.3k' },
    { n: 456_789, want: '456k' },
    { n: 999_949, want: '999k' }, // floor, not round: stays 999k
    { n: 999_950, want: '999k' }, // the classic round-up edge — floor must NOT bump this to 1,000k
    { n: 999_999, want: '999k' },
    // M tier.
    { n: 1_000_000, want: '1M' },
    { n: 1_234_567, want: '1.23M' },
    { n: 45_600_000, want: '45.6M' },
    { n: 999_999_999, want: '999M' }, // just under the 1e9 boundary
    // B tier.
    { n: 1_000_000_000, want: '1B' }, // the 1e9 boundary itself
    { n: 1_230_000_000, want: '1.23B' },
    { n: 12_345_000_000, want: '12.3B' },
  ])('formatCompactCount($n) = $want', ({ n, want }) => {
    expect(formatCompactCount(n)).toBe(want)
  })

  it('preserves the sign for a negative count (defensive — counts are never negative in practice)', () => {
    expect(formatCompactCount(-15_000_000)).toBe('-15M')
  })
})
