import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it } from 'vitest'

import { useConsumersStore } from '@/stores/consumers'
import { useEventsStore } from '@/stores/events'
import { useFiltersStore } from '@/stores/filters'
import { resetSessionData } from '@/stores/session'
import type { AdminEventView } from '@/types/api'

describe('resetSessionData', () => {
  beforeEach(() => setActivePinia(createPinia()))

  it('clears API data held by data stores', () => {
    const events = useEventsStore()
    events.events = [{ kind: 'x' } as unknown as AdminEventView]
    events.error = 'boom'
    resetSessionData()
    expect(events.events).toEqual([])
    expect(events.error).toBe('')
  })

  it('returns a populated consumers store to its initial state', () => {
    const consumers = useConsumersStore()
    const initial = JSON.stringify(consumers.$state)
    consumers.error = 'stale'
    resetSessionData()
    expect(JSON.stringify(consumers.$state)).toBe(initial)
  })

  it('keeps UI state such as the global filters', () => {
    const filters = useFiltersStore()
    const before = JSON.stringify(filters.$state)
    resetSessionData()
    expect(JSON.stringify(filters.$state)).toBe(before)
  })
})
