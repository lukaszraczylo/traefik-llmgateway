import { defineStore } from 'pinia'

import { AdminApiError, adminFetch } from '@/lib/api'
import { createVisibilityPoller, type VisibilityPoller } from '@/lib/polling'
import { useAuthStore } from '@/stores/auth'
import type { AdminEventsResponse, AdminEventView } from '@/types/api'

/** How often the Events view polls while mounted — same cadence as the Providers/Usage/Targets dashboard poll (stores/dashboard.ts's own POLL_MS): a rate-limit/upstream event is exactly the kind of thing an operator wants to see within a few seconds, not thirty. */
const POLL_MS = 5000

/** EVENTS_LIMIT matches the ring's own fixed capacity (events.go: eventRingCap, admin.go: adminEventsMaxLimit) — asking for fewer would silently hide events the ring/Redis list still holds. */
const EVENTS_LIMIT = 200

/**
 * useEventsStore polls GET /admin/api/events?limit=200, feeding the new
 * Events tab (App.vue, F3). Unlike useDashboardStore, this store is
 * polled only while the Events view is mounted (EventsView.vue's own
 * onMounted/onUnmounted) rather than for the app's whole lifetime — an
 * operator who never opens the tab pays no extra request for it.
 */
export const useEventsStore = defineStore('events', {
  state: () => ({
    /** Newest first, exactly as the server orders them — see types/api.ts's AdminEventsResponse doc comment. */
    events: [] as AdminEventView[],
    /** 'redis' (fleet-wide) or 'replica' (this replica's own in-memory ring, read because Redis is unreachable or unconfigured) — the view must caption this rather than present a replica-only fallback as fleet-wide. */
    source: 'replica' as 'redis' | 'replica',
    replica: '',
    /** The ring's fixed capacity (events.go: eventRingCap) — lets the view show "N of capacity" rather than a bare list length. */
    capacity: 0,
    /** Set only when Redis IS configured but the read failed (never when Redis is simply not configured at all — see AdminEventsResponse's own doc comment). */
    degraded: false,
    lastUpdated: null as Date | null,
    error: '',
    /**
     * kindFilter/userFilter (F9, hash-state) — the Events view's own kind
     * Select and user SearchInput. Lifted up here rather than kept as
     * local refs in EventsView.vue, mirroring stores/nav.ts's own
     * usageQuery doc comment: state that must survive a #<tab>?<query>
     * hash round trip (composables/useHashState.ts, lib/hash-state.ts's
     * parseEventsParams/kind,user) needs one canonical place to read from
     * and write to, not a component-local ref useHashState cannot reach.
     * kindFilter is an exact AdminEventKind string, or '' for "every
     * kind" (EventsView.vue's own ALL_KINDS sentinel maps to/from this at
     * the template boundary — see lib/events-filter.ts's own EventFilter
     * shape, which this pairs with directly). userFilter is the raw,
     * un-normalized search text; EventsView.vue binds it via
     * useSearchQuery's external-ref option (composables/useSearchQuery.ts)
     * the same way UsageView.vue binds nav.usageQuery.
     */
    kindFilter: '',
    userFilter: '',
    poller: undefined as VisibilityPoller | undefined,
    /** Mirrors dashboard.ts's own in-flight guard: a round trip slower than POLL_MS must never stack a second refresh() on top of the first. */
    refreshing: false,
  }),
  actions: {
    async refresh(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      if (this.refreshing) return
      this.refreshing = true
      try {
        const res = await adminFetch<AdminEventsResponse>(`/admin/api/events?limit=${EVENTS_LIMIT}`)
        this.events = res.events
        this.source = res.source
        this.replica = res.replica
        this.capacity = res.capacity
        this.degraded = res.degraded ?? false
        this.lastUpdated = new Date()
        this.error = ''
      } catch (err) {
        // A 401/403 already rejected the key (lib/api.ts) — the AuthGate
        // takes over the whole view, matching dashboard.ts's own
        // identical carve-out; nothing left to report here.
        if (err instanceof AdminApiError && (err.status === 401 || err.status === 403)) return
        this.error = err instanceof Error ? err.message : String(err)
      } finally {
        this.refreshing = false
      }
    },
    /** setKindFilter sets the exact-match kind filter — '' means "every kind" (EventsView.vue translates its own ALL_KINDS Select sentinel to/from '' at the template boundary). */
    setKindFilter(kind: string): void {
      this.kindFilter = kind
    },
    /** setUserFilter sets the raw (un-normalized) user/group search text — EventsView.vue reads the normalized form through useSearchQuery's own `normalized` computed rather than this action re-deriving it. */
    setUserFilter(user: string): void {
      this.userFilter = user
    },
    /** startPolling is idempotent, driven by the shared createVisibilityPoller (lib/polling.ts, F5) — an immediate fetch, then every POLL_MS while the tab is visible. */
    startPolling(): void {
      if (this.poller) return
      this.poller = createVisibilityPoller({ intervalMs: POLL_MS, tick: () => void this.refresh() })
      this.poller.start()
    },
    stopPolling(): void {
      this.poller?.stop()
      this.poller = undefined
    },
  },
})
