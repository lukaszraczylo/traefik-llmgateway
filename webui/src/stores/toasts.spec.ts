import { createPinia, setActivePinia } from 'pinia'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { MAX_VISIBLE_TOASTS, TOAST_TIMEOUT_MS, useToastsStore } from './toasts'

beforeEach(() => {
  setActivePinia(createPinia())
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('useToastsStore.push', () => {
  it('enqueues a toast with the default timeout for its kind', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'success', message: 'Copied' })
    expect(toasts.toasts).toHaveLength(1)
    expect(toasts.toasts[0]).toMatchObject({ kind: 'success', message: 'Copied', timeoutMs: TOAST_TIMEOUT_MS.success })
  })

  it('error toasts default to the longer 6s timeout', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'error', message: 'Export failed' })
    expect(toasts.toasts[0].timeoutMs).toBe(6000)
    expect(TOAST_TIMEOUT_MS.error).toBe(6000)
  })

  it('success/info toasts default to the shorter 3s timeout', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'success', message: 'a' })
    toasts.push({ kind: 'info', message: 'b' })
    expect(toasts.toasts[0].timeoutMs).toBe(3000)
    expect(toasts.toasts[1].timeoutMs).toBe(3000)
  })

  it('accepts a per-push timeoutMs override', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'success', message: 'a', timeoutMs: 100 })
    expect(toasts.toasts[0].timeoutMs).toBe(100)
  })

  it('assigns each toast a distinct, increasing id', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'success', message: 'a' })
    toasts.push({ kind: 'success', message: 'b' })
    expect(toasts.toasts[1].id).toBeGreaterThan(toasts.toasts[0].id)
  })

  it('auto-dismisses after its own timeoutMs', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'success', message: 'a', timeoutMs: 1000 })
    expect(toasts.toasts).toHaveLength(1)
    vi.advanceTimersByTime(999)
    expect(toasts.toasts).toHaveLength(1)
    vi.advanceTimersByTime(1)
    expect(toasts.toasts).toHaveLength(0)
  })

  it('dedupes an identical kind+message pushed consecutively', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'error', message: 'refresh failed' })
    toasts.push({ kind: 'error', message: 'refresh failed' })
    expect(toasts.toasts).toHaveLength(1)
  })

  it('does NOT dedupe the same message once a different toast came in between', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'error', message: 'refresh failed' })
    toasts.push({ kind: 'info', message: 'other' })
    toasts.push({ kind: 'error', message: 'refresh failed' })
    expect(toasts.toasts).toHaveLength(3)
  })

  it('does NOT dedupe the same message text across different kinds', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'error', message: 'x' })
    toasts.push({ kind: 'success', message: 'x' })
    expect(toasts.toasts).toHaveLength(2)
  })

  it('caps visible toasts at MAX_VISIBLE_TOASTS, dropping the oldest first (FIFO)', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'info', message: '1' })
    toasts.push({ kind: 'info', message: '2' })
    toasts.push({ kind: 'info', message: '3' })
    toasts.push({ kind: 'info', message: '4' })
    expect(MAX_VISIBLE_TOASTS).toBe(3)
    expect(toasts.toasts).toHaveLength(3)
    expect(toasts.toasts.map((t) => t.message)).toEqual(['2', '3', '4'])
  })

  it('cancels a dropped toast\'s own timer so it cannot fire after eviction', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'info', message: '1', timeoutMs: 500 })
    toasts.push({ kind: 'info', message: '2', timeoutMs: 500 })
    toasts.push({ kind: 'info', message: '3', timeoutMs: 500 })
    toasts.push({ kind: 'info', message: '4', timeoutMs: 500 }) // drops '1'
    expect(toasts.toasts.map((t) => t.message)).toEqual(['2', '3', '4'])
    vi.advanceTimersByTime(500)
    expect(toasts.toasts).toHaveLength(0)
  })
})

describe('useToastsStore.dismiss', () => {
  it('removes the named toast immediately', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'success', message: 'a' })
    const id = toasts.toasts[0].id
    toasts.dismiss(id)
    expect(toasts.toasts).toHaveLength(0)
  })

  it('cancels the pending auto-dismiss timer, so it does not double-fire on an id that no longer exists', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'success', message: 'a', timeoutMs: 1000 })
    const id = toasts.toasts[0].id
    toasts.dismiss(id)
    // Nothing should throw when the (canceled) timer would otherwise have fired.
    expect(() => vi.advanceTimersByTime(2000)).not.toThrow()
    expect(toasts.toasts).toHaveLength(0)
  })

  it('is a no-op for an unknown id', () => {
    const toasts = useToastsStore()
    toasts.push({ kind: 'success', message: 'a' })
    toasts.dismiss(999)
    expect(toasts.toasts).toHaveLength(1)
  })
})
