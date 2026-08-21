import { describe, expect, it } from 'vitest'

import {
  type ExpandState,
  clearExpandOverrides,
  computeExpandedItems,
  toggleItemExpand,
} from './search-expand'

/** newState is a fresh ExpandState — every test starts from an empty, un-searched panel. */
function newState(): ExpandState {
  return { manuallyExpanded: new Set(), manuallyCollapsed: new Set() }
}

describe('computeExpandedItems', () => {
  it('returns manuallyExpanded verbatim when there is no active query', () => {
    const state = newState()
    state.manuallyExpanded.add('openai')
    expect(computeExpandedItems(state, false, ['openai', 'anthropic'])).toEqual(new Set(['openai']))
  })

  it('auto-expands every matching id while a query is active', () => {
    const state = newState()
    expect(computeExpandedItems(state, true, ['openai', 'anthropic'])).toEqual(new Set(['openai', 'anthropic']))
  })

  it('unions manuallyExpanded with matches while a query is active', () => {
    const state = newState()
    state.manuallyExpanded.add('gemini') // manually opened before the search started, does not match it
    expect(computeExpandedItems(state, true, ['openai'])).toEqual(new Set(['gemini', 'openai']))
  })

  it('suppresses an auto-match the user explicitly collapsed', () => {
    const state = newState()
    state.manuallyCollapsed.add('openai')
    expect(computeExpandedItems(state, true, ['openai', 'anthropic'])).toEqual(new Set(['anthropic']))
  })
})

describe('toggleItemExpand', () => {
  it('opening (currentlyExpanded=false) adds to manuallyExpanded and clears any collapse override', () => {
    const state = newState()
    state.manuallyCollapsed.add('openai')
    toggleItemExpand(state, 'openai', false)
    expect(state.manuallyExpanded.has('openai')).toBe(true)
    expect(state.manuallyCollapsed.has('openai')).toBe(false)
  })

  it('closing (currentlyExpanded=true) removes from manuallyExpanded and adds a collapse override', () => {
    const state = newState()
    state.manuallyExpanded.add('openai')
    toggleItemExpand(state, 'openai', true)
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

describe('the review-flagged sequence: an auto-matched (never manually opened) item, clicked closed', () => {
  it('never enters manuallyExpanded, and stays collapsed after the query clears', () => {
    const state = newState()
    const matches = ['openai']

    // 1. Search starts matching "openai" — it shows via auto-expand only,
    //    manuallyExpanded is still empty.
    let expanded = computeExpandedItems(state, true, matches)
    expect(expanded.has('openai')).toBe(true)
    expect(state.manuallyExpanded.has('openai')).toBe(false)

    // 2. User clicks it closed while it's only auto-expanded. The RAW bug
    //    this module exists to fix: reading manuallyExpanded.has('openai')
    //    directly (false) would have ADDED it to manuallyExpanded instead
    //    of suppressing the auto-expand. toggleItemExpand takes the
    //    EFFECTIVE current state (expanded.has('openai') === true) instead.
    toggleItemExpand(state, 'openai', expanded.has('openai'))
    expect(state.manuallyExpanded.has('openai')).toBe(false)
    expect(state.manuallyCollapsed.has('openai')).toBe(true)

    // 3. Still searching: the override suppresses the auto-expand.
    expanded = computeExpandedItems(state, true, matches)
    expect(expanded.has('openai')).toBe(false)

    // 4. Query clears: a stale override must never carry into a later,
    //    unrelated search.
    clearExpandOverrides(state)
    expanded = computeExpandedItems(state, false, [])
    expect(expanded.has('openai')).toBe(false)
  })
})

describe('an item manually opened before search, still matches, clicked closed during search', () => {
  it("the user's in-search click wins, and it stays closed after the query clears too", () => {
    const state = newState()
    state.manuallyExpanded.add('openai') // opened before any search existed
    const matches = ['openai']

    let expanded = computeExpandedItems(state, true, matches)
    expect(expanded.has('openai')).toBe(true)

    toggleItemExpand(state, 'openai', expanded.has('openai'))
    expect(state.manuallyExpanded.has('openai')).toBe(false)
    expect(state.manuallyCollapsed.has('openai')).toBe(true)

    clearExpandOverrides(state)
    expanded = computeExpandedItems(state, false, [])
    expect(expanded.has('openai')).toBe(false)
  })
})

// This module is domain-agnostic on what an "id" represents — it started as
// ProvidersView.vue's provider names, but UsageView.vue now feeds it group
// ids too (see that view's own doc comment: matchingIds there is every
// group that matches ONLY via a member, never via its own name). These two
// cases exist to pin that the module behaves identically for that second
// domain, not because anything here differs by id shape.
describe('generic ids (Usage group ids, not just Providers provider names)', () => {
  it('auto-expands and can be collapsed/overridden identically for a group id', () => {
    const state = newState()
    const matches = ['team-a']

    let expanded = computeExpandedItems(state, true, matches)
    expect(expanded.has('team-a')).toBe(true)

    toggleItemExpand(state, 'team-a', expanded.has('team-a'))
    expect(state.manuallyExpanded.has('team-a')).toBe(false)
    expect(state.manuallyCollapsed.has('team-a')).toBe(true)

    expanded = computeExpandedItems(state, true, matches)
    expect(expanded.has('team-a')).toBe(false)

    clearExpandOverrides(state)
    expanded = computeExpandedItems(state, false, [])
    expect(expanded.has('team-a')).toBe(false)
  })

  it('a group id manually opened stays open when the query clears', () => {
    const state = newState()
    state.manuallyExpanded.add('team-a')
    expect(computeExpandedItems(state, false, [])).toEqual(new Set(['team-a']))
  })
})
