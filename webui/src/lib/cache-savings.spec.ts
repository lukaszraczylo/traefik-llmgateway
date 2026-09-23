import { describe, expect, it } from 'vitest'

import { cacheSavingsStatus } from './cache-savings'

describe('cacheSavingsStatus (N2)', () => {
  it('is unavailable for an hour window, with a reason naming the granularity, not a fabricated $0', () => {
    const status = cacheSavingsStatus('hour')
    expect(status.available).toBe(false)
    expect(status.note).toMatch(/hour/i)
  })

  it('is available with no caveat for a day window (exact figure)', () => {
    expect(cacheSavingsStatus('day')).toEqual({ available: true, note: null })
  })

  it('is available but notes the 35-day retention cap for a month window', () => {
    const status = cacheSavingsStatus('month')
    expect(status.available).toBe(true)
    expect(status.note).toMatch(/35/)
  })
})
