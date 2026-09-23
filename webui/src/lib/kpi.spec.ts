import { describe, expect, it } from 'vitest'

import {
  budgetRatio,
  budgetTier,
  errorRateTier,
  fleetErrorRate,
  openBreakerCount,
  sumGroupBudgetMicros,
} from './kpi'
import type { AdminGroupView, AdminProviderView } from '@/types/api'

function makeGroup(overrides: Partial<AdminGroupView> = {}): AdminGroupView {
  return { name: 'eng', memberCount: 3, ...overrides }
}

function makeProvider(overrides: Partial<AdminProviderView> = {}): AdminProviderView {
  return {
    name: 'openai',
    type: 'openai',
    baseUrl: 'https://api.openai.com',
    lastRefresh: '0001-01-01T00:00:00Z',
    models: [],
    modelCount: 0,
    modelMeta: {},
    discoveryEnabled: false,
    healthState: 'closed',
    openUntil: '0001-01-01T00:00:00Z',
    attemptsDay: 0,
    failuresDay: 0,
    attemptsMinute: 0,
    failuresMinute: 0,
    modelRates: {},
    ...overrides,
  }
}

describe('sumGroupBudgetMicros', () => {
  it('sums every group\'s costPerMonthUSD limit, converted to micro-USD', () => {
    const groups = [
      makeGroup({ limits: { costPerMonthUSD: 10 } }),
      makeGroup({ limits: { costPerMonthUSD: 25.5 } }),
    ]
    expect(sumGroupBudgetMicros(groups)).toBe(35_500_000)
  })

  it('skips a group with no costPerMonthUSD configured', () => {
    const groups = [makeGroup({ limits: undefined }), makeGroup({ limits: { costPerMonthUSD: 0 } })]
    expect(sumGroupBudgetMicros(groups)).toBe(0)
  })

  it('returns 0 for an empty group list', () => {
    expect(sumGroupBudgetMicros([])).toBe(0)
  })
})

describe('budgetRatio / budgetTier', () => {
  it('returns null when budgetMicros is 0 ("no budget set")', () => {
    expect(budgetRatio(5_000_000, 0)).toBeNull()
    expect(budgetTier(null)).toBe('ok')
  })

  it('classifies below 0.8 as ok, [0.8, 1.0) as warn, >= 1.0 as critical', () => {
    expect(budgetTier(budgetRatio(700_000, 1_000_000))).toBe('ok')
    expect(budgetTier(budgetRatio(900_000, 1_000_000))).toBe('warn')
    expect(budgetTier(budgetRatio(1_200_000, 1_000_000))).toBe('critical')
  })
})

describe('openBreakerCount', () => {
  it('counts providers whose breaker is not closed', () => {
    const providers = [
      makeProvider({ healthState: 'closed' }),
      makeProvider({ healthState: 'open' }),
      makeProvider({ healthState: 'half-open' }),
    ]
    expect(openBreakerCount(providers)).toBe(2)
  })

  it('returns 0 when every breaker is closed', () => {
    expect(openBreakerCount([makeProvider(), makeProvider()])).toBe(0)
  })
})

describe('fleetErrorRate / errorRateTier', () => {
  it('sums attemptsMinute/failuresMinute across every provider', () => {
    const providers = [
      makeProvider({ attemptsMinute: 100, failuresMinute: 1 }),
      makeProvider({ attemptsMinute: 50, failuresMinute: 4 }),
    ]
    const result = fleetErrorRate(providers)
    expect(result.attempts).toBe(150)
    expect(result.failures).toBe(5)
    expect(result.rate).toBeCloseTo(5 / 150)
  })

  it('rate is null when there were no attempts (never a fabricated 0%)', () => {
    const result = fleetErrorRate([makeProvider({ attemptsMinute: 0, failuresMinute: 0 })])
    expect(result.rate).toBeNull()
    expect(errorRateTier(result.rate)).toBe('ok')
  })

  it('classifies error rate into ok/warn/critical using the shared provider-rate thresholds', () => {
    expect(errorRateTier(0.001)).toBe('ok') // 99.9% success
    expect(errorRateTier(0.05)).toBe('warn') // 95% success, below the 99% amber threshold
    expect(errorRateTier(0.2)).toBe('critical') // 80% success, below the 90% destructive threshold
  })
})
