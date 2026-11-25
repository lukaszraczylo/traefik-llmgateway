import { describe, expect, it } from 'vitest'

import { effectiveLimitRows, isHot, meterModel, primaryLimit } from '@/lib/meters'
import type { LimitsConfig } from '@/types/api'

const PACING = { startPct: 80, maxDelay: '10s' }
const LIMITS: LimitsConfig = { requestsPerMinute: 30, requestsPerDay: 3000, tokensPerMinute: 1_200_000, pacing: PACING }
const ENTRY = { requestsPerMinute: 27, requestsPerDay: 1500, tokensPerMinute: 900_000 }

describe('meterModel with the usage store down', () => {
  it('marks every meter unavailable rather than reporting a zero reading', () => {
    const meters = meterModel({ ...ENTRY, storeDown: true }, LIMITS)
    expect(meters.length).toBe(3)
    expect(meters.every((m) => m.unavailable === true)).toBe(true)
  })

  it('is never hot when the readings are unknown', () => {
    expect(isHot({ ...ENTRY, storeDown: true }, LIMITS)).toBe(false)
  })

  it('leaves live meters without the unavailable flag', () => {
    expect(meterModel(ENTRY, LIMITS).some((m) => m.unavailable)).toBe(false)
  })
})

describe('meterModel', () => {
  it('requests meter has pacing marker at startPct and limit marker', () => {
    const m = meterModel(ENTRY, LIMITS).find((x) => x.label === 'Requests/min')
    expect(m).toEqual({
      label: 'Requests/min',
      value: 27,
      max: 30,
      markers: [
        { value: 24, label: 'pacing 80%', tone: 'warning' },
        { value: 30, label: 'limit', tone: 'danger' },
      ],
    })
  })

  it('absent tokensPerMinute shows limit only', () => {
    const m = meterModel({ requestsPerMinute: 1, requestsPerDay: 2 }, LIMITS).find((x) => x.label === 'Tokens/min')
    expect(m).toEqual({ label: 'Tokens/min', value: 0, max: 1_200_000, markers: [{ value: 1_200_000, label: 'limit', tone: 'danger' }] })
  })

  it('no limits -> []', () => {
    expect(meterModel(ENTRY, undefined)).toEqual([])
    expect(meterModel(ENTRY, {})).toEqual([])
  })

  it('day meter carries the limit only, never pacing', () => {
    const m = meterModel(ENTRY, LIMITS).find((x) => x.label === 'Requests/day')
    expect(m?.markers).toEqual([{ value: 3000, label: 'limit', tone: 'danger' }])
  })

  it('without pacing the minute meters have only the limit marker', () => {
    const m = meterModel(ENTRY, { requestsPerMinute: 30 })
    expect(m).toHaveLength(1)
    expect(m[0]?.markers).toEqual([{ value: 30, label: 'limit', tone: 'danger' }])
  })

  it('lists the meters in a fixed order', () => {
    expect(meterModel(ENTRY, LIMITS).map((m) => m.label)).toEqual(['Requests/min', 'Requests/day', 'Tokens/min'])
  })

  it('treats a zero limit as unset', () => {
    expect(meterModel(ENTRY, { requestsPerMinute: 0, requestsPerDay: 0 })).toEqual([])
  })
})

describe('isHot', () => {
  it('is hot once a minute meter reaches the pacing threshold', () => {
    expect(isHot({ requestsPerMinute: 24, requestsPerDay: 0 }, LIMITS)).toBe(true)
    expect(isHot({ requestsPerMinute: 23, requestsPerDay: 0 }, LIMITS)).toBe(false)
  })
  it('reads live tokens too', () => {
    expect(isHot({ requestsPerMinute: 0, requestsPerDay: 0, tokensPerMinute: 1_000_000 }, LIMITS)).toBe(true)
  })
  it('falls back to 80% without pacing and is false without limits', () => {
    expect(isHot({ requestsPerMinute: 8, requestsPerDay: 0 }, { requestsPerMinute: 10 })).toBe(true)
    expect(isHot({ requestsPerMinute: 7, requestsPerDay: 0 }, { requestsPerMinute: 10 })).toBe(false)
    expect(isHot({ requestsPerMinute: 99, requestsPerDay: 0 }, undefined)).toBe(false)
  })
})

describe('primaryLimit', () => {
  it('prefers requests/min, then tokens/min, then requests/day', () => {
    expect(primaryLimit(LIMITS)).toEqual({ limit: 30, unit: 'min' })
    expect(primaryLimit({ tokensPerMinute: 5 })).toEqual({ limit: 5, unit: 'tok/min' })
    expect(primaryLimit({ requestsPerDay: 9 })).toEqual({ limit: 9, unit: 'day' })
    expect(primaryLimit(undefined)).toEqual({ limit: null, unit: '' })
  })
})

describe('effectiveLimitRows', () => {
  it('labels each limit with where it came from', () => {
    const rows = effectiveLimitRows(LIMITS, { requestsPerMinute: 'memberLimits:friends', requestsPerDay: 'user' })
    expect(rows.find((r) => r.label === 'Requests/min')).toMatchObject({ value: '30', source: 'group friends (per member)' })
    expect(rows.find((r) => r.label === 'Requests/day')).toMatchObject({ value: '3,000', source: 'user' })
    expect(rows.find((r) => r.label === 'Pacing')).toMatchObject({ value: 'from 80%, up to 10s delay' })
  })
  it('formats cost limits with the shared cost format and marks pacing as prose', () => {
    const rows = effectiveLimitRows({ costPerMonthUSD: 25, pacing: PACING }, undefined)
    expect(rows.find((r) => r.key === 'costPerMonthUSD')?.value).toBe('$25.00')
    expect(rows.find((r) => r.key === 'pacing')?.prose).toBe(true)
  })
  it('returns no rows without limits', () => {
    expect(effectiveLimitRows(undefined, undefined)).toEqual([])
  })
})
