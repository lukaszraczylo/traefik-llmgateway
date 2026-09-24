import { describe, expect, it } from 'vitest'

import { BAR_AMBER_THRESHOLD, BAR_RED_THRESHOLD, budgetRatios, budgetValueText, ratioTier } from './usage-bars'
import type { AdminUsageEntryView } from '@/types/api'

function testEntry(overrides: Partial<AdminUsageEntryView> = {}): AdminUsageEntryView {
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

describe('ratioTier', () => {
  const cases: { name: string; ratio: number; expected: 'normal' | 'amber' | 'red' }[] = [
    { name: 'zero usage', ratio: 0, expected: 'normal' },
    { name: 'just below the amber threshold', ratio: 0.79, expected: 'normal' },
    { name: 'exactly at the amber threshold', ratio: BAR_AMBER_THRESHOLD, expected: 'amber' },
    { name: 'between amber and red', ratio: 0.99, expected: 'amber' },
    { name: 'exactly at the red threshold', ratio: BAR_RED_THRESHOLD, expected: 'red' },
    { name: 'over the limit', ratio: 1.5, expected: 'red' },
    { name: 'negative (defensive)', ratio: -0.2, expected: 'normal' },
    { name: 'NaN (defensive)', ratio: Number.NaN, expected: 'normal' },
  ]

  it.each(cases)('$name -> $expected', ({ ratio, expected }) => {
    expect(ratioTier(ratio)).toBe(expected)
  })
})

describe('budgetRatios', () => {
  it('returns an empty array when the entry has no limits configured', () => {
    expect(budgetRatios(testEntry())).toEqual([])
  })

  it('returns an empty array when limits is present but every field is 0/omitted (LimitsConfig "unlimited" convention)', () => {
    expect(budgetRatios(testEntry({ limits: {} }))).toEqual([])
  })

  it('returns an empty array for a storeDown entry even with limits configured — every counter is stale/unknown', () => {
    expect(budgetRatios(testEntry({ limits: { requestsPerDay: 100 }, requestsPerDay: 50, storeDown: true }))).toEqual([])
  })

  it('includes only the limits actually configured, one BudgetRatio each', () => {
    const ratios = budgetRatios(testEntry({ limits: { requestsPerMinute: 10 }, requestsPerMinute: 5 }))
    expect(ratios).toEqual([{ id: 'reqMin', used: 5, limit: 10, ratio: 0.5 }])
  })

  it('combines tokensInPerDay + tokensOutPerDay into one tokDay bar against tokensPerDay', () => {
    const ratios = budgetRatios(
      testEntry({ limits: { tokensPerDay: 1000 }, tokensInPerDay: 300, tokensOutPerDay: 400 }),
    )
    expect(ratios).toEqual([{ id: 'tokDay', used: 700, limit: 1000, ratio: 0.7 }])
  })

  it('combines tokensInPerMonth + tokensOutPerMonth into one tokMonth bar against tokensPerMonth', () => {
    const ratios = budgetRatios(
      testEntry({ limits: { tokensPerMonth: 2000 }, tokensInPerMonth: 900, tokensOutPerMonth: 300 }),
    )
    expect(ratios).toEqual([{ id: 'tokMonth', used: 1200, limit: 2000, ratio: 0.6 }])
  })

  it('converts costPerDayUSD to micro-USD (Math.round(usd*1e6)) for the costDay bar', () => {
    const ratios = budgetRatios(
      testEntry({ limits: { costPerDayUSD: 0.5 }, costPerDayMicroUsd: 250_000 }),
    )
    expect(ratios).toEqual([{ id: 'costDay', used: 250_000, limit: 500_000, ratio: 0.5 }])
  })

  it('converts costPerMonthUSD to micro-USD for the costMonth bar', () => {
    const ratios = budgetRatios(
      testEntry({ limits: { costPerMonthUSD: 10 }, costPerMonthMicroUsd: 12_000_000 }),
    )
    expect(ratios).toEqual([{ id: 'costMonth', used: 12_000_000, limit: 10_000_000, ratio: 1.2 }])
  })

  // P11 review fix: a configured cost limit tiny enough to round to 0
  // micro-USD must be SKIPPED, exactly like Go does (limits.go/metrics.go:
  // `limit <= 0` skips the check) — never pushed as a ratio dividing by 0.
  it('skips a costDay limit that rounds to 0 micro-USD, instead of dividing by 0', () => {
    const ratios = budgetRatios(testEntry({ limits: { costPerDayUSD: 0.0000001 }, costPerDayMicroUsd: 5 }))
    expect(ratios).toEqual([])
  })

  it('skips a costMonth limit that rounds to 0 micro-USD, instead of dividing by 0', () => {
    const ratios = budgetRatios(testEntry({ limits: { costPerMonthUSD: 0.0000001 }, costPerMonthMicroUsd: 5 }))
    expect(ratios).toEqual([])
  })

  it('returns one BudgetRatio per configured limit, in a fixed order, when every limit is set', () => {
    const ratios = budgetRatios(
      testEntry({
        limits: {
          requestsPerMinute: 10,
          requestsPerDay: 100,
          tokensPerDay: 1000,
          tokensPerMonth: 20000,
          costPerDayUSD: 1,
          costPerMonthUSD: 20,
        },
        requestsPerMinute: 1,
        requestsPerDay: 2,
        tokensInPerDay: 3,
        tokensOutPerDay: 4,
        tokensInPerMonth: 5,
        tokensOutPerMonth: 6,
        costPerDayMicroUsd: 7,
        costPerMonthMicroUsd: 8,
      }),
    )
    expect(ratios.map((r) => r.id)).toEqual(['reqMin', 'reqDay', 'tokDay', 'tokMonth', 'costDay', 'costMonth'])
  })
})

describe('budgetValueText', () => {
  it('formats a cost-kind ratio (id starting "cost") through formatCost, both sides', () => {
    expect(budgetValueText({ id: 'costMonth', used: 1_500_000, limit: 5_000_000, ratio: 0.3 })).toBe('$1.5000 / $5.0000')
  })

  it('formats every other kind through formatExactInt, both sides', () => {
    expect(budgetValueText({ id: 'reqMin', used: 42, limit: 100, ratio: 0.42 })).toBe('42 / 100')
    expect(budgetValueText({ id: 'tokDay', used: 1_500, limit: 2_000, ratio: 0.75 })).toBe('1,500 / 2,000')
  })
})

// estimatedRunOutDate was removed (P2 item 14 — it disagreed with
// lib/burndown.ts's runOutDate on an already-exceeded budget and on day 1
// of the month; UserDetail.vue and BurnDownChart.vue now both call the
// one function in lib/burndown.ts, which owns this coverage —
// lib/burndown.spec.ts's own `describe('runOutDate', ...)`).
