import { describe, expect, it } from 'vitest'

import { PAGE_IDS } from './pages'
import { buildHash, parseGlobalParams, parseHashState, parsePageParams } from './hash-state'

describe('parseHashState', () => {
  it('splits a page and query string', () => {
    const { page, query } = parseHashState('#spend?range=30d&by=model')
    expect(page).toBe('spend')
    expect(query.get('range')).toBe('30d')
    expect(query.get('by')).toBe('model')
  })

  it('parses a bare page with no query to an empty query', () => {
    const { page, query } = parseHashState('#consumers')
    expect(page).toBe('consumers')
    expect(Array.from(query.keys())).toEqual([])
  })

  it('falls back to DEFAULT_PAGE ("home") for an empty hash', () => {
    expect(parseHashState('').page).toBe('home')
  })

  it('falls back to DEFAULT_PAGE for an unrecognized, non-legacy page, and still parses its query', () => {
    const { page, query } = parseHashState('#bogus?range=30d')
    expect(page).toBe('home')
    expect(query.get('range')).toBe('30d')
  })

  it('accepts a hash with or without the leading #', () => {
    expect(parseHashState('targets').page).toBe('targets')
  })

  it('every current page id round-trips through parseHashState(buildHash(...))', () => {
    for (const page of PAGE_IDS) {
      expect(parseHashState(buildHash(page, {})).page).toBe(page)
    }
  })

  describe('legacy tab-name mapping', () => {
    it.each([
      ['providers', 'models'],
      ['usage', 'consumers'],
      ['charts', 'spend'],
      ['events', 'reliability'],
      ['targets', 'targets'],
    ] as const)('#%s maps to #%s', (legacy, mapped) => {
      expect(parseHashState(`#${legacy}`).page).toBe(mapped)
    })

    it('drops the old hash\'s query params entirely, regardless of shape', () => {
      const { page, query } = parseHashState('#charts?tab=models&metric=cost&scope=user%3Aalice')
      expect(page).toBe('spend')
      expect(Array.from(query.keys())).toEqual([])
    })
  })
})

describe('buildHash', () => {
  it('omits the query suffix entirely when params is empty', () => {
    expect(buildHash('home', {})).toBe('#home')
  })

  it('appends a query string for non-empty params', () => {
    expect(buildHash('spend', { range: '30d' })).toBe('#spend?range=30d')
  })

  it('joins multiple params with &, in insertion order', () => {
    const hash = buildHash('spend', { range: '30d', cmp: 'prev', scope: 'user:alice' })
    expect(hash).toBe('#spend?range=30d&cmp=prev&scope=user%3Aalice')
  })

  it('drops a param whose value is an explicit empty string', () => {
    expect(buildHash('consumers', { q: '' })).toBe('#consumers')
  })
})

describe('parseGlobalParams', () => {
  it('accepts every valid range/cmp/scope value', () => {
    const p = parseGlobalParams(new URLSearchParams('range=30d&cmp=prev&scope=group:eng'))
    expect(p).toEqual({ range: '30d', cmp: 'prev', scope: 'group:eng' })
  })

  it('omits range when it is not a recognized RangeKey', () => {
    expect(parseGlobalParams(new URLSearchParams('range=fortnight'))).toEqual({})
  })

  it('omits cmp when it is not "none" or "prev"', () => {
    expect(parseGlobalParams(new URLSearchParams('cmp=bogus'))).toEqual({})
  })

  it.each([
    ['all', true],
    ['group:eng', true],
    ['user:alice', true],
    ['provider:openai', true],
    ['user:', false],
    ['bogus', false],
    ['', false],
  ] as const)('scope %s valid=%s', (scope, valid) => {
    const p = parseGlobalParams(new URLSearchParams(scope ? `scope=${encodeURIComponent(scope)}` : ''))
    expect(p.scope === scope).toBe(valid)
  })

  it('omits one invalid field while keeping the other valid ones', () => {
    const p = parseGlobalParams(new URLSearchParams('range=fortnight&cmp=prev&scope=all'))
    expect(p).toEqual({ cmp: 'prev', scope: 'all' })
  })

  it('returns an empty object for an empty query', () => {
    expect(parseGlobalParams(new URLSearchParams())).toEqual({})
  })
})

describe('parsePageParams', () => {
  it('excludes range/cmp/scope but keeps every other key', () => {
    const p = parsePageParams(new URLSearchParams('range=30d&cmp=prev&scope=all&by=model&metric=cost'))
    expect(p).toEqual({ by: 'model', metric: 'cost' })
  })

  it('drops a key with an explicit empty-string value', () => {
    expect(parsePageParams(new URLSearchParams('q='))).toEqual({})
  })

  it('passes through a key this build does not recognize (forward-compat)', () => {
    expect(parsePageParams(new URLSearchParams('someFutureParam=x'))).toEqual({ someFutureParam: 'x' })
  })

  it('returns an empty object for an empty query', () => {
    expect(parsePageParams(new URLSearchParams())).toEqual({})
  })
})
