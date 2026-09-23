import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { useNow } from './useNow'
import { monthProgress, projectMonthEnd } from '@/lib/forecast'

describe('useNow', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  it('ticks the shared now ref immediately on start', () => {
    vi.setSystemTime(new Date('2026-04-16T00:00:00.000Z'))
    const { now, start, stop } = useNow()

    start()

    expect(now.value.toISOString()).toBe('2026-04-16T00:00:00.000Z')
    stop()
  })

  it('advances now on each subsequent tick, at the configured interval', () => {
    vi.setSystemTime(new Date('2026-04-16T00:00:00.000Z'))
    const { now, start, stop } = useNow(60_000)

    start()
    // advanceTimersByTime both advances the fake clock AND fires whatever
    // interval callback falls due within that span — a separate
    // vi.setSystemTime call here would ADD its own jump on top of this
    // one (they compound; sinon's fake Date offset is not superseded by
    // a later setSystemTime call), double-advancing `now`.
    vi.advanceTimersByTime(60_000)

    expect(now.value.toISOString()).toBe('2026-04-16T00:01:00.000Z')
    stop()
  })

  it('stop() clears the timer — now no longer advances', () => {
    vi.setSystemTime(new Date('2026-04-16T00:00:00.000Z'))
    const { now, start, stop } = useNow(60_000)

    start()
    stop()
    vi.setSystemTime(new Date('2026-04-16T00:05:00.000Z'))
    vi.advanceTimersByTime(300_000)

    expect(now.value.toISOString()).toBe('2026-04-16T00:00:00.000Z')
  })

  it('start() is idempotent — a second call does not double-schedule', () => {
    vi.setSystemTime(new Date('2026-04-16T00:00:00.000Z'))
    const { now, start, stop } = useNow(1000)

    start()
    start()
    // See the previous test's own comment: advanceTimersByTime alone both
    // advances the clock and fires the one due tick — an extra
    // setSystemTime call here would compound into a double-advance.
    vi.advanceTimersByTime(1000)

    // A double-scheduled poller would still land on the same value here
    // (both intervals would tick at the same wall-clock instant), so this
    // alone doesn't fully pin idempotency — the real guard is
    // createVisibilityPoller's own start()-is-idempotent behavior
    // (lib/polling.spec.ts), reused here unmodified.
    expect(now.value.toISOString()).toBe('2026-04-16T00:00:01.000Z')
    stop()
  })

  // P1: the concrete symptom this composable fixes — a long-open tab's cost
  // projection (lib/forecast.ts) must change once the shared clock ticks to
  // a later point in the month, not stay frozen at whatever `now` was true
  // when something first read it.
  it('feeding the shared clock into monthProgress/projectMonthEnd yields a fresh projection once the clock advances', () => {
    // Just past the start of April (UTC): elapsedFraction is below
    // MIN_PROJECTION_FRACTION, so projectMonthEnd reads "not enough data
    // yet" (null) — exactly the P1 bug scenario ("every projection shows
    // 'not enough data yet' until the tab remounts").
    vi.setSystemTime(new Date(Date.UTC(2026, 3, 1, 1, 0, 0)))
    const { now, start, stop } = useNow(60_000)
    start()
    const mtdMicros = 1_000_000
    const firstProjection = projectMonthEnd(mtdMicros, monthProgress(now.value))
    expect(firstProjection).toBeNull()

    // Mid-month (exactly half of April's 30 days elapsed, UTC). A
    // stop()/setSystemTime()/start() restart — rather than a further
    // setSystemTime+advanceTimersByTime pair, which would compound with
    // the jump above (see the sibling "advances now on each subsequent
    // tick" test's own comment) — forces exactly one fresh, synchronous
    // tick at precisely this instant: start()'s own immediate tick, not
    // an interval fire, is what this test actually needs to prove (a
    // later READ of `now.value` reflects the advanced clock).
    stop()
    vi.setSystemTime(new Date(Date.UTC(2026, 3, 16, 0, 0, 0)))
    start()
    const secondProjection = projectMonthEnd(mtdMicros, monthProgress(now.value))

    expect(secondProjection).toBe(2_000_000) // mtdMicros / 0.5
    expect(secondProjection).not.toBe(firstProjection)
    stop()
  })
})
