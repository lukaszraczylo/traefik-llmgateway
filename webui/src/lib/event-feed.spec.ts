import { describe, expect, it } from 'vitest'

import { collapseEvents, eventSubject, formatEventTime } from './event-feed'
import type { AdminEventView } from '@/types/api'

function event(overrides: Partial<AdminEventView> = {}): AdminEventView {
  return { time: '2026-11-25T12:00:00Z', replica: 'pod-a', route: 'chat/completions', kind: 'upstream', message: 'status 503', provider: 'amd', ...overrides }
}
const local = (y: number, mo: number, d: number, h: number, mi: number) => new Date(y, mo - 1, d, h, mi).toISOString()

describe('formatEventTime', () => {
  const now = new Date(2026, 10, 25, 15, 0)
  it('shows only the clock for an event from today', () => {
    expect(formatEventTime(local(2026, 11, 25, 9, 5), now)).toBe('09:05')
  })
  it('adds the date for an earlier day', () => {
    expect(formatEventTime(local(2026, 11, 24, 23, 59), now)).toBe('Nov 24 23:59')
  })
  it('adds the date across a year boundary', () => {
    expect(formatEventTime(local(2025, 12, 31, 8, 0), now)).toBe('Dec 31 08:00')
  })
  it('is empty for an unparseable time', () => {
    expect(formatEventTime('garbage', now)).toBe('')
  })
})

describe('eventSubject', () => {
  it('prefers user, then group, provider, model, route, message', () => {
    expect(eventSubject(event({ user: 'u', group: 'g' }))).toBe('u')
    expect(eventSubject(event({ provider: undefined, model: 'm' }))).toBe('m')
    expect(eventSubject(event({ provider: undefined, route: '' }))).toBe('status 503')
  })
})

describe('collapseEvents', () => {
  it('merges consecutive identical events into one row with a count', () => {
    const rows = collapseEvents([event({ time: 'a' }), event({ time: 'b' }), event({ time: 'c' })])
    expect(rows).toHaveLength(1)
    expect(rows[0].count).toBe(3)
    expect(rows[0].event.time).toBe('a')
  })
  it('does not merge across a different event', () => {
    expect(collapseEvents([event(), event({ message: 'other' }), event()]).map((r) => r.count)).toEqual([1, 1, 1])
  })
  it('treats a different kind or subject as different', () => {
    expect(collapseEvents([event(), event({ kind: 'timeout' }), event({ kind: 'timeout', provider: 'x' })])).toHaveLength(3)
  })
  it('keeps distinct keys for rows', () => {
    const keys = collapseEvents([event({ message: 'a' }), event({ message: 'b' })]).map((r) => r.key)
    expect(new Set(keys).size).toBe(2)
  })
  it('is empty for no events', () => {
    expect(collapseEvents([])).toEqual([])
  })
})
