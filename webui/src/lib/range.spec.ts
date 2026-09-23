import { describe, expect, it } from 'vitest'

import {
  comparison,
  dayOrMonthClampNotice,
  dayOrMonthWindow,
  DEFAULT_CMP,
  DEFAULT_RANGE,
  isCmpMode,
  isRangeKey,
  RANGE_KEYS,
  RANGE_PRESETS,
  seriesUrl,
  totalsUrl,
} from './range'

describe('RANGE_PRESETS', () => {
  it('resolves every preset to the documented (window, span) pair', () => {
    expect(RANGE_PRESETS['24h']).toEqual({ window: 'hour', span: 24 })
    expect(RANGE_PRESETS['48h']).toEqual({ window: 'hour', span: 48 })
    expect(RANGE_PRESETS['7d']).toEqual({ window: 'day', span: 7 })
    expect(RANGE_PRESETS['14d']).toEqual({ window: 'day', span: 14 })
    expect(RANGE_PRESETS['30d']).toEqual({ window: 'day', span: 30 })
    expect(RANGE_PRESETS['3mo']).toEqual({ window: 'month', span: 3 })
    expect(RANGE_PRESETS['6mo']).toEqual({ window: 'month', span: 6 })
    expect(RANGE_PRESETS['12mo']).toEqual({ window: 'month', span: 12 })
  })

  it('covers every RangeKey with no gaps', () => {
    for (const key of RANGE_KEYS) expect(RANGE_PRESETS[key]).toBeDefined()
  })
})

describe('DEFAULT_RANGE / DEFAULT_CMP', () => {
  it('defaults to 7d / none', () => {
    expect(DEFAULT_RANGE).toBe('7d')
    expect(DEFAULT_CMP).toBe('none')
  })
})

describe('comparison', () => {
  it.each([
    ['24h', true, 24],
    ['7d', true, 7],
    ['14d', true, 14],
    ['3mo', true, 3],
    ['6mo', true, 6],
  ] as const)('%s is available with offset %i', (range, available, offset) => {
    const result = comparison(range)
    expect(result.available).toBe(available)
    expect(result.offset).toBe(offset)
    expect(result.reason).toBeUndefined()
  })

  it.each(['48h', '30d', '12mo'] as const)('%s is unavailable (2*span exceeds retention max)', (range) => {
    const result = comparison(range)
    expect(result.available).toBe(false)
    expect(result.reason).toBe('retention limit')
  })
})

describe('isRangeKey / isCmpMode', () => {
  it('accepts every real value and rejects bogus ones', () => {
    for (const key of RANGE_KEYS) expect(isRangeKey(key)).toBe(true)
    expect(isRangeKey('bogus')).toBe(false)
    expect(isCmpMode('none')).toBe(true)
    expect(isCmpMode('prev')).toBe(true)
    expect(isCmpMode('bogus')).toBe(false)
  })
})

// dayOrMonthWindow (P2 item 2): the shared clamp GET /admin/api/usage/
// totals?kind=usermodel|modeluser|targetcaller needs — moved here from
// lib/target-columns.ts so stores/consumers.ts (usermodel) and
// stores/spend.ts (usermodel drilldown) share the identical clamp
// TargetCallers.vue already used, rather than each re-deriving it (or,
// before this fix, not clamping at all and 400ing on an hour-window
// range).
describe('dayOrMonthWindow', () => {
  it('passes a day window through unchanged, with its own span', () => {
    expect(dayOrMonthWindow('day', 14)).toEqual({ window: 'day', span: 14 })
  })

  it('passes a month window through unchanged, with its own span', () => {
    expect(dayOrMonthWindow('month', 6)).toEqual({ window: 'month', span: 6 })
  })

  it('clamps an hour window (no day/month equivalent span) to a fixed one-week day window', () => {
    expect(dayOrMonthWindow('hour', 48)).toEqual({ window: 'day', span: 7 })
    expect(dayOrMonthWindow('hour', 24)).toEqual({ window: 'day', span: 7 })
  })
})

// dayOrMonthClampNotice (N3, verify-redesign-final.md): the shared
// "this view silently clamped your range" note text for every reader of
// dayOrMonthWindow above that surfaces the clamp to a screen.
describe('dayOrMonthClampNotice', () => {
  it('is null for a day window — dayOrMonthWindow passes it through unchanged', () => {
    expect(dayOrMonthClampNotice('day', 14)).toBeNull()
  })

  it('is null for a month window — dayOrMonthWindow passes it through unchanged', () => {
    expect(dayOrMonthClampNotice('month', 6)).toBeNull()
  })

  it('names the exact substituted range for an hour window, regardless of the requested span', () => {
    expect(dayOrMonthClampNotice('hour', 24)).toBe('Showing last 7 days — per-hour data is not kept for this view.')
    expect(dayOrMonthClampNotice('hour', 48)).toBe('Showing last 7 days — per-hour data is not kept for this view.')
  })
})

describe('seriesUrl', () => {
  it('repeats scope as separate params and omits offset when 0', () => {
    const url = seriesUrl({ scope: ['user:alice', 'group:eng'], metric: 'cost', window: 'day', span: 7 })
    expect(url).toBe('/admin/api/usage/series?scope=user%3Aalice&scope=group%3Aeng&metric=cost&window=day&span=7')
  })

  it('includes offset when non-zero', () => {
    const url = seriesUrl({ scope: ['total'], metric: 'req', window: 'hour', span: 24, offset: 24 })
    expect(url).toContain('offset=24')
  })
})

describe('totalsUrl', () => {
  it('builds the minimal query for kind alone', () => {
    expect(totalsUrl({ kind: 'user' })).toBe('/admin/api/usage/totals?kind=user')
  })

  it('joins metrics with commas and includes every optional param provided', () => {
    const url = totalsUrl({
      kind: 'usermodel',
      window: 'month',
      span: 1,
      metrics: ['req', 'cost'],
      limit: 5,
      user: 'alice',
    })
    expect(url).toBe('/admin/api/usage/totals?kind=usermodel&window=month&span=1&metrics=req%2Ccost&limit=5&user=alice')
  })
})
