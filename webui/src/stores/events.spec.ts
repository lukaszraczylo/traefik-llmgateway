// events.spec.ts covers useEventsStore's refresh action and its polling
// wiring — same harness as dashboard.spec.ts/history.spec.ts: a Pinia
// store is plain reactive state in the 'node' environment, adminFetch
// fully mocked, so no real network or sessionStorage access happens.

import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/lib/api', async () => {
  const actual = await vi.importActual<typeof import('@/lib/api')>('@/lib/api')
  return { ...actual, adminFetch: vi.fn() }
})

import { AdminApiError, adminFetch } from '@/lib/api'
import { useAuthStore } from '@/stores/auth'
import { useEventsStore } from '@/stores/events'
import type { AdminEventsResponse } from '@/types/api'

const mockedAdminFetch = vi.mocked(adminFetch)

function eventsResponse(overrides: Partial<AdminEventsResponse> = {}): AdminEventsResponse {
  return {
    events: [
      {
        time: '2026-08-20T12:00:00Z',
        replica: 'pod-a',
        route: 'chat/completions',
        kind: 'rate_limit',
        message: 'requests/min limit exceeded',
        status: 429,
        user: 'alice',
      },
    ],
    source: 'redis',
    replica: 'pod-a',
    capacity: 200,
    ...overrides,
  }
}

describe('useEventsStore.refresh', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    useAuthStore().submit('test-admin-key')
  })

  it('populates events/source/replica/capacity on success and clears error', async () => {
    mockedAdminFetch.mockResolvedValue(eventsResponse())
    const events = useEventsStore()

    await events.refresh()

    expect(events.events).toHaveLength(1)
    expect(events.source).toBe('redis')
    expect(events.replica).toBe('pod-a')
    expect(events.capacity).toBe(200)
    expect(events.degraded).toBe(false)
    expect(events.error).toBe('')
    expect(events.lastUpdated).not.toBeNull()
  })

  it('requests the ring capacity as the limit', async () => {
    mockedAdminFetch.mockResolvedValue(eventsResponse())
    const events = useEventsStore()

    await events.refresh()

    expect(mockedAdminFetch).toHaveBeenCalledWith('/admin/api/events?limit=200')
  })

  it('surfaces degraded when the response marks it', async () => {
    mockedAdminFetch.mockResolvedValue(eventsResponse({ source: 'replica', degraded: true }))
    const events = useEventsStore()

    await events.refresh()

    expect(events.source).toBe('replica')
    expect(events.degraded).toBe(true)
  })

  it('defaults degraded to false when the response omits it (replica source, Redis simply unconfigured)', async () => {
    mockedAdminFetch.mockResolvedValue(eventsResponse({ source: 'replica', degraded: undefined }))
    const events = useEventsStore()

    await events.refresh()

    expect(events.degraded).toBe(false)
  })

  it('surfaces a non-auth failure as error and leaves events untouched', async () => {
    mockedAdminFetch.mockResolvedValue(eventsResponse())
    const events = useEventsStore()
    await events.refresh()
    const seeded = events.events

    mockedAdminFetch.mockRejectedValue(new Error('events store down'))
    await events.refresh()

    expect(events.error).toBe('events store down')
    expect(events.events).toBe(seeded) // never blanked on a failed poll
  })

  it('does not surface an error on a 401 (the auth store owns that case)', async () => {
    mockedAdminFetch.mockRejectedValue(new AdminApiError('unauthorized', 401))
    const events = useEventsStore()

    await events.refresh()

    expect(events.error).toBe('')
  })

  it('skips a refresh() call that starts while one is already in flight', async () => {
    let resolveFirst!: (v: AdminEventsResponse) => void
    mockedAdminFetch.mockImplementation(
      () => new Promise<AdminEventsResponse>((resolve) => (resolveFirst = resolve)) as ReturnType<typeof adminFetch>,
    )
    const events = useEventsStore()

    const first = events.refresh()
    const second = events.refresh()

    resolveFirst(eventsResponse())
    await Promise.all([first, second])

    expect(mockedAdminFetch).toHaveBeenCalledTimes(1)
  })

  it('no-ops before a key is stored', async () => {
    useAuthStore().reject('logged out')
    const events = useEventsStore()

    await events.refresh()

    expect(mockedAdminFetch).not.toHaveBeenCalled()
  })
})

describe('useEventsStore.startPolling / stopPolling', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    mockedAdminFetch.mockReset()
    mockedAdminFetch.mockResolvedValue(eventsResponse())
    useAuthStore().submit('test-admin-key')
  })

  it('fetches immediately on startPolling, then again every POLL_MS', async () => {
    vi.useFakeTimers()
    try {
      const events = useEventsStore()

      events.startPolling()
      await vi.advanceTimersByTimeAsync(0)
      expect(mockedAdminFetch).toHaveBeenCalledTimes(1)

      await vi.advanceTimersByTimeAsync(5000)
      expect(mockedAdminFetch).toHaveBeenCalledTimes(2)

      events.stopPolling()
    } finally {
      vi.useRealTimers()
    }
  })

  it('startPolling is idempotent', async () => {
    vi.useFakeTimers()
    try {
      const events = useEventsStore()

      events.startPolling()
      events.startPolling()
      await vi.advanceTimersByTimeAsync(0)

      expect(mockedAdminFetch).toHaveBeenCalledTimes(1)
      events.stopPolling()
    } finally {
      vi.useRealTimers()
    }
  })

  it('stopPolling stops further fetches', async () => {
    vi.useFakeTimers()
    try {
      const events = useEventsStore()

      events.startPolling()
      await vi.advanceTimersByTimeAsync(0)
      events.stopPolling()
      const callsAfterStop = mockedAdminFetch.mock.calls.length

      await vi.advanceTimersByTimeAsync(15_000)
      expect(mockedAdminFetch.mock.calls.length).toBe(callsAfterStop)
    } finally {
      vi.useRealTimers()
    }
  })
})

// initFromParams was removed (P2 item 10): ReliabilityPage.vue now keeps
// kindFilter/userFilter synced with nav.params via its own reactive
// `watch(() => [nav.params.kind, nav.params.user], ...)`, rather than a
// one-shot mount-time seed — that watch's own doc comment covers why.

describe('useEventsStore.setKindFilter / setUserFilter', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
  })

  it('setKindFilter sets the exact-match kind filter, including back to the "" (every kind) sentinel', () => {
    const events = useEventsStore()
    events.setKindFilter('timeout')
    expect(events.kindFilter).toBe('timeout')
    events.setKindFilter('')
    expect(events.kindFilter).toBe('')
  })

  it('setUserFilter sets the raw, un-normalized user/group search text', () => {
    const events = useEventsStore()
    events.setUserFilter('alice')
    expect(events.userFilter).toBe('alice')
  })
})
