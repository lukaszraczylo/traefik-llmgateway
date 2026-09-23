import { describe, expect, it } from 'vitest'

import { modelDetailCsv, modelRankingCsv, usageCsv } from './usage-csv'
import type { AdminUsageEntryView, AdminUsageModelEntry } from '@/types/api'

function testEntry(overrides: Partial<AdminUsageEntryView> = {}): AdminUsageEntryView {
  return {
    kind: 'user',
    id: 'alice',
    requestsPerMinute: 1,
    requestsPerDay: 2,
    tokensInPerDay: 3,
    tokensOutPerDay: 4,
    tokensInPerMonth: 5,
    tokensOutPerMonth: 6,
    costPerDayMicroUsd: 1_234_500,
    costPerMonthMicroUsd: 12_345_000,
    rejectionsPerDay: 7,
    ...overrides,
  }
}

// Fixed at exactly the midpoint of a UTC 30-day month (April 2026) so
// elapsedFraction is a clean 0.5 — projectMonthEnd(mtd, 0.5) == mtd*2 —
// matching usage-columns.spec.ts's own identical fixture for the on-screen
// costMonthProj column, since usageCsv is meant to mirror it exactly.
const NOW = new Date(Date.UTC(2026, 3, 16, 0, 0, 0))

describe('usageCsv', () => {
  const HEADER =
    'Name,Group,Limits,req/min,req/day,rejected/day,tokIn/day,tokOut/day,tokIn/month,tokOut/month,cost/day (USD),cost/month (USD),"cost/month (proj., USD)"'

  it('renders the Users header row with "Group" as the secondary column', () => {
    const csv = usageCsv([], 'users', () => '', NOW)
    expect(csv).toBe(`${HEADER}\r\n`)
  })

  it('renders the Groups header row with "Members" as the secondary column', () => {
    const csv = usageCsv([], 'groups', () => '', NOW)
    expect(csv).toBe(`${HEADER.replace('Group', 'Members')}\r\n`)
  })

  // P6 review fix: every numeric cell is now a PLAIN machine-readable
  // number — no thousands separators (formatExactInt) and no "$" prefix
  // (formatCost) — and the trailing cost/month (proj.) column is present.
  it('renders one data row per entry, as plain numbers (no locale grouping, no currency symbol), including the projected month-end cost', () => {
    const entries = [testEntry({ id: 'alice' }), testEntry({ id: 'bob' })]
    const csv = usageCsv(entries, 'users', (e) => `group-of-${e.id}`, NOW)
    const lines = csv.split('\r\n')
    // costPerDayMicroUsd 1_234_500 -> 1.234500 USD; costPerMonthMicroUsd
    // 12_345_000 -> 12.345000 USD; projected = 12_345_000 / 0.5 = 24_690_000
    // micro-USD -> 24.690000 USD.
    expect(lines[1]).toBe('alice,group-of-alice,none,1,2,7,3,4,5,6,1.234500,12.345000,24.690000')
    expect(lines[2]).toBe('bob,group-of-bob,none,1,2,7,3,4,5,6,1.234500,12.345000,24.690000')
  })

  it('renders "not enough data yet" for the proj. column when too little of the month has elapsed', () => {
    const earlyInMonth = new Date(Date.UTC(2026, 3, 1, 0, 5, 0))
    const csv = usageCsv([testEntry()], 'users', () => 'g', earlyInMonth)
    expect(csv.split('\r\n')[1]).toBe('alice,g,none,1,2,7,3,4,5,6,1.234500,12.345000,not enough data yet')
  })

  it('formats Limits via formatLimits, matching the on-screen table', () => {
    const csv = usageCsv([testEntry({ limits: { requestsPerDay: 100 } })], 'users', () => 'g', NOW)
    expect(csv).toContain('req/day 100')
  })

  it('masks every counter as "?" for a storeDown entry, never a stale number', () => {
    const csv = usageCsv([testEntry({ storeDown: true })], 'users', () => 'g', NOW)
    const dataLine = csv.split('\r\n')[1]
    expect(dataLine).toBe('alice,g,none,?,?,?,?,?,?,?,?,?,?')
  })

  it('quotes a field that needs it (e.g. a secondaryValue containing a comma)', () => {
    const csv = usageCsv([testEntry()], 'users', () => 'a, b', NOW)
    expect(csv).toContain('"a, b"')
  })

  it('defaults `now` to the real current time when omitted (production call sites)', () => {
    // Just asserts it does not throw and produces a well-formed row — the
    // exact projected value depends on the real clock, covered precisely
    // by the explicit-`now` tests above.
    const csv = usageCsv([testEntry()], 'users', () => 'g')
    expect(csv.split('\r\n')[1].split(',')).toHaveLength(13)
  })
})

