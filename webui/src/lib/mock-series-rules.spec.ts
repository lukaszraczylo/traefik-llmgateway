import { describe, expect, it } from 'vitest'

import manifestJson from '../../mock/manifest.json'
import type { Fixture } from '../../mock/match'
import { respond } from '../../mock/respond'
import { parseSeriesScope, seriesQueryError, seriesValidMetric, type SeriesScopeKind } from '../../mock/series-rules'
import { CAPTURE_AT } from '../../mock/time-shift'

const ALL_METRICS = ['req', 'tokin', 'tokout', 'cost', 'rej', 'attempt', 'fail', 'timeout', 'fover', 'r402', 'chit', 'cmiss', 'csave']
const WINDOWS = ['hour', 'day', 'month']
const BASE = ['req', 'tokin', 'tokout', 'cost']

// Valid metrics per scope kind and window, transcribed from stats_read.go's seriesValidMetric switch.
const VALID: Record<SeriesScopeKind, Record<string, string[]>> = {
  user: { hour: [...BASE, 'rej'], day: [...BASE, 'rej'], month: BASE },
  group: { hour: [...BASE, 'rej'], day: [...BASE, 'rej'], month: BASE },
  total: {
    hour: [...BASE, 'rej', 'chit', 'cmiss', 'csave', 'r402'],
    day: [...BASE, 'rej', 'chit', 'cmiss', 'csave', 'r402'],
    month: BASE,
  },
  model: { hour: BASE, day: [...BASE, 'chit', 'csave', 'r402'], month: BASE },
  prov: { hour: ['attempt', 'fail', 'timeout', 'fover'], day: ['attempt', 'fail', 'timeout', 'fover'], month: [] },
  provmodel: { hour: ['attempt', 'fail'], day: ['attempt', 'fail'], month: [] },
}

describe('seriesValidMetric', () => {
  for (const [kind, byWindow] of Object.entries(VALID) as [SeriesScopeKind, Record<string, string[]>][]) {
    for (const window of WINDOWS) {
      it(`${kind} @ ${window} accepts exactly ${(byWindow[window] ?? []).join(',') || 'nothing'}`, () => {
        for (const metric of ALL_METRICS) {
          expect(seriesValidMetric(kind, metric, window), `${kind}/${metric}/${window}`).toBe((byWindow[window] ?? []).includes(metric))
        }
      })
    }
  }

  it('rejects a metric the backend does not know', () => {
    expect(seriesValidMetric('total', 'bogus', 'day')).toBe(false)
  })
})

describe('parseSeriesScope', () => {
  it.each([
    ['total', 'total'],
    ['user:alice', 'user'],
    ['group:friends', 'group'],
    ['model:openai/gpt-5', 'model'],
    ['provider:openai', 'prov'],
    ['provmodel:openai/gpt-5', 'provmodel'],
    ['user:', null],
    ['bogus:x', null],
    ['alice', null],
  ])('%s -> %s', (raw, kind) => expect(parseSeriesScope(raw)).toBe(kind))
})

describe('seriesQueryError', () => {
  const q = (s: string) => new URLSearchParams(s)
  it.each([
    ['metric=req&window=day', 'at least one scope is required'],
    ['scope=total&metric=req', 'unknown window'],
    ['scope=total&metric=req&window=min', 'unknown window'],
    ['scope=total&metric=nope&window=day', 'unknown metric'],
    ['scope=total&metric=req&window=hour&span=49', 'invalid span'],
    ['scope=total&metric=req&window=day&span=36', 'invalid span'],
    ['scope=total&metric=req&window=month&span=14', 'invalid span'],
    ['scope=total&metric=req&window=day&span=0', 'invalid span'],
    ['scope=total&metric=req&window=day&span=x', 'invalid span'],
    ['scope=total&metric=req&window=day&span=7&offset=29', 'invalid offset'],
    ['scope=total&metric=req&window=day&span=7&offset=-1', 'invalid offset'],
    ['scope=provider:openai&metric=req&window=day&span=7', 'unknown metric for scope kind'],
    ['scope=provider:openai&metric=attempt&window=month&span=3', 'unknown metric for scope kind'],
    ['scope=total&scope=provider:openai&metric=req&window=day&span=7', 'unknown metric for scope kind'],
  ])('%s -> %s', (query, message) => expect(seriesQueryError(q(query))).toBe(message))

  it.each([
    'scope=provider:openai&metric=attempt&window=day&span=30',
    'scope=total&metric=req&window=month&span=13',
    'scope=total&metric=req&window=day&span=7&offset=28',
    'scope=total&metric=req&window=day',
    'scope=bogus:x&metric=req&window=day',
  ])('accepts %s', (query) => expect(seriesQueryError(q(query))).toBeNull())

  it('rejects more than 100 scopes', () => {
    const p = new URLSearchParams('metric=req&window=day&span=1')
    for (let i = 0; i < 101; i++) p.append('scope', `user:u${i}`)
    expect(seriesQueryError(p)).toBe('too many scopes')
  })
})

describe('mock usage/series handler', () => {
  const manifest = manifestJson as Fixture[]
  const get = (query: string) =>
    respond({ manifest, readFixture: () => ({}), path: '/admin/api/usage/series', query: new URLSearchParams(query), scenarios: [], now: CAPTURE_AT, rng: () => 0.5 })

  it('answers an invalid scope/metric combination with the backend 400 body', () => {
    const r = get('scope=provider:openai&metric=req&window=day&span=7')
    expect(r.status).toBe(400)
    expect(r.body).toEqual({ error: { message: 'unknown metric for scope kind', type: 'invalid_request_error', code: '400' } })
  })
})
