import { describe, expect, it } from 'vitest'

import {
  ADMIN_MAX_KEYS_PER_REQUEST,
  DEFAULT_TOP_USERS_MODE,
  parseTopUsersMode,
  rankTopUsers,
  TOP_USERS_METRICS_BY_MODE,
  TOP_USERS_MODE_LABEL,
  TOP_USERS_MODES,
  topUsersCapCheckResult,
  topUsersCapExceeded,
  topUsersTotalsUrl,
} from './top-users'
import type { AdminTotalsResponse } from '@/types/api'

function row(id: string, values: Partial<Record<'req' | 'tokin' | 'tokout' | 'cost', number>>): AdminTotalsResponse['rows'][number] {
  return { id, values: { req: 0, tokin: 0, tokout: 0, cost: 0, ...values } }
}

describe('TOP_USERS_METRICS_BY_MODE', () => {
  it('requests only the metric(s) each mode ranks by', () => {
    expect(TOP_USERS_METRICS_BY_MODE.cost).toEqual(['cost'])
    expect(TOP_USERS_METRICS_BY_MODE.req).toEqual(['req'])
    expect(TOP_USERS_METRICS_BY_MODE.tokens).toEqual(['tokin', 'tokout'])
  })
})

describe('topUsersTotalsUrl', () => {
  it('builds the cost-mode query string with metrics=cost', () => {
    expect(topUsersTotalsUrl('day', 7, 'cost')).toBe('/admin/api/usage/totals?kind=user&window=day&span=7&offset=0&metrics=cost&limit=1000')
  })

  it('builds the req-mode query string with metrics=req', () => {
    expect(topUsersTotalsUrl('hour', 24, 'req')).toBe('/admin/api/usage/totals?kind=user&window=hour&span=24&offset=0&metrics=req&limit=1000')
  })

  it('builds the tokens-mode query string with metrics=tokin,tokout', () => {
    expect(topUsersTotalsUrl('month', 12, 'tokens')).toBe(
      '/admin/api/usage/totals?kind=user&window=month&span=12&offset=0&metrics=tokin%2Ctokout&limit=1000',
    )
  })

  it('honours an explicit limit override', () => {
    expect(topUsersTotalsUrl('day', 7, 'cost', 50)).toBe('/admin/api/usage/totals?kind=user&window=day&span=7&offset=0&metrics=cost&limit=50')
  })
})

describe('ADMIN_MAX_KEYS_PER_REQUEST', () => {
  it('mirrors the server constant (stats_read.go: adminMaxKeysPerRequest)', () => {
    expect(ADMIN_MAX_KEYS_PER_REQUEST).toBe(64000)
  })
})

describe('topUsersCapExceeded', () => {
  it('mirrors stats_read.go:573 exactly: len(ids)*len(metrics)*span > cap', () => {
    // 1000 users * 1 metric * 48h span = 48000, under the 64000 cap.
    expect(topUsersCapExceeded(1000, 1, 48)).toBe(false)
    // 1334 users * 1 metric * 48h span = 64032, over the cap.
    expect(topUsersCapExceeded(1334, 1, 48)).toBe(true)
  })

  it('is false at exactly the cap (strict >, not >=, matching the server)', () => {
    expect(topUsersCapExceeded(64000, 1, 1)).toBe(false)
    expect(topUsersCapExceeded(64001, 1, 1)).toBe(true)
  })

  it('scales with metrics count — tokens mode (2 metrics) trips sooner than cost/req (1 metric)', () => {
    expect(topUsersCapExceeded(700, 1, 48)).toBe(false)
    expect(topUsersCapExceeded(700, 2, 48)).toBe(true)
  })

  it('is false for zero users, metrics, or span', () => {
    expect(topUsersCapExceeded(0, 1, 48)).toBe(false)
    expect(topUsersCapExceeded(500, 0, 48)).toBe(false)
    expect(topUsersCapExceeded(500, 1, 0)).toBe(false)
  })
})

