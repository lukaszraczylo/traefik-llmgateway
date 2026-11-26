import { describe, expect, it } from 'vitest'

import { eventFilterFromParams, eventKindVariant, EVENT_KIND_LABEL, EVENT_KINDS, filterEvents, itemsFromFilter, kindLabel, paramsFromItems } from './events-filter'
import type { AdminEventView } from '@/types/api'

function event(overrides: Partial<AdminEventView> = {}): AdminEventView {
  return {
    time: '2026-08-20T12:00:00Z',
    replica: 'pod-a',
    route: 'chat/completions',
    kind: 'rate_limit',
    message: 'requests/min limit exceeded',
    ...overrides,
  }
}

describe('EVENT_KIND_LABEL', () => {
  it('has a label for every AdminEventKind the server can emit', () => {
    const kinds = ['rate_limit', 'budget', 'store_down', 'upstream', 'timeout', 'unpriced', 'capacity'] as const
    for (const kind of kinds) {
      expect(EVENT_KIND_LABEL[kind]).toBeTruthy()
    }
  })
})

// P11 review fix: EVENT_KINDS is derived from EVENT_KIND_LABEL's own keys
// (the single source of truth), not a third independent literal array —
// this pins that it actually contains every known kind, in the same order
// EVENT_KIND_LABEL's own keys were declared.
describe('EVENT_KINDS', () => {
  it('contains every known AdminEventKind, derived from EVENT_KIND_LABEL', () => {
    expect(EVENT_KINDS).toEqual(Object.keys(EVENT_KIND_LABEL))
    expect(EVENT_KINDS).toEqual(['rate_limit', 'budget', 'store_down', 'upstream', 'timeout', 'unpriced', 'capacity', 'paced', 'router_escalation', 'router_timeout_fallback'])
  })

  it('every entry has a truthy label (no silently-empty display text)', () => {
    for (const kind of EVENT_KINDS) {
      expect(EVENT_KIND_LABEL[kind]).toBeTruthy()
    }
  })
})

describe('kindLabel', () => {
  it('renders the known display label for every AdminEventKind', () => {
    for (const kind of EVENT_KINDS) {
      expect(kindLabel(kind)).toBe(EVENT_KIND_LABEL[kind])
    }
  })

  it('falls back to the raw kind string for a kind this build does not recognize', () => {
    expect(kindLabel('some_future_kind')).toBe('some_future_kind')
  })
})

describe('eventKindVariant', () => {
  it.each([
    ['rate_limit', 'destructive'],
    ['budget', 'destructive'],
    ['capacity', 'destructive'],
    ['store_down', 'secondary'],
    ['upstream', 'secondary'],
    ['timeout', 'secondary'],
    ['unpriced', 'outline'],
  ] as const)('maps %s to %s', (kind, variant) => {
    expect(eventKindVariant(kind)).toBe(variant)
  })

  it('falls back to outline (the neutral variant) for a kind this build does not recognize', () => {
    expect(eventKindVariant('some_future_kind')).toBe('outline')
  })
})

describe('filterEvents', () => {
  const events: AdminEventView[] = [
    event({ kind: 'rate_limit', user: 'alice' }),
    event({ kind: 'budget', group: 'ops' }),
    event({ kind: 'upstream', user: 'bob-smith' }),
    event({ kind: 'timeout' }), // neither user nor group — a total/all-scope or capacity-style event
  ]

  it('returns every event when no filter is set', () => {
    expect(filterEvents(events, { kind: '', user: '' })).toHaveLength(4)
  })

  it('filters by exact kind', () => {
    const result = filterEvents(events, { kind: 'budget', user: '' })
    expect(result).toHaveLength(1)
    expect(result[0]!.group).toBe('ops')
  })

  it('matches user against the event\'s own user field, case-insensitively, substring', () => {
    const result = filterEvents(events, { kind: '', user: 'ALICE' })
    expect(result).toHaveLength(1)
    expect(result[0]!.user).toBe('alice')
  })

  it('matches user against the event\'s own group field too (a group-scoped event has no user)', () => {
    const result = filterEvents(events, { kind: '', user: 'ops' })
    expect(result).toHaveLength(1)
    expect(result[0]!.group).toBe('ops')
  })

  it('matches a partial substring, not just a full match', () => {
    const result = filterEvents(events, { kind: '', user: 'smith' })
    expect(result).toHaveLength(1)
    expect(result[0]!.user).toBe('bob-smith')
  })

  it('trims and lowercases the raw query defensively, even if a caller skips useSearchQuery', () => {
    const result = filterEvents(events, { kind: '', user: '  Alice  ' })
    expect(result).toHaveLength(1)
  })

  it('combines kind and user filters (both must match)', () => {
    const result = filterEvents(events, { kind: 'upstream', user: 'bob' })
    expect(result).toHaveLength(1)
    expect(result[0]!.user).toBe('bob-smith')
  })

  it('excludes an event with neither user nor group once a user filter is active', () => {
    const result = filterEvents(events, { kind: '', user: 'anything' })
    expect(result.every((e) => e.user || e.group)).toBe(true)
  })

  it('returns an empty array when nothing matches', () => {
    expect(filterEvents(events, { kind: 'unpriced', user: '' })).toEqual([])
  })
})

describe('filterEvents provider and route', () => {
  const rows = [
    event({ provider: 'alibaba', route: 'chat/completions' }),
    event({ provider: 'amd', route: 'embeddings' }),
    event({ route: 'pricing', kind: 'unpriced' }),
  ]
  it('matches provider exactly, case-insensitively', () => {
    expect(filterEvents(rows, { kind: '', user: '', provider: 'AMD' })).toHaveLength(1)
  })
  it('matches route by substring', () => {
    expect(filterEvents(rows, { kind: '', user: '', route: 'chat' })).toHaveLength(1)
  })
  it('ignores absent provider and route filters', () => {
    expect(filterEvents(rows, { kind: '', user: '' })).toHaveLength(3)
  })
})

describe('EVENT_KIND_LABEL router_escalation and paced', () => {
  it('labels both', () => {
    expect(EVENT_KIND_LABEL.paced).toBeTruthy()
    expect(EVENT_KIND_LABEL.router_escalation).toBeTruthy()
  })
})

describe('event filter hash params', () => {
  it('reads the four params, dropping empties', () => {
    expect(eventFilterFromParams({ kind: 'upstream', user: ' bob ', provider: 'amd', route: '' })).toEqual({ kind: 'upstream', user: 'bob', provider: 'amd', route: '' })
  })
  it('round-trips through filter items', () => {
    const f = { kind: 'upstream', user: 'bob', provider: 'amd', route: 'chat' }
    expect(eventFilterFromParams(paramsFromItems(itemsFromFilter(f)))).toEqual(f)
  })
  it('keeps one value per field, the last winning', () => {
    const items = [
      { id: 'a', field: 'kind', value: 'budget' },
      { id: 'b', field: 'kind', value: 'upstream' },
    ]
    expect(paramsFromItems(items)).toEqual({ kind: 'upstream' })
  })
  it('ignores unknown fields', () => {
    expect(paramsFromItems([{ id: 'a', field: 'nope', value: 'x' }])).toEqual({})
  })
})
