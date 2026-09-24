import { defineStore } from 'pinia'

import { adminFetch, isAuthRejection, messageOf } from '@/lib/api'
import { createVisibilityPoller, type VisibilityPoller } from '@/lib/polling'
import { useAuthStore } from '@/stores/auth'
import { useToastsStore } from '@/stores/toasts'
import type { AdminOverviewResponse, AdminTargetsResponse, AdminUsageResponse } from '@/types/api'

/** How often the Providers/Usage/Targets views poll (matches the replaced vanilla-JS page). */
const POLL_MS = 5000

/**
 * useDashboardStore polls GET /admin/api/overview, GET /admin/api/usage,
 * and GET /admin/api/targets together every POLL_MS — the fleet-wide,
 * always-current data every page in the redesigned shell reads from
 * (Home's KPI tiles, ConsumerDirectory/UserDetail's own usage rows,
 * ProviderHealthPanel's provider list, TargetsView's target list). A
 * single store (not three) because every route is always fetched
 * together — the old vanilla-JS page's own Promise.all([overview, usage])
 * pattern, extended to targets (Feature B, v0.21) rather than given its
 * own separate poll timer.
 */
export const useDashboardStore = defineStore('dashboard', {
  state: () => ({
    overview: null as AdminOverviewResponse | null,
    usage: null as AdminUsageResponse | null,
    targets: null as AdminTargetsResponse | null,
    lastUpdated: null as Date | null,
    error: '',
    poller: undefined as VisibilityPoller | undefined,
    /**
     * refreshing guards against overlapping polls (review finding): the 5s
     * setInterval has no in-flight check on its own, so a round trip that
     * takes longer than POLL_MS (a slow Redis behind /admin/api/overview,
     * say) would otherwise stack a second refresh() on top of the first —
     * two requests per section in flight at once, free to resolve in
     * either order and briefly show older data after the newer one already
     * landed. A tick that finds a refresh still in flight simply skips —
     * the next tick (or the already-in-flight one finishing) catches up,
     * so the dashboard never resorts to comparing response ordering.
     */
    refreshing: false,
    /** toastedForFailure guards states-plan.md item 3's "one toast per failure streak until it recovers" — set once a background-refresh-failure toast has fired for the CURRENT ongoing failure streak, reset the moment a refresh fully succeeds again (failures.length === 0 in doRefresh). */
    toastedForFailure: false,
  }),
  actions: {
    async refresh(): Promise<void> {
      const auth = useAuthStore()
      if (!auth.isAuthenticated) return
      if (this.refreshing) return
      this.refreshing = true
      try {
        await this.doRefresh()
      } finally {
        this.refreshing = false
      }
    },
    /** doRefresh is refresh()'s actual body, split out so the in-flight guard above wraps it in one place rather than every early-return branch below needing its own `finally`. */
    async doRefresh(): Promise<void> {
      // Captured BEFORE this tick's results are assigned below — overview/
      // usage/targets are never cleared on a failed fetch (only ever
      // assigned on a FULFILLED result), so this is a true "did the reader
      // already have something on screen before this attempt" read, not
      // just "did the previous refresh fully succeed" (a first attempt
      // that partially succeeds still advances lastUpdated below, which
      // would otherwise make a same-tick check on `lastUpdated` see this
      // attempt's own partial success instead of the PRIOR state).
      const hadDataBefore = this.overview !== null || this.usage !== null || this.targets !== null

      const [overviewResult, usageResult, targetsResult] = await Promise.allSettled([
        adminFetch<AdminOverviewResponse>('/admin/api/overview'),
        adminFetch<AdminUsageResponse>('/admin/api/usage'),
        adminFetch<AdminTargetsResponse>('/admin/api/targets'),
      ])

      // Each section is assigned independently (review round 2, v0.21
      // fix): one endpoint failing (e.g. GET /admin/api/targets) must
      // never blank the OTHER two, already-populated sections. The
      // earlier Promise.all threw on the FIRST rejection and updated
      // nothing at all — a single flaky route degraded every view in the
      // dashboard, not just its own.
      if (overviewResult.status === 'fulfilled') this.overview = overviewResult.value
      if (usageResult.status === 'fulfilled') this.usage = usageResult.value
      if (targetsResult.status === 'fulfilled') this.targets = targetsResult.value

      const failures: { label: string; reason: unknown }[] = []
      if (overviewResult.status === 'rejected') failures.push({ label: 'overview', reason: overviewResult.reason })
      if (usageResult.status === 'rejected') failures.push({ label: 'usage', reason: usageResult.reason })
      if (targetsResult.status === 'rejected') failures.push({ label: 'targets', reason: targetsResult.reason })

      if (failures.length === 0) {
        this.lastUpdated = new Date()
        this.error = ''
        this.toastedForFailure = false
        return
      }

      // A 401/403 on ANY section already rejected the key (lib/api.ts) —
      // the AuthGate takes over the whole view in that case, matching the
      // pre-existing single-Promise.all behavior; nothing left to report.
      const authRejected = failures.some(({ reason }) => isAuthRejection(reason))
      if (authRejected) return

      // A partial failure still advances lastUpdated: at least one
      // section genuinely has fresh data, even though `error` (below)
      // still surfaces that something is degraded.
      if (failures.length < 3) this.lastUpdated = new Date()
      this.error = failures.map(({ label, reason }) => `${label}: ${messageOf(reason)}`).join('; ')

      // states-plan.md item 3: a background refresh failure while data is
      // ALREADY shown gets a toast instead of wiping content (the header's
      // own inline `error` text already covers that case too, but a toast
      // surfaces it even when the reader is looking at a different page
      // than the one showing the stale data) — never fired on a genuine
      // first-load failure (hadDataBefore false), which is an ErrorState's
      // job instead, and never repeated on every tick of an ONGOING
      // failure streak (toastedForFailure).
      if (hadDataBefore && !this.toastedForFailure) {
        useToastsStore().push({ kind: 'error', message: `Dashboard refresh failed: ${this.error}` })
      }
      this.toastedForFailure = true
    },
    /**
     * startPolling is idempotent and safe to call before a key is stored —
     * refresh() no-ops until then. Driven by the shared
     * createVisibilityPoller (lib/polling.ts, F5) rather than a bare
     * setInterval: an immediate tick (the same "fetch once, then poll"
     * shape this action already had), then every POLL_MS while the tab is
     * visible, pausing while it is hidden and catching up the moment it is
     * looked at again.
     */
    startPolling(): void {
      if (this.poller) return
      this.poller = createVisibilityPoller({ intervalMs: POLL_MS, tick: () => void this.refresh() })
      this.poller.start()
    },
    // P11 review fix: a stopPolling action mirroring events.ts's own
    // stop() used to live here too, but nothing outside its OWN spec ever
    // called it — App.vue mounts this store's poller once, for the app's
    // whole session, and never unmounts it (unlike events.ts, whose store
    // IS torn down when EventsView unmounts — the same page-scoped
    // pattern the pre-redesign Usage/Charts tabs' own now-deleted stores
    // used to follow too). Dead application-facing API surface, removed;
    // `poller` itself stays (startPolling's own idempotency guard still
    // needs it).
  },
})
