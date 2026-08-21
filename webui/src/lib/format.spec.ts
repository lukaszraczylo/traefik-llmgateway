import { describe, expect, it } from 'vitest'

import { refreshLabel } from './format'

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
