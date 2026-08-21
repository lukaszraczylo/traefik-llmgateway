import { describe, expect, it } from 'vitest'

import { refreshDetailLabel, refreshLabel } from './format'

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