// verify-ui-states-2.md #4: HomePage.vue's own configuredUserCount is now
// an EXACT count (dashboard.usage.users.length), but is null before that
// store has ever loaded — topUsersCapCheckResult is the wrapper that
// decides what to do with that null case (skip the predictive check
// entirely, rather than treating null as 0 or guessing).
describe('topUsersCapCheckResult', () => {
  it('skips the predictive check (returns false) when configuredUserCount is null', () => {
    // Would trip the cap if it were treated as any real number here.
    expect(topUsersCapCheckResult(null, 2, 48)).toBe(false)
  })

  it('delegates to topUsersCapExceeded once configuredUserCount is a real number', () => {
    expect(topUsersCapCheckResult(1334, 1, 48)).toBe(true)
    expect(topUsersCapCheckResult(1000, 1, 48)).toBe(false)
  })

  it('is false for a genuinely zero configured user count', () => {
    expect(topUsersCapCheckResult(0, 2, 48)).toBe(false)
  })
})

describe('parseTopUsersMode', () => {
  it('parses each valid mode', () => {
    expect(parseTopUsersMode('cost')).toBe('cost')
    expect(parseTopUsersMode('req')).toBe('req')
    expect(parseTopUsersMode('tokens')).toBe('tokens')
  })

  it('falls back to cost for an unknown value', () => {
    expect(parseTopUsersMode('bogus')).toBe('cost')
  })

  it('falls back to cost when undefined', () => {
    expect(parseTopUsersMode(undefined)).toBe(DEFAULT_TOP_USERS_MODE)
  })
})

describe('TOP_USERS_MODES / TOP_USERS_MODE_LABEL', () => {
  it('orders the three modes cost, req, tokens', () => {
    expect(TOP_USERS_MODES).toEqual(['cost', 'req', 'tokens'])
  })

  it('labels every mode', () => {
    expect(TOP_USERS_MODE_LABEL).toEqual({ cost: 'Cost', req: 'Requests', tokens: 'Tokens' })
  })
})

describe('rankTopUsers', () => {
  it('ranks by cost descending', () => {
    const rows = [row('alice', { cost: 5 }), row('bob', { cost: 20 }), row('carl', { cost: 10 })]
    expect(rankTopUsers(rows, 'cost')).toEqual([
      { id: 'bob', value: 20 },
      { id: 'carl', value: 10 },
      { id: 'alice', value: 5 },
    ])
  })

  it('ranks by requests descending', () => {
    const rows = [row('alice', { req: 5, cost: 999 }), row('bob', { req: 20, cost: 1 })]
    expect(rankTopUsers(rows, 'req')).toEqual([
      { id: 'bob', value: 20 },
      { id: 'alice', value: 5 },
    ])
  })

  it('ranks by tokens as tokin+tokout summed, descending', () => {
    const rows = [row('alice', { tokin: 100, tokout: 50 }), row('bob', { tokin: 10, tokout: 10 })]
    expect(rankTopUsers(rows, 'tokens')).toEqual([
      { id: 'alice', value: 150 },
      { id: 'bob', value: 20 },
    ])
  })

  it('breaks ties by id ascending', () => {
    const rows = [row('zed', { cost: 10 }), row('amy', { cost: 10 })]
    expect(rankTopUsers(rows, 'cost')).toEqual([
      { id: 'amy', value: 10 },
      { id: 'zed', value: 10 },
    ])
  })

  it('excludes zero rows for the CURRENT mode only', () => {
    // bob has requests but $0 cost (free-model-only traffic): out of the
    // cost ranking, but present in the requests ranking.
    const rows = [row('alice', { cost: 5, req: 1 }), row('bob', { cost: 0, req: 9 })]
    expect(rankTopUsers(rows, 'cost')).toEqual([{ id: 'alice', value: 5 }])
    expect(rankTopUsers(rows, 'req')).toEqual([
      { id: 'bob', value: 9 },
      { id: 'alice', value: 1 },
    ])
  })

  it('truncates to the limit (default 5)', () => {
    const rows = Array.from({ length: 8 }, (_, i) => row(`user${i}`, { cost: 8 - i }))
    expect(rankTopUsers(rows, 'cost')).toHaveLength(5)
    expect(rankTopUsers(rows, 'cost')[0]).toEqual({ id: 'user0', value: 8 })
  })

  it('honours a custom limit', () => {
    const rows = [row('a', { cost: 3 }), row('b', { cost: 2 }), row('c', { cost: 1 })]
    expect(rankTopUsers(rows, 'cost', 2)).toHaveLength(2)
  })

  it('returns an empty array when every row is zero for the mode', () => {
    const rows = [row('alice', { cost: 0 }), row('bob', { cost: 0 })]
    expect(rankTopUsers(rows, 'cost')).toEqual([])
  })

  it('returns an empty array for no rows', () => {
    expect(rankTopUsers([], 'cost')).toEqual([])
  })
})
