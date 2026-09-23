import { describe, expect, it } from 'vitest'

import { usageCsv } from './usage-csv'
import type { AdminUsageEntryView } from '@/types/api'

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
    'Name,Group,Limits,req/min,req/day,rejected/day,tokIn/day,tokOut/day,tokIn/month,tokOut/month,cost/day (USD),cost/month (USD),"cost/month (proj., USD)",last seen (unix seconds)'

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
  it('renders one data row per entry, as plain numbers (no locale grouping, no currency symbol), including the projected month-end cost and last-seen unix seconds', () => {
    const entries = [testEntry({ id: 'alice', lastSeen: 1_700_000_000 }), testEntry({ id: 'bob' })]
    const csv = usageCsv(entries, 'users', (e) => `group-of-${e.id}`, NOW)
    const lines = csv.split('\r\n')
    // costPerDayMicroUsd 1_234_500 -> 1.234500 USD; costPerMonthMicroUsd
    // 12_345_000 -> 12.345000 USD; projected = 12_345_000 / 0.5 = 24_690_000
    // micro-USD -> 24.690000 USD. bob has no lastSeen -> 0.
    expect(lines[1]).toBe('alice,group-of-alice,none,1,2,7,3,4,5,6,1.234500,12.345000,24.690000,1700000000')
    expect(lines[2]).toBe('bob,group-of-bob,none,1,2,7,3,4,5,6,1.234500,12.345000,24.690000,0')
  })

  it('renders "not enough data yet" for the proj. column when too little of the month has elapsed', () => {
    const earlyInMonth = new Date(Date.UTC(2026, 3, 1, 0, 5, 0))
    const csv = usageCsv([testEntry()], 'users', () => 'g', earlyInMonth)
    expect(csv.split('\r\n')[1]).toBe('alice,g,none,1,2,7,3,4,5,6,1.234500,12.345000,not enough data yet,0')
  })

  it('never masks last-seen as "?" for a storeDown entry — it is its own counter family, independent of the counters storeDown guards', () => {
    const csv = usageCsv([testEntry({ storeDown: true, lastSeen: 1_700_000_000 })], 'users', () => 'g', NOW)
    expect(csv.split('\r\n')[1]).toBe('alice,g,none,?,?,?,?,?,?,?,?,?,?,1700000000')
  })

  it('formats Limits via formatLimits, matching the on-screen table', () => {
    const csv = usageCsv([testEntry({ limits: { requestsPerDay: 100 } })], 'users', () => 'g', NOW)
    expect(csv).toContain('req/day 100')
  })

  it('masks every counter as "?" for a storeDown entry, never a stale number', () => {
    const csv = usageCsv([testEntry({ storeDown: true })], 'users', () => 'g', NOW)
    const dataLine = csv.split('\r\n')[1]
    expect(dataLine).toBe('alice,g,none,?,?,?,?,?,?,?,?,?,?,0')
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
    expect(csv.split('\r\n')[1].split(',')).toHaveLength(14)
  })
})

// modelRankingCsv/modelDetailCsv (and their own describe blocks here)
// were removed with the functions themselves (P3 item 25, dead code) —
// see usage-csv.ts's own doc comment.
