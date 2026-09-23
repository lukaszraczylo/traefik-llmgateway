// createVisibilityPoller is the ONE polling primitive stores/dashboard.ts,
// stores/history.ts, and stores/events.ts each wire their own refresh
// action into (vue.md: "if you've written it twice, you owe an
// abstraction") — three near-identical setInterval-plus-immediate-tick
// blocks collapsed into one tested module. Beyond the plain interval every
// one of them already had, this adds visibility awareness: a background
// browser tab has no reason to keep polling a Redis-backed admin panel
// every 5s/30s for no reader to see, and it catches back up the instant
// the tab is looked at again instead of waiting out whatever fraction of
// the interval happened to be left.

/**
 * The subset of `Document` this module reads — a structural type, not the
 * real DOM `Document`, so a test's fake stand-in needs no jsdom (vitest's
 * 'node' environment, vite.config.ts, has none).
 */
export interface VisibilityDocumentLike {
  readonly hidden: boolean
  addEventListener(type: 'visibilitychange', listener: () => void): void
  removeEventListener(type: 'visibilitychange', listener: () => void): void
}

export interface VisibilityPollerOptions {
  /** How often `tick` fires while the page is visible. */
  intervalMs: number
  /**
   * The store's own refresh step. Never awaited here — a caller that
   * needs its own in-flight guard (history.ts keeps its "skip a tick
   * while the previous fetch is still in flight" rule) builds that into
   * `tick` itself, same as before this module existed.
   */
  tick: () => void
  /**
   * The document to watch for visibility changes. Defaults to the real
   * `document` global when one exists. In a non-browser context (vitest's
   * 'node' test environment, or any future non-DOM caller) `document` is
   * undefined, which is deliberately the "no document -> plain interval"
   * fallback: every access to `doc` below is guarded, so importing or
   * calling this module never throws there — it just behaves like a bare
   * setInterval with an immediate first tick.
   */
  doc?: VisibilityDocumentLike
}

export interface VisibilityPoller {
  /** Idempotent: calling start() while already running is a no-op, mirroring the stores' own pre-existing startPolling/startAutoRefresh guards. */
  start(): void
  /** Idempotent: safe to call whether or not start() ever ran. */
  stop(): void
}

/** resolveDoc picks the explicit `doc` option when given, else the real global if one exists, else undefined — computed once at creation, not read live on every tick. */
function resolveDoc(doc: VisibilityDocumentLike | undefined): VisibilityDocumentLike | undefined {
  if (doc) return doc
  return typeof document === 'undefined' ? undefined : (document as unknown as VisibilityDocumentLike)
}

export function createVisibilityPoller({ intervalMs, tick, doc }: VisibilityPollerOptions): VisibilityPoller {
  const target = resolveDoc(doc)
  let timer: ReturnType<typeof setInterval> | undefined
  // P13 review fix: `timer !== undefined` alone used to double as start()'s
  // idempotency guard, but a poller that STARTS while the page is hidden
  // (below) never schedules a timer at all until the page is first looked
  // at — with no separate flag, a second start() call while still hidden
  // would find `timer` still undefined and incorrectly tick/schedule a
  // second time. `started` is the idempotency guard now; `timer` is purely
  // "is a tick currently scheduled".
  let started = false

  function clearTimer(): void {
    if (timer === undefined) return
    clearInterval(timer)
    timer = undefined
  }

  function scheduleTimer(): void {
    clearTimer()
    timer = setInterval(tick, intervalMs)
  }

  // Pauses on hidden (clear the interval — nobody can see the result of a
  // background poll) and catches up on visible again: an immediate tick,
  // not just a freshly (re)started interval that would otherwise make the
  // reader wait out the full intervalMs before seeing anything current.
  function onVisibilityChange(): void {
    if (!target) return
    if (target.hidden) {
      clearTimer()
    } else {
      tick()
      scheduleTimer()
    }
  }

  // P13: a poller STARTED while the document is already hidden (e.g. this
  // admin panel loaded in a background tab) must not tick at all until the
  // reader actually looks at it — ticking immediately regardless of
  // visibility, the pre-fix behavior, polled a Redis-backed admin endpoint
  // for a tab nobody could see, for as long as it stayed backgrounded.
  // Ticking is deferred to onVisibilityChange's own "became visible" branch
  // above, which already ticks immediately and reschedules the moment the
  // tab is first shown — so a start-while-hidden poller's first tick is
  // simply later, never skipped outright.
  function start(): void {
    if (started) return
    started = true
    if (!target?.hidden) {
      tick()
      scheduleTimer()
    }
    target?.addEventListener('visibilitychange', onVisibilityChange)
  }

  function stop(): void {
    started = false
    clearTimer()
    target?.removeEventListener('visibilitychange', onVisibilityChange)
  }

  return { start, stop }
}
