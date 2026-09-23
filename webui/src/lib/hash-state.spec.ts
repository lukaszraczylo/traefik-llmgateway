import { describe, expect, it } from 'vitest'

import {
  buildHash,
  DEFAULT_TAB,
  parseChartsParams,
  parseEventsParams,
  parseHashState,
  parseUsageParams,
} from './hash-state'

describe('parseHashState', () => {
  it('splits a tab and query string', () => {
    const { tab, query } = parseHashState('#charts?window=day&scope=user%3Aalice')
    expect(tab).toBe('charts')
    expect(query.get('window')).toBe('day')
    expect(query.get('scope')).toBe('user:alice')
  })

  it('parses a bare tab with no query to an empty query', () => {
    const { tab, query } = parseHashState('#usage')
    expect(tab).toBe('usage')
    expect(Array.from(query.keys())).toEqual([])
  })

  it('falls back to DEFAULT_TAB for an empty hash', () => {
    expect(parseHashState('').tab).toBe(DEFAULT_TAB)
  })

  it('falls back to DEFAULT_TAB for an unrecognized tab, and still parses its query', () => {
    const { tab, query } = parseHashState('#bogus?window=day')
    expect(tab).toBe(DEFAULT_TAB)
    expect(query.get('window')).toBe('day')
  })

  it('accepts a hash with or without the leading #', () => {
    expect(parseHashState('targets').tab).toBe('targets')
  })

  it('every valid tab round-trips through parseHashState(buildHash(...))', () => {
    for (const tab of ['providers', 'usage', 'charts', 'targets', 'events'] as const) {
      expect(parseHashState(buildHash(tab, {})).tab).toBe(tab)
    }
  })
})

describe('buildHash', () => {
  it('omits the query suffix entirely when params is empty', () => {
    expect(buildHash('providers', {})).toBe('#providers')
  })

  it('appends a query string for non-empty params', () => {
    expect(buildHash('charts', { window: 'day' })).toBe('#charts?window=day')
  })

  it('joins multiple params with &', () => {
    const hash = buildHash('charts', { window: 'day', scope: 'user:alice' })
    expect(hash).toBe('#charts?window=day&scope=user%3Aalice')
  })

  it('drops a param whose value is an explicit empty string', () => {
    expect(buildHash('usage', { q: '' })).toBe('#usage')
  })
})

describe('parseChartsParams', () => {
  it('accepts every valid window/tab/metric value', () => {
    const p = parseChartsParams(new URLSearchParams('window=month&tab=models&metric=tokin'))
    expect(p).toEqual({ window: 'month', tab: 'models', metric: 'tokin' })
  })

  it('omits window when it is not a recognized HistoryWindow', () => {
    expect(parseChartsParams(new URLSearchParams('window=fortnight'))).toEqual({})
  })

  it('omits tab when it is not a recognized ChartTab', () => {
    expect(parseChartsParams(new URLSearchParams('tab=bogus'))).toEqual({})
  })

  it('omits metric when it is not a recognized ModelMetric', () => {
    expect(parseChartsParams(new URLSearchParams('metric=bogus'))).toEqual({})
  })

  it('reads filter when present (free text, not an enum — same convention as scope/q)', () => {
    expect(parseChartsParams(new URLSearchParams('filter=openai%2F'))).toEqual({ filter: 'openai/' })
  })

  it('omits filter when absent', () => {
    expect(parseChartsParams(new URLSearchParams())).toEqual({})
  })

  it('omits filter when it is an explicit empty string', () => {
    expect(parseChartsParams(new URLSearchParams('filter='))).toEqual({})
  })

  it.each([
    ['total', true],
    ['user:alice', true],
    ['group:ops', true],
    ['model:openai/gpt-5', true],
    ['user:', false], // empty id after the colon
    ['bogus', false],
    ['', false],
  ] as const)('scope %s valid=%s', (scope, valid) => {
    const p = parseChartsParams(new URLSearchParams(scope ? `scope=${encodeURIComponent(scope)}` : ''))
    expect(p.scope === scope).toBe(valid)
  })

  it('omits one invalid field while keeping the other valid ones (independent per-field validation)', () => {
    const p = parseChartsParams(new URLSearchParams('window=fortnight&tab=models&scope=total'))
    expect(p).toEqual({ tab: 'models', scope: 'total' })
  })

  it('returns an empty object for an empty query', () => {
    expect(parseChartsParams(new URLSearchParams())).toEqual({})
  })
})

describe('parseUsageParams', () => {
  it('reads q when present', () => {
    expect(parseUsageParams(new URLSearchParams('q=alice'))).toEqual({ q: 'alice' })
  })

  it('omits q when absent', () => {
    expect(parseUsageParams(new URLSearchParams())).toEqual({})
  })

  it('omits q when it is an explicit empty string', () => {
    expect(parseUsageParams(new URLSearchParams('q='))).toEqual({})
  })
})

describe('parseEventsParams', () => {
  it('reads kind and user when both present', () => {
    expect(parseEventsParams(new URLSearchParams('kind=rate_limit&user=bob'))).toEqual({
      kind: 'rate_limit',
      user: 'bob',
    })
  })

  it('accepts a kind this build does not recognize as an AdminEventKind (forward-compat, never rejected)', () => {
    expect(parseEventsParams(new URLSearchParams('kind=some_future_kind'))).toEqual({ kind: 'some_future_kind' })
  })

  it('omits either field independently when absent', () => {
    expect(parseEventsParams(new URLSearchParams('kind=budget'))).toEqual({ kind: 'budget' })
    expect(parseEventsParams(new URLSearchParams('user=alice'))).toEqual({ user: 'alice' })
  })

  it('returns an empty object for an empty query', () => {
    expect(parseEventsParams(new URLSearchParams())).toEqual({})
  })
})
