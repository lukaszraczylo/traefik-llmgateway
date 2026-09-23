import { defineStore } from 'pinia'

import { AdminApiError, adminFetch } from '@/lib/api'
import { createVisibilityPoller, type VisibilityPoller } from '@/lib/polling'
import { useAuthStore } from '@/stores/auth'
import { useToastsStore } from '@/stores/toasts'
import type { AdminEventsResponse, AdminEventView } from '@/types/api'

/** How often the Events view polls while mounted — same cadence as the Providers/Usage/Targets dashboard poll (stores/dashboard.ts's own POLL_MS): a rate-limit/upstream event is exactly the kind of thing an operator wants to see within a few seconds, not thirty. */
const POLL_MS = 5000

/** EVENTS_LIMIT matches the ring's own fixed capacity (events.go: eventRingCap, admin.go: adminEventsMaxLimit) — asking for fewer would silently hide events the ring/Redis list still holds. */
const EVENTS_LIMIT = 200

/**
 * useEventsStore polls GET /admin/api/events?limit=200, feeding
 * EventsView.vue — the Reliability page's own embedded events section
 * (redesign-plan.md section 3.4), and Home's own latest-8-events list.
 * Unlike useDashboardStore, this store is polled only while a page that
 * reads it is mounted (EventsView.vue's own onMounted/onUnmounted) rather
 * than for the app's whole lifetime — an operator who never opens
 * Reliability or Home pays no extra request for it.
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
     * kindFilter/userFilter — the Events section's own kind Select and
     * user SearchInput (redesign-plan.md section 3.4, EventsView.vue
     * reused as a Reliability page section). Lifted up here rather than
     * kept as local refs in EventsView.vue so ReliabilityPage.vue can
     * keep them synced with nav.params (`kind`/`user`, section 3.1's
     * Reliability page params) via its own reactive `watch(() =>
     * [nav.params.kind, nav.params.user], ...)` — not just seeded once on
     * mount (P2 item 10: a one-shot mount-time seed left a filter set on
     * an earlier visit stuck in place across a remount with the param
     * cleared, and never picked up a hash change, e.g. browser back/
     * forward, while already mounted). kindFilter is an exact
     * AdminEventKind string, or '' for "every kind" (EventsView.vue's own
     * ALL_KINDS sentinel maps to/from this at the template boundary — see
     * lib/events-filter.ts's own EventFilter shape, which this pairs with
     * directly). userFilter is the raw, un-normalized search text;
     * EventsView.vue binds it via useSearchQuery's external-ref option
     * (composables/useSearchQuery.ts).
     */
    kindFilter: '',
    userFilter: '',
    poller: undefined as VisibilityPoller | undefined,
    /** Mirrors dashboard.ts's own in-flight guard: a round trip slower than POLL_MS must never stack a second refresh() on top of the first. */
    refreshing: false,
    /** toastedForFailure mirrors dashboard.ts's own flag: states-plan.md item 3's "one toast per failure streak per store until it recovers". */
    toastedForFailure: false,
  }),
  actions: {
    async refresh(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      if (this.refreshing) return
      this.refreshing = true
      // Captured before this tick's own result lands — see dashboard.ts's
      // doRefresh's identical hadDataBefore doc comment: `error` alone is
      // stale-friendly (a previous failure leaves it set), but `lastUpdated`
      // is null exactly until the FIRST successful fetch ever completes,
      // regardless of how many failures came before it.
      const hadDataBefore = this.lastUpdated !== null
      try {
        const res = await adminFetch<AdminEventsResponse>(`/admin/api/events?limit=${EVENTS_LIMIT}`)
        this.events = res.events
        this.source = res.source
        this.replica = res.replica
        this.capacity = res.capacity
        this.degraded = res.degraded ?? false
        this.lastUpdated = new Date()
        this.error = ''
        this.toastedForFailure = false
      } catch (err) {
        // A 401/403 already rejected the key (lib/api.ts) — the AuthGate
        // takes over the whole view, matching dashboard.ts's own
        // identical carve-out; nothing left to report here.
        if (err instanceof AdminApiError && (err.status === 401 || err.status === 403)) return
        this.error = err instanceof Error ? err.message : String(err)
        // states-plan.md item 3 — see dashboard.ts's doRefresh for the
        // full reasoning: only when the reader already had a successful
        // fetch to show, and only once per ongoing failure streak.
        if (hadDataBefore && !this.toastedForFailure) {
          useToastsStore().push({ kind: 'error', message: `Events refresh failed: ${this.error}` })
        }
        this.toastedForFailure = true
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
