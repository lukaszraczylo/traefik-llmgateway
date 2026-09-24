import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { useCopyFeedback } from './useCopyFeedback'

describe('useCopyFeedback', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('starts idle', () => {
    const { state } = useCopyFeedback()
    expect(state.value).toBe('idle')
  })

  it.each(['copied', 'selected', 'failed'] as const)('set(%s) records that outcome', (outcome) => {
    const { state, set } = useCopyFeedback()
    set(outcome)
    expect(state.value).toBe(outcome)
  })

  it('reverts to idle after 1500ms', () => {
    const { state, set } = useCopyFeedback()
    set('copied')
    vi.advanceTimersByTime(1499)
    expect(state.value).toBe('copied')
    vi.advanceTimersByTime(1)
    expect(state.value).toBe('idle')
  })

  it('a second set() before the first reset restarts the 1500ms window rather than stacking timers', () => {
    const { state, set } = useCopyFeedback()
    set('copied')
    vi.advanceTimersByTime(1000)
    set('selected')
    vi.advanceTimersByTime(1000)
    // 2000ms since the first set(), but only 1000ms since the second —
    // still showing the second outcome, not yet reset.
    expect(state.value).toBe('selected')
    vi.advanceTimersByTime(500)
    expect(state.value).toBe('idle')
  })
})
