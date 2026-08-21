import { describe, expect, it } from 'vitest'

import { hashToTab, tabToHash } from './useTabHash'

const TAB_VALUES = ['providers', 'usage', 'charts', 'targets'] as const

describe('hashToTab', () => {
  it('maps a recognized hash (with leading #) to its tab value', () => {
    expect(hashToTab('#usage', TAB_VALUES, 'providers')).toBe('usage')
  })

  it('maps a recognized hash without a leading # too (defensive: a caller might pass window.location.hash.slice(1) already)', () => {
    expect(hashToTab('charts', TAB_VALUES, 'providers')).toBe('charts')
  })

  it('falls back to defaultTab for an empty hash (absent, load with no hash at all)', () => {
    expect(hashToTab('', TAB_VALUES, 'providers')).toBe('providers')
  })

  it('falls back to defaultTab for a bare "#" (empty fragment)', () => {
    expect(hashToTab('#', TAB_VALUES, 'providers')).toBe('providers')
  })

  it('falls back to defaultTab for an unrecognized hash (hand-edited URL)', () => {
    expect(hashToTab('#nonexistent-tab', TAB_VALUES, 'providers')).toBe('providers')
  })

  it('is case-sensitive: a differently-cased match still falls back to defaultTab', () => {
    expect(hashToTab('#Providers', TAB_VALUES, 'providers')).toBe('providers')
  })

  it('every valid tab round-trips to itself', () => {
    for (const tab of TAB_VALUES) {
      expect(hashToTab(`#${tab}`, TAB_VALUES, 'providers')).toBe(tab)
    }
  })
})

describe('tabToHash', () => {
  it('prefixes the tab value with "#"', () => {
    expect(tabToHash('targets')).toBe('#targets')
  })

  it('round-trips through hashToTab back to the same tab', () => {
    for (const tab of TAB_VALUES) {
      expect(hashToTab(tabToHash(tab), TAB_VALUES, 'providers')).toBe(tab)
    }
  })
})
