import { describe, expect, it } from 'vitest'

import { loadState } from './load-state'

describe('loadState', () => {
  it('is "ready" when there is data, no error, not loading', () => {
    expect(loadState({ loading: false, hasData: true, error: '' })).toBe('ready')
  })

  it('is "ready" when there is data even if loading is true (a background refresh in flight)', () => {
    expect(loadState({ loading: true, hasData: true, error: '' })).toBe('ready')
  })

  // states-plan.md item 1: a background refresh failure must never blank
  // already-rendered data into an error state — that surfaces via a
  // toast instead (stores/toasts.ts), not by switching away from 'ready'.
  it('is "ready" when there is data even if a background refresh just failed (error set)', () => {
    expect(loadState({ loading: false, hasData: true, error: 'usage: fetch failed' })).toBe('ready')
  })

  it('is "error" when there is no data and an error is set', () => {
    expect(loadState({ loading: false, hasData: false, error: 'overview: fetch failed' })).toBe('error')
  })

  it('is "error" even while loading, when there is no data and an error is set (a retry already in flight)', () => {
    expect(loadState({ loading: true, hasData: false, error: 'overview: fetch failed' })).toBe('error')
  })

  it('is "skeleton" when there is no data, no error, and loading is true', () => {
    expect(loadState({ loading: true, hasData: false, error: '' })).toBe('skeleton')
  })

  it('is "empty" when there is no data, no error, and not loading', () => {
    expect(loadState({ loading: false, hasData: false, error: '' })).toBe('empty')
  })
})
