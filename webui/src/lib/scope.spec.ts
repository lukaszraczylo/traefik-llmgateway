import { describe, expect, it } from 'vitest'

import { drilldownLevel, makeScope, parseScope, scopeBudgetLimits, scopeId } from './scope'
import type { AdminGroupView, AdminUsageEntryView } from '@/types/api'

function group(overrides: Partial<AdminGroupView> = {}): AdminGroupView {
  return { name: 'eng', memberCount: 1, ...overrides }
}

function user(overrides: Partial<AdminUsageEntryView> = {}): AdminUsageEntryView {
  return {
    kind: 'user',
    id: 'alice',
    requestsPerMinute: 0,
    requestsPerDay: 0,
    tokensInPerDay: 0,
    tokensOutPerDay: 0,
    tokensInPerMonth: 0,
    tokensOutPerMonth: 0,
    costPerDayMicroUsd: 0,
    costPerMonthMicroUsd: 0,
    rejectionsPerDay: 0,
    ...overrides,
  }
}

describe('parseScope', () => {
  it.each([
    ['group:eng', { kind: 'group', id: 'eng' }],
    ['user:alice', { kind: 'user', id: 'alice' }],
    ['model:gpt-4', { kind: 'model', id: 'gpt-4' }],
    ['provider:openai', { kind: 'provider', id: 'openai' }],
  ] as const)('splits %s into its kind and id', (input, expected) => {
    expect(parseScope(input)).toEqual(expected)
  })

  it('returns null for a string with no recognized "kind:" prefix', () => {
    expect(parseScope('all')).toBeNull()
    expect(parseScope('total')).toBeNull()
    expect(parseScope('')).toBeNull()
  })

  it('splits only on the FIRST colon, keeping the rest of the id intact', () => {
    expect(parseScope('model:openai/gpt-4:latest')).toEqual({ kind: 'model', id: 'openai/gpt-4:latest' })
  })
})

describe('scopeId', () => {
  it('strips the "kind:" prefix', () => {
    expect(scopeId('group:eng')).toBe('eng')
    expect(scopeId('provider:openai')).toBe('openai')
  })

  it('returns the string unchanged when it carries no recognized prefix', () => {
    expect(scopeId('all')).toBe('all')
    expect(scopeId('total')).toBe('total')
  })
})

describe('makeScope', () => {
  it('is parseScope\'s own inverse', () => {
    expect(makeScope('group', 'eng')).toBe('group:eng')
    expect(parseScope(makeScope('user', 'alice'))).toEqual({ kind: 'user', id: 'alice' })
  })
})

describe('drilldownLevel', () => {
  it('reads "" as the group level', () => {
    expect(drilldownLevel('')).toEqual({ kind: 'group' })
  })

  it('reads "group:x" as the user level, naming the parent group', () => {
    expect(drilldownLevel('group:eng')).toEqual({ kind: 'user', group: 'eng' })
  })

  it('reads "user:x" as the usermodel level, naming the user', () => {
    expect(drilldownLevel('user:alice')).toEqual({ kind: 'usermodel', user: 'alice' })
  })

  it('reads any other non-empty string as the usermodel level verbatim (defensive fallback)', () => {
    expect(drilldownLevel('alice')).toEqual({ kind: 'usermodel', user: 'alice' })
  })
})

describe('scopeBudgetLimits', () => {
  const groups = [group({ name: 'eng', limits: { costPerMonthUSD: 100 } }), group({ name: 'ops', limits: { costPerMonthUSD: 50 } })]
  const users = [user({ id: 'alice', limits: { costPerMonthUSD: 10 } })]

  it('sums every configured group\'s own budget for "all" (or any other unscoped root string)', () => {
    expect(scopeBudgetLimits('all', groups, users)).toEqual({ costPerMonthUSD: 150 })
    expect(scopeBudgetLimits('total', groups, users)).toEqual({ costPerMonthUSD: 150 })
  })

  it('returns undefined for the fleet-wide sum when no group has a budget configured', () => {
    expect(scopeBudgetLimits('all', [group({ limits: undefined })], [])).toBeUndefined()
  })

  it('resolves a group scope to that one group\'s own limits', () => {
    expect(scopeBudgetLimits('group:eng', groups, users)).toEqual({ costPerMonthUSD: 100 })
  })

  it('resolves a user scope to that one user\'s own limits', () => {
    expect(scopeBudgetLimits('user:alice', groups, users)).toEqual({ costPerMonthUSD: 10 })
  })

  it('returns undefined for a group/user scope this fleet does not actually have', () => {
    expect(scopeBudgetLimits('group:nope', groups, users)).toBeUndefined()
    expect(scopeBudgetLimits('user:nope', groups, users)).toBeUndefined()
  })

  it('returns undefined for a provider scope (no single group/user limit applies)', () => {
    expect(scopeBudgetLimits('provider:openai', groups, users)).toBeUndefined()
  })
})