describe('modelRankingCsv', () => {
  const models: AdminUsageModelEntry[] = [
    { id: 'openai/gpt-4', value: 100 },
    { id: 'anthropic/claude', value: 50 },
  ]

  it('renders a header naming the ranked metric, window, and span (quoted — the label itself contains a comma)', () => {
    const csv = modelRankingCsv(models, 'Cost', '24h', 24)
    expect(csv.split('\r\n')[0]).toBe('Model,"Cost (24h, span 24)"')
  })

  it('renders one row per model with its raw value when isCostMetric is omitted/false', () => {
    const csv = modelRankingCsv(models, 'Requests', '30d', 30)
    const lines = csv.split('\r\n')
    expect(lines[1]).toBe('openai/gpt-4,100')
    expect(lines[2]).toBe('anthropic/claude,50')
  })

  it('renders just the header for an empty ranking', () => {
    expect(modelRankingCsv([], 'Cost', '24h', 24)).toBe('Model,"Cost (24h, span 24)"\r\n')
  })

  // P6 review fix: a cost-metric ranking's `value` is raw micro-USD with no
  // unit stated anywhere — isCostMetric:true states the unit in the header
  // and converts each value to a plain decimal USD number.
  it('states the USD unit in the header and converts raw micro-USD values to plain decimal USD when isCostMetric is true', () => {
    const costModels: AdminUsageModelEntry[] = [{ id: 'openai/gpt-4', value: 1_234_500 }]
    const csv = modelRankingCsv(costModels, 'Cost', '24h', 24, true)
    const lines = csv.split('\r\n')
    expect(lines[0]).toBe('Model,"Cost (USD) (24h, span 24)"')
    expect(lines[1]).toBe('openai/gpt-4,1.234500')
  })
})

// free-models-plan.md: the Models tab's CSV export now includes every
// detail=1 figure per row (Requests/Tokens in/Tokens out/Cost), plus a
// separate Free flag column — not just the single ranked metric
// modelRankingCsv above exports.
describe('modelDetailCsv', () => {
  it('renders a header naming every detail column, with window/span folded into Requests only', () => {
    const csv = modelDetailCsv([], '24h', 24)
    expect(csv).toBe('Model,"Requests (24h, span 24)",Tokens in,Tokens out,Cost (USD),Free\r\n')
  })

  it('renders one row per model with every detail figure as a plain machine-readable number', () => {
    const models: AdminUsageModelEntry[] = [
      { id: 'openai/gpt-4', value: 100, requests: 10, tokensIn: 1000, tokensOut: 2000, costMicroUsd: 1_234_500, free: false },
    ]
    const csv = modelDetailCsv(models, '24h', 24)
    expect(csv.split('\r\n')[1]).toBe('openai/gpt-4,10,1000,2000,1.234500,false')
  })

  it('exports the real 0 cost (never the string "free") in the Cost column, with Free as its own "true" column', () => {
    const models: AdminUsageModelEntry[] = [
      { id: 'uni/free-model', value: 0, requests: 5, tokensIn: 50, tokensOut: 75, costMicroUsd: 0, free: true },
    ]
    const csv = modelDetailCsv(models, '24h', 24)
    expect(csv.split('\r\n')[1]).toBe('uni/free-model,5,50,75,0.000000,true')
  })

  it('falls back every missing detail field to 0/false when detail was not actually requested (defensive)', () => {
    const models: AdminUsageModelEntry[] = [{ id: 'openai/gpt-4', value: 100 }]
    const csv = modelDetailCsv(models, '24h', 24)
    expect(csv.split('\r\n')[1]).toBe('openai/gpt-4,0,0,0,0.000000,false')
  })

  it('renders just the header for an empty ranking', () => {
    expect(modelDetailCsv([], '30d', 30)).toBe('Model,"Requests (30d, span 30)",Tokens in,Tokens out,Cost (USD),Free\r\n')
  })
})
