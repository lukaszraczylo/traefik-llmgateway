import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { createVisibilityPoller, type VisibilityDocumentLike } from './polling'

/**
 * createFakeDoc builds a minimal VisibilityDocumentLike test double: a
 * mutable `hidden` flag the test flips directly, a single
 * visibilitychange listener slot (real Document supports many, but this
 * module only ever registers one), and `fire()`/`hasListener()` helpers
 * so a test can drive and inspect it without any real DOM.
 */
function createFakeDoc() {
  let hiddenValue = false
  let listener: (() => void) | undefined
  const doc: VisibilityDocumentLike = {
    get hidden() {
      return hiddenValue
    },
    addEventListener(_type, l) {
      listener = l
    },
    removeEventListener(_type, l) {
      if (listener === l) listener = undefined
    },
  }
  return {
    doc,
    setHidden: (v: boolean) => {
      hiddenValue = v
    },
    fire: () => listener?.(),
    hasListener: () => listener !== undefined,
  }
}

describe('createVisibilityPoller', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('ticks immediately on start, before the first interval elapses', () => {
    const tick = vi.fn()
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: createFakeDoc().doc })

    poller.start()

    expect(tick).toHaveBeenCalledTimes(1)
  })

  it('ticks again every intervalMs while running', () => {
    const tick = vi.fn()
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: createFakeDoc().doc })

    poller.start()
    vi.advanceTimersByTime(3000)

    expect(tick).toHaveBeenCalledTimes(4) // immediate + 3 interval ticks
  })

  it('start() is idempotent — calling it twice does not double-schedule', () => {
    const tick = vi.fn()
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: createFakeDoc().doc })

    poller.start()
    poller.start()
    vi.advanceTimersByTime(1000)

    expect(tick).toHaveBeenCalledTimes(2) // one immediate + one interval tick, not two of either
  })

  it('stop() clears the interval — no further ticks', () => {
    const tick = vi.fn()
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: createFakeDoc().doc })

    poller.start()
    poller.stop()
    vi.advanceTimersByTime(5000)

    expect(tick).toHaveBeenCalledTimes(1) // only the immediate tick from start()
  })

  it('stop() is idempotent — safe to call before start() or twice in a row', () => {
    const tick = vi.fn()
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: createFakeDoc().doc })

    expect(() => {
      poller.stop()
      poller.start()
      poller.stop()
      poller.stop()
    }).not.toThrow()
    expect(tick).toHaveBeenCalledTimes(1)
  })

  it('pauses polling while the document is hidden', () => {
    const tick = vi.fn()
    const fake = createFakeDoc()
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: fake.doc })

    poller.start()
    tick.mockClear()
    fake.setHidden(true)
    fake.fire()
    vi.advanceTimersByTime(5000)

    expect(tick).not.toHaveBeenCalled()
  })

  it('catches up immediately when the document becomes visible again, then resumes the interval', () => {
    const tick = vi.fn()
    const fake = createFakeDoc()
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: fake.doc })

    poller.start()
    fake.setHidden(true)
    fake.fire()
    tick.mockClear()

    fake.setHidden(false)
    fake.fire()
    expect(tick).toHaveBeenCalledTimes(1) // the catch-up tick, not waiting out intervalMs

    vi.advanceTimersByTime(1000)
    expect(tick).toHaveBeenCalledTimes(2) // interval resumed after the catch-up
  })

  it('stop() removes the visibilitychange listener', () => {
    const tick = vi.fn()
    const fake = createFakeDoc()
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: fake.doc })

    poller.start()
    expect(fake.hasListener()).toBe(true)
    poller.stop()

    expect(fake.hasListener()).toBe(false)
  })

  it('falls back to a plain interval with no visibility awareness when no document is available (vitest\'s node environment, no explicit doc passed)', () => {
    const tick = vi.fn()
    const poller = createVisibilityPoller({ intervalMs: 1000, tick })

    poller.start()
    vi.advanceTimersByTime(2000)

    expect(tick).toHaveBeenCalledTimes(3) // immediate + 2 interval ticks — never throws for lack of `document`
    poller.stop()
  })
  // P13: a poller started while the document is already hidden (e.g. this
  // admin panel loaded in a background tab) must not tick until the reader
  // actually looks at it — the pre-fix version ticked immediately and
  // scheduled a background interval regardless of visibility.
  it('does not tick immediately when the document is hidden at start', () => {
    const tick = vi.fn()
    const fake = createFakeDoc()
    fake.setHidden(true)
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: fake.doc })

    poller.start()

    expect(tick).not.toHaveBeenCalled()
    poller.stop()
  })

  it('does not schedule a background interval while started hidden', () => {
    const tick = vi.fn()
    const fake = createFakeDoc()
    fake.setHidden(true)
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: fake.doc })

    poller.start()
    vi.advanceTimersByTime(10_000)

    expect(tick).not.toHaveBeenCalled()
    poller.stop()
  })

  it('ticks for the first time once the document becomes visible, having started hidden', () => {
    const tick = vi.fn()
    const fake = createFakeDoc()
    fake.setHidden(true)
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: fake.doc })

    poller.start()
    fake.setHidden(false)
    fake.fire()

    expect(tick).toHaveBeenCalledTimes(1)

    vi.advanceTimersByTime(1000)
    expect(tick).toHaveBeenCalledTimes(2) // interval resumed after the deferred first tick
    poller.stop()
  })

  it('start() while hidden is still idempotent — a second call does not schedule a second listener/timer', () => {
    const tick = vi.fn()
    const fake = createFakeDoc()
    fake.setHidden(true)
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: fake.doc })

    poller.start()
    poller.start()
    fake.setHidden(false)
    fake.fire()

    expect(tick).toHaveBeenCalledTimes(1) // one catch-up tick, not two
    poller.stop()
  })

  it('ticks immediately when the document is already visible at start (unchanged, non-hidden case)', () => {
    const tick = vi.fn()
    const fake = createFakeDoc()
    fake.setHidden(false)
    const poller = createVisibilityPoller({ intervalMs: 1000, tick, doc: fake.doc })

    poller.start()

    expect(tick).toHaveBeenCalledTimes(1)
    poller.stop()
  })
})
