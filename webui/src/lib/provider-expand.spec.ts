import { describe, expect, it } from 'vitest'

import {
  type ExpandState,
  clearExpandOverrides,
  computeExpandedProviders,
  toggleProviderExpand,
} from './provider-expand'

/** newState is a fresh ExpandState — every test starts from an empty, un-searched panel. */
function newState(): ExpandState {
  return { manuallyExpanded: new Set(), manuallyCollapsed: new Set() }
}

describe('computeExpandedProviders', () => {
  it('returns manuallyExpanded verbatim when there is no active query', () => {
    const state = newState()
    state.manuallyExpanded.add('openai')
    expect(computeExpandedProviders(state, false, ['openai', 'anthropic'])).toEqual(new Set(['openai']))
  })

  it('auto-expands every matching name while a query is active', () => {
    const state = newState()
    expect(computeExpandedProviders(state, true, ['openai', 'anthropic'])).toEqual(new Set(['openai', 'anthropic']))
  })

  it('unions manuallyExpanded with matches while a query is active', () => {
    const state = newState()
    state.manuallyExpanded.add('gemini') // manually opened before the search started, does not match it
    expect(computeExpandedProviders(state, true, ['openai'])).toEqual(new Set(['gemini', 'openai']))
  })

  it('suppresses an auto-match the user explicitly collapsed', () => {
    const state = newState()
    state.manuallyCollapsed.add('openai')
    expect(computeExpandedProviders(state, true, ['openai', 'anthropic'])).toEqual(new Set(['anthropic']))
  })
})

describe('toggleProviderExpand', () => {
  it('opening (currentlyExpanded=false) adds to manuallyExpanded and clears any collapse override', () => {
    const state = newState()
    state.manuallyCollapsed.add('openai')
    toggleProviderExpand(state, 'openai', false)
    expect(state.manuallyExpanded.has('openai')).toBe(true)
    expect(state.manuallyCollapsed.has('openai')).toBe(false)
  })

  it('closing (currentlyExpanded=true) removes from manuallyExpanded and adds a collapse override', () => {
    const state = newState()
    state.manuallyExpanded.add('openai')
    toggleProviderExpand(state, 'openai', true)
    expect(state.manuallyExpanded.has('openai')).toBe(false)
    expect(state.manuallyCollapsed.has('openai')).toBe(true)
  })
})

describe('clearExpandOverrides', () => {
  it('drops manuallyCollapsed but leaves manuallyExpanded untouched', () => {
    const state = newState()
    state.manuallyExpanded.add('openai')
    state.manuallyCollapsed.add('anthropic')
    clearExpandOverrides(state)
    expect(state.manuallyCollapsed.size).toBe(0)
    expect(state.manuallyExpanded.has('openai')).toBe(true)
  })
})

describe('the review-flagged sequence: an auto-matched (never manually opened) provider, clicked closed', () => {
  it('never enters manuallyExpanded, and stays collapsed after the query clears', () => {
    const state = newState()
    const matches = ['openai']

    // 1. Search starts matching "openai" — it shows via auto-expand only,
    //    manuallyExpanded is still empty.
    let expanded = computeExpandedProviders(state, true, matches)
    expect(expanded.has('openai')).toBe(true)
    expect(state.manuallyExpanded.has('openai')).toBe(false)

    // 2. User clicks it closed while it's only auto-expanded. The RAW bug
    //    this module exists to fix: reading manuallyExpanded.has('openai')
    //    directly (false) would have ADDED it to manuallyExpanded instead
    //    of suppressing the auto-expand. toggleProviderExpand takes the
    //    EFFECTIVE current state (expanded.has('openai') === true) instead.
    toggleProviderExpand(state, 'openai', expanded.has('openai'))
    expect(state.manuallyExpanded.has('openai')).toBe(false)
    expect(state.manuallyCollapsed.has('openai')).toBe(true)

    // 3. Still searching: the override suppresses the auto-expand.
    expanded = computeExpandedProviders(state, true, matches)
    expect(expanded.has('openai')).toBe(false)

    // 4. Query clears: a stale override must never carry into a later,
    //    unrelated search.
    clearExpandOverrides(state)
    expanded = computeExpandedProviders(state, false, [])
    expect(expanded.has('openai')).toBe(false)
  })
})

describe('a provider manually opened before search, still matches, clicked closed during search', () => {
  it("the user's in-search click wins, and it stays closed after the query clears too", () => {
    const state = newState()
    state.manuallyExpanded.add('openai') // opened before any search existed
    const matches = ['openai']

    let expanded = computeExpandedProviders(state, true, matches)
    expect(expanded.has('openai')).toBe(true)

    toggleProviderExpand(state, 'openai', expanded.has('openai'))
    expect(state.manuallyExpanded.has('openai')).toBe(false)
    expect(state.manuallyCollapsed.has('openai')).toBe(true)

    clearExpandOverrides(state)
    expanded = computeExpandedProviders(state, false, [])
    expect(expanded.has('openai')).toBe(false)
  })
})
