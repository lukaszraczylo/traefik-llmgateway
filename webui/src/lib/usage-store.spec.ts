import { describe, expect, it } from 'vitest'

import { countOrUnknown, formatCount } from '@/lib/usage-store'

describe('usage-store', () => {
  it('hides the counter when the store is down', () => {
    expect(countOrUnknown(0, true)).toBeNull()
    expect(countOrUnknown(12, undefined)).toBe(12)
  })

  it('renders an unknown counter as a dash, never 0', () => {
    expect(formatCount(null)).toBe('—')
    expect(formatCount(0)).toBe('0')
  })
})
