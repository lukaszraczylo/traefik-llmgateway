import { describe, expect, it } from 'vitest'

import { statusRank } from './breaker-severity'
import { sortThenSlice } from './table-sort'

describe('statusRank', () => {
  it('ranks open > half-open > degraded > closed', () => {
    expect(statusRank('open')).toBeGreaterThan(statusRank('half-open'))
    expect(statusRank('half-open')).toBeGreaterThan(statusRank('degraded'))
    expect(statusRank('degraded')).toBeGreaterThan(statusRank('closed'))
  })
  it('ranks an unknown state below closed so it never reads as severe', () => {
    expect(statusRank('mystery')).toBeLessThan(statusRank('closed'))
  })
  it('sorts severity-first, not alphabetically', () => {
    const rows = ['closed', 'open', 'degraded', 'half-open'].map((s) => ({ s, rank: statusRank(s) }))
    expect(sortThenSlice(rows, { key: 'rank', direction: 'desc' }, 10, 0).map((r) => r.s)).toEqual(['open', 'half-open', 'degraded', 'closed'])
  })
})
