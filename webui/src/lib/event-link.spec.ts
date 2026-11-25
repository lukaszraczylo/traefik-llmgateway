import { describe, expect, it } from 'vitest'

import { eventLink } from './event-link'
import type { AdminEventView } from '@/types/api'

function event(overrides: Partial<AdminEventView> = {}): AdminEventView {
  return { time: '2026-11-25T12:00:00Z', replica: 'pod-a', route: 'chat/completions', kind: 'rate_limit', message: 'limit exceeded', ...overrides }
}

describe('eventLink', () => {
  it('rate_limit links to user panel', () => {
    expect(eventLink(event({ kind: 'rate_limit', user: 'alice@example.com' }))).toEqual({ page: 'users', sel: 'user:alice%40example.com' })
  })

  it('upstream links to provider panel', () => {
    expect(eventLink(event({ kind: 'upstream', provider: 'alibaba' }))).toEqual({ page: 'models', sel: 'provider:alibaba' })
  })

  it('unknown kind → null', () => {
    expect(eventLink(event({ kind: 'from_the_future', user: 'alice' }))).toBeNull()
  })

  it('prefers the user over the provider', () => {
    expect(eventLink(event({ kind: 'upstream', user: 'alice', provider: 'alibaba' }))).toEqual({ page: 'users', sel: 'user:alice' })
  })

  it('falls back to the group panel for a group-scoped event', () => {
    expect(eventLink(event({ kind: 'budget', group: 'friends' }))).toEqual({ page: 'users', sel: 'group:friends' })
  })

  it('links a known kind without an entity to the events page', () => {
    expect(eventLink(event({ kind: 'store_down' }))).toEqual({ page: 'events' })
  })

  it('links router_escalation to its provider panel', () => {
    expect(eventLink(event({ kind: 'router_escalation', provider: 'gx10' }))).toEqual({ page: 'models', sel: 'provider:gx10' })
  })
})
