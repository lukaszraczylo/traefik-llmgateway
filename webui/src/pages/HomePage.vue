<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'

import ErrorState from '@/components/ErrorState.vue'
import KpiTile from '@/components/KpiTile.vue'
import LoadStateView from '@/components/LoadStateView.vue'
import SkeletonKpiTile from '@/components/SkeletonKpiTile.vue'
import TopList from '@/components/TopList.vue'
import type { TopListItem } from '@/components/TopList.vue'
import TryLongerRangeButton from '@/components/TryLongerRangeButton.vue'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { AdminApiError, adminFetch, messageOf } from '@/lib/api'
import { eventKindVariant, kindLabel } from '@/lib/events-filter'
import { formatCompactCount, formatCost, formatTimestamp } from '@/lib/format'
import { headroom, monthProgress, NOT_ENOUGH_DATA, projectMonthEnd } from '@/lib/forecast'
import {
  budgetRatio,
  budgetTier,
  errorRateTier,
  fleetErrorRate,
  openBreakerCount,
  sumGroupBudgetMicros,
} from '@/lib/kpi'
import { loadState } from '@/lib/load-state'
import { createVisibilityPoller } from '@/lib/polling'
import type { VisibilityPoller } from '@/lib/polling'
import { formatErrorRatePercent } from '@/lib/provider-rate'
import {
  DEFAULT_TOP_USERS_MODE,
  parseTopUsersMode,
  rankTopUsers,
  TOP_USERS_METRICS_BY_MODE,
  TOP_USERS_MODE_LABEL,
  TOP_USERS_MODES,
  topUsersCapCheckResult,
  topUsersTotalsUrl,
} from '@/lib/top-users'
import type { TopUsersMode } from '@/lib/top-users'
import { microsToUsd } from '@/lib/usage-bars'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { useEventsStore } from '@/stores/events'
import { useFiltersStore } from '@/stores/filters'
import { useNavStore } from '@/stores/nav'
import type { AdminTotalsResponse, AdminUsageModelsResponse } from '@/types/api'

/**
 * HomePage (redesign-plan.md section 3.4) is the redesign's landing
 * page: fleet-wide KPI tiles, the top-5 spenders and top-5 models for
 * the current global range, the 8 most recent events, and a count of
 * config warnings / unpriced models. Every KPI tile reads from
 * stores/dashboard.ts's existing 5s poll (overview/usage) — no extra
 * round trip for figures that are already fetched fleet-wide. The two
 * top-5 lists are the one genuinely NEW read this page makes (GET
 * /admin/api/usage/totals?kind=user, and the existing GET /admin/api/
 * usage/models ranking), refetched on a 60s poll rather than every 5s
 * (Q12, redesign-plan.md DECISIONS: "new endpoints on demand or 60s").
 */
const TOP_LIST_LIMIT = 5
const TOP_LIST_POLL_MS = 60_000

const auth = useAuthStore()
const dashboard = useDashboardStore()
const events = useEventsStore()
const filters = useFiltersStore()
const nav = useNavStore()

// --- Spend vs budget ---------------------------------------------------

const spendTodayMicros = computed<number>(() => dashboard.usage?.total.costPerDayMicroUsd ?? 0)
const spendMtdMicros = computed<number>(() => dashboard.usage?.total.costPerMonthMicroUsd ?? 0)
/** Q2 (DECISIONS): the Home budget baseline is the sum of every configured group's costPerMonthUSD — "no budget set" reads as 0 here. */
const budgetMicros = computed<number>(() => sumGroupBudgetMicros(dashboard.overview?.groups ?? []))
const spendTier = computed(() => budgetTier(budgetRatio(spendMtdMicros.value, budgetMicros.value)))
const spendDelta = computed<string>(() => {
  const today = `${formatCost(spendTodayMicros.value)} today`
  return budgetMicros.value > 0 ? `${today} · budget ${formatCost(budgetMicros.value)}/mo` : `${today} · no budget set`
})

// --- Projected month-end (lib/forecast.ts) ------------------------------

const projectedMicros = computed<number | null>(() => projectMonthEnd(spendMtdMicros.value, monthProgress(new Date())))
const projectedHeadroom = computed(() =>
  headroom(projectedMicros.value, budgetMicros.value > 0 ? microsToUsd(budgetMicros.value) : undefined),
)
const projectedValue = computed<string>(() =>
  projectedMicros.value === null ? NOT_ENOUGH_DATA : formatCost(projectedMicros.value),
)
const projectedTier = computed(() => (projectedHeadroom.value.willExceed ? 'critical' : 'ok'))
const projectedDelta = computed<string | undefined>(() => {
  if (projectedMicros.value === null) return undefined
  if (budgetMicros.value <= 0) return 'no budget set'
  return projectedHeadroom.value.willExceed ? 'projected to exceed budget' : 'projected within budget'
})

// --- Requests/min --------------------------------------------------------

const requestsPerMinute = computed<number>(() => dashboard.usage?.total.requestsPerMinute ?? 0)

// --- Fleet error rate (lib/kpi.ts) ---------------------------------------

const errorRate = computed(() => fleetErrorRate(dashboard.overview?.providers ?? []))
const errorTier = computed(() => errorRateTier(errorRate.value.rate))
/** errorValue reuses lib/provider-rate.ts's shared formatErrorRatePercent (reuse-audit.md F13) — the Home tile's own "no traffic" null wording is the ONE difference from the Reliability/Models pages' "no data" default, passed via the nullLabel param rather than a second, hand-rolled copy of the same ceiling-percent formatting. */
const errorValue = computed<string>(() => formatErrorRatePercent(errorRate.value.rate, 'no traffic'))
const errorDelta = computed<string>(() => `${errorRate.value.failures}/${errorRate.value.attempts} attempts in the last minute`)

// --- Open breakers / warnings / unpriced ---------------------------------

const breakerCount = computed<number>(() => openBreakerCount(dashboard.overview?.providers ?? []))
const warningCount = computed<number>(() => dashboard.overview?.warnings.length ?? 0)
const unpricedCount = computed<number>(() => dashboard.overview?.pricing.unpriced ?? 0)

/**
 * kpiTilesState (states-plan.md item 1) gates the KPI grid's skeleton/error
 * branches via the shared lib/load-state.ts helper — every KpiTile computed
 * above already defaults to a sensible zero/fallback reading (`?? 0`, "not
 * enough data yet") when dashboard.overview/usage is null, which used to
 * render as a confusing flash of "$0.0000"/"0" tiles (or "no traffic") on
 * first load, or a PERMANENT one on a first-load failure — indistinguishable
 * from a fleet that has genuinely spent/seen nothing, rather than a loading
 * or error state. `loading: true` mirrors TargetsView.vue's/AccessMatrix.vue's
 * own identical idiom for a source with no separate in-flight flag of its
 * own: it is only ever consulted once `hasData` is already false, at which
 * point this page has nothing else to show but a skeleton anyway.
 *
 * `hasData` (verify-ui-states-2.md #5 fix) is `dashboard.overview !== null
 * && dashboard.usage !== null`, NOT `dashboard.lastUpdated !== null` —
 * dashboard.ts's own doRefresh still advances lastUpdated on a PARTIAL
 * success (`failures.length < 3`), which can leave overview and/or usage
 * still null (e.g. only /targets succeeded) while every tile above already
 * falls back to a `?? 0`/"no traffic" reading off that null source. The
 * old lastUpdated-based check read that combination as 'ready' and
 * rendered those fallbacks as real fleet facts ("$0.0000", "0" breakers)
 * instead of the error this page actually has to show. `overview`/`usage`
 * are never cleared on a failed refresh (dashboard.ts only ever assigns a
 * FULFILLED result), so once both have landed once, a later background
 * failure still reads 'ready' and the KPI figures stay stable across polls
 * — loadState's own "ready wins over a stale error" precedence.
 */
const kpiTilesState = computed(() =>
  loadState({ loading: true, hasData: dashboard.overview !== null && dashboard.usage !== null, error: dashboard.error }),
)

// --- Top 5 users by cost/requests/tokens / top 5 models by cost ----------

/**
 * topUsersMode is the "top users" card's Cost/Requests/Tokens Tabs
 * selection, kept in nav.params.top (lib/top-users.ts's own `top` hash
 * param convention: 'req'/'tokens' written, 'cost' omitted as the
 * default) so a reload or shared link restores the same ranking —
 * mirrors AccessMatrix.vue's `kind` prop / ConsumersPage.vue's
 * `matrix` param wiring.
 */
const topUsersMode = computed<TopUsersMode>(() => parseTopUsersMode(nav.params.top))

function setTopUsersMode(next: unknown): void {
  if (typeof next !== 'string' || !(TOP_USERS_MODES as readonly string[]).includes(next)) return
  const mode = next as TopUsersMode
  if (mode === DEFAULT_TOP_USERS_MODE) {
    const { top: _top, ...rest } = nav.params
    nav.goTo('home', rest)
    return
  }
  nav.goTo('home', { ...nav.params, top: mode })
}

/**
 * configuredUserCount (verify-ui-states-2.md #4 fix) is the EXACT
 * population the server's own kind=user cap check counts (stats_read.go:
 * 566-576, `ids` built from auth.snapshot()'s userSummaries) — read
 * directly off dashboard.usage.users, which has exactly one entry per
 * configured user (admin.go's own userSummaries, already polled every 5s).
 * An earlier revision summed each already-polled group's own memberCount
 * instead (dashboard.overview) — that over-counts any user who belongs to
 * more than one group, which could trip topUsersCapExceeded (lib/
 * top-users.ts) for a range the server would have served fine. Null before
 * dashboard.usage has ever loaded (a cold mount, or a load failure) —
 * topUsersCapCheckResult skips the predictive check entirely in that case
 * rather than guessing; fetchTopUsers below still catches the server's
 * own real 400 as a fallback for whatever this happens to miss.
 */
const configuredUserCount = computed<number | null>(() => dashboard.usage?.users.length ?? null)

/** topUsersRaw is the CURRENT mode's fetched /usage/totals?kind=user rows — topUsersCache below keeps every mode's own last-fetched rows so a tab switch back to an already-seen mode can show them immediately (states-plan.md item 1/4). */
const topUsersRaw = ref<AdminTotalsResponse['rows']>([])
const topModels = ref<TopListItem[]>([])
const topUsersLoading = ref(false)
const topUsersError = ref('')
const topModelsLoading = ref(false)
const topModelsError = ref('')
/**
 * topModelsLoaded (verify-ui-states-2.md #2 fix) is "has a fetch completed
 * for the CURRENT range's top-models card" — TopList.vue's own `loaded`
 * prop — set only on a successful fetchTopModels, reset on a genuine range
 * change (the watcher below). Distinct from `topModels.length > 0`: a
 * genuinely empty top-models card must keep reading as settled/empty
 * across the 60s poll's own `topModelsLoading` flips, not flash a skeleton
 * on every tick.
 */
const topModelsLoaded = ref(false)
/**
 * topUsersRangeTooLarge (verify-ui-states.md #1) is set either predictively
 * (configuredUserCount trips topUsersCapCheckResult, so fetchTopUsers
 * never even fires the doomed request) or reactively (the server's own 400
 * for this exact reason — AdminApiError.status === 400 with its "range too
 * large" message, stats_read.go:574) — either way this renders TopList's
 * own `notice` slot instead of a raw error, naming the actual fix (a
 * shorter range) rather than surfacing the server's terse 400 text.
 */
const topUsersRangeTooLarge = ref(false)
/**
 * topUsersCache keeps the last successfully fetched raw rows PER MODE, for
 * the CURRENT range only (cleared in the range watcher below) — a tab
 * switch to an already-fetched mode shows its rows immediately (no
 * skeleton) while a background refetch (still fired — "refetch on tab
 * switch", verify-ui-states.md #1) brings it current, the same
 * cached-reopen principle UserDetail.vue's own detail cache follows.
 * `reactive()` (not a plain Map, verify-ui-states-2.md #2 fix): topUsersLoaded
 * below reads `.has()` inside a `computed`, which needs Vue to track the
 * Map's own mutations (`.set()`/`.clear()`) as reactive dependencies — a
 * plain Map's mutations are invisible to Vue's reactivity system, so the
 * computed would never re-run when a fetch actually completes.
 */
const topUsersCache = reactive(new Map<TopUsersMode, AdminTotalsResponse['rows']>())
/**
 * topUsersLoaded (verify-ui-states-2.md #2 fix) is "has a fetch completed
 * for the CURRENT mode" — TopList.vue's own `loaded` prop for the top-users
 * card — reusing topUsersCache's own per-mode presence (set only on a
 * successful fetch, in fetchTopUsers below) rather than a duplicate flag: a
 * mode's cache entry existing already means at least one response has
 * landed for it in the current range, even when every one of its rows
 * ranks to 0 for this mode (rankTopUsers filters those out, e.g. Cost mode
 * over free-model-only traffic) — distinct from `topUsers.length > 0`,
 * which a genuinely empty selection can never satisfy and which used to
 * flash a skeleton back on over that settled empty state on every 60s poll
 * tick (`topUsersLoading` flips true on every tick, regardless of whether
 * this is the mode's first fetch).
 */
const topUsersLoaded = computed<boolean>(() => topUsersCache.has(topUsersMode.value))
/** topUsersReqId/topModelsReqId are two INDEPENDENT latest-request-wins counters (not one shared counter): a mode switch only refetches users, a range change refetches both, and the 60s poller ticks both — a shared counter would make an in-flight models fetch spuriously discard a still-relevant users fetch, or vice versa, merely for starting first. */
let topUsersReqId = 0
let topModelsReqId = 0

/** formatTopUserValue applies the existing formatter each mode's unit calls for — formatCost (already 2-decimal-cents-safe) for money, formatCompactCount for raw counts/token sums. */
function formatTopUserValue(mode: TopUsersMode, value: number): string {
  return mode === 'cost' ? formatCost(value) : formatCompactCount(value)
}

const topUsers = computed<TopListItem[]>(() =>
  rankTopUsers(topUsersRaw.value, topUsersMode.value).map((row) => ({
    id: row.id,
    label: row.id,
    value: formatTopUserValue(topUsersMode.value, row.value),
    kind: 'user' as const,
  })),
)

const topUsersTitle = computed(() => `Top 5 users by ${TOP_USERS_MODE_LABEL[topUsersMode.value].toLowerCase()}`)

/**
 * fetchTopUsers (verify-ui-states.md #1) requests ONLY the current mode's
 * own metric(s) — lib/top-users.ts's TOP_USERS_METRICS_BY_MODE — instead of
 * always asking for all four, and is called again on every mode switch
 * (the watch below), not just on mount/poll/range-change, since a
 * different mode now means a genuinely different request. Independent of
 * fetchTopModels (own loading/error/reqId) so one failing never blanks the
 * other's card.
 */
async function fetchTopUsers(): Promise<void> {
  if (!auth.isAuthenticated) return
  const mode = topUsersMode.value
  const metricsCount = TOP_USERS_METRICS_BY_MODE[mode].length
  if (topUsersCapCheckResult(configuredUserCount.value, metricsCount, filters.span)) {
    // verify-ui-states-2.md #1: invalidate any OLDER in-flight fetch (a
    // different mode's request, or the previous range's) the same way a
    // genuine fetch below does — without bumping topUsersReqId here, a
    // request that was already in flight before this predictive block
    // still passes the `requestId !== topUsersReqId` check once it lands,
    // and writes its own (now wrong mode/range) rows out from under this
    // notice.
    ++topUsersReqId
    topUsersLoading.value = false
    topUsersRangeTooLarge.value = true
    topUsersError.value = ''
    return
  }
  const requestId = ++topUsersReqId
  topUsersLoading.value = true
  try {
    const res = await adminFetch<AdminTotalsResponse>(topUsersTotalsUrl(filters.window, filters.span, mode))
    if (requestId !== topUsersReqId) return
    topUsersCache.set(mode, res.rows)
    topUsersRaw.value = res.rows
    topUsersError.value = ''
    topUsersRangeTooLarge.value = false
  } catch (err) {
    if (requestId !== topUsersReqId) return
    if (err instanceof AdminApiError && err.status === 400 && /range too large/i.test(err.message)) {
      topUsersRangeTooLarge.value = true
      topUsersError.value = ''
    } else {
      topUsersError.value = messageOf(err)
    }
  } finally {
    if (requestId === topUsersReqId) topUsersLoading.value = false
  }
}

async function fetchTopModels(): Promise<void> {
  if (!auth.isAuthenticated) return
  const requestId = ++topModelsReqId
  topModelsLoading.value = true
  try {
    const res = await adminFetch<AdminUsageModelsResponse>(
      `/admin/api/usage/models?metric=cost&window=${filters.window}&span=${filters.span}&limit=${TOP_LIST_LIMIT}`,
    )
    if (requestId !== topModelsReqId) return
    topModels.value = res.models.map((m) => ({
      id: m.id,
      label: m.id,
      value: formatCost(m.value),
      kind: 'model' as const,
    }))
    topModelsError.value = ''
    topModelsLoaded.value = true
  } catch (err) {
    if (requestId !== topModelsReqId) return
    topModelsError.value = messageOf(err)
  } finally {
    if (requestId === topModelsReqId) topModelsLoading.value = false
  }
}

/** fetchTopLists fires both independently (Promise.allSettled, not Promise.all) — each of fetchTopUsers/fetchTopModels already catches its own errors internally, so neither can reject and blank the other's card (verify-ui-states.md #1). The one shared entry point mount/poll/range-change all call. */
async function fetchTopLists(): Promise<void> {
  await Promise.allSettled([fetchTopUsers(), fetchTopModels()])
}

let topListsPoller: VisibilityPoller | undefined

watch(topUsersMode, (mode) => {
  // A tab switch is a genuine SELECTION change, but — unlike a range
  // change — an already-fetched mode's rows stay cached (topUsersCache):
  // show them immediately (no skeleton flash for data already in hand),
  // then still fire a background refetch so they come current.
  const cached = topUsersCache.get(mode)
  topUsersRaw.value = cached ?? []
  topUsersError.value = ''
  topUsersRangeTooLarge.value = false
  void fetchTopUsers()
})

watch(
  () => filters.range,
  () => {
    // An explicit range change is a genuine SELECTION change for BOTH
    // cards, unlike the 60s poller's own background tick — states-plan.md
    // item 1's rule ("skeleton only for the current selection's own
    // missing data") means the previous range's rows (and every mode's
    // cache, which is only ever valid for the range it was fetched under)
    // are cleared here so the skeleton shows for the new range's own
    // fetch, rather than leaving the old range's numbers on screen under
    // the (about to change) heading until the new response lands.
    topUsersCache.clear()
    topUsersRaw.value = []
    topModels.value = []
    topModelsLoaded.value = false
    topUsersError.value = ''
    topModelsError.value = ''
    topUsersRangeTooLarge.value = false
    void fetchTopLists()
  },
)

// --- Latest 8 events -------------------------------------------------------

const latestEvents = computed(() => events.events.slice(0, 8))

/** eventsCardState (states-plan.md gap item: "Home Latest events card still says 'No events recorded yet.' while loading or on error") mirrors EventsView.vue's own eventsLoadState — hasData is "has this store ever completed a fetch", not "are there events", so a clean fleet with lastUpdated set still reads 'ready' and falls through to noEventsRecorded below, not a permanent skeleton. */
const eventsCardState = computed(() => loadState({ loading: events.refreshing, hasData: events.lastUpdated !== null, error: events.error }))
const noEventsRecorded = computed(() => latestEvents.value.length === 0)

onMounted(() => {
  // Events store is polled only while a page that reads it is mounted
  // (its own doc comment) — Home is one more such consumer, alongside
  // the (future) Reliability page's own EventsView section.
  events.startPolling()
  topListsPoller = createVisibilityPoller({ intervalMs: TOP_LIST_POLL_MS, tick: () => void fetchTopLists() })
  topListsPoller.start()
})
onUnmounted(() => {
  events.stopPolling()
  topListsPoller?.stop()
})
</script>

<template>
  <div class="flex flex-col gap-4">
    <section aria-label="Key metrics" class="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4">
      <template v-if="kpiTilesState === 'skeleton'">
        <SkeletonKpiTile v-for="i in 7" :key="i" />
      </template>
      <div v-else-if="kpiTilesState === 'error'" class="col-span-full">
        <ErrorState :message="dashboard.error" :on-retry="dashboard.refresh" />
      </div>
      <template v-else>
        <KpiTile label="Spend (MTD)" :value="formatCost(spendMtdMicros)" :delta="spendDelta" :tier="spendTier" :to="{ page: 'spend' }" />
        <KpiTile
          label="Projected month-end"
          :value="projectedValue"
          :delta="projectedDelta"
          :tier="projectedTier"
          :to="{ page: 'spend' }"
        />
        <KpiTile label="Requests/min" :value="formatCompactCount(requestsPerMinute)" />
        <KpiTile
          label="Error rate (last minute)"
          :value="errorValue"
          :delta="errorDelta"
          :tier="errorTier"
          :to="{ page: 'reliability' }"
        />
        <KpiTile
          label="Open breakers (per replica)"
          :value="String(breakerCount)"
          :tier="breakerCount > 0 ? 'warn' : 'ok'"
          :to="{ page: 'models' }"
        />
        <KpiTile
          label="Config warnings"
          :value="String(warningCount)"
          :tier="warningCount > 0 ? 'warn' : 'ok'"
          :to="{ page: 'config' }"
        />
        <KpiTile
          label="Unpriced models"
          :value="String(unpricedCount)"
          :tier="unpricedCount > 0 ? 'warn' : 'ok'"
          :to="{ page: 'spend' }"
        />
      </template>
    </section>

    <section aria-label="Top spenders and models" class="grid grid-cols-1 gap-3 lg:grid-cols-2">
      <TopList
        :title="topUsersTitle"
        :items="topUsers"
        empty-text="No traffic in this range."
        :loading="topUsersLoading"
        :loaded="topUsersLoaded"
        :error="topUsersError"
        :on-retry="fetchTopUsers"
        :notice="topUsersRangeTooLarge ? 'Too many configured users for this range.' : undefined"
        notice-description="Pick a shorter range above — a fleet this size needs a narrower window to rank users."
      >
        <template #controls>
          <Tabs :model-value="topUsersMode" @update:model-value="setTopUsersMode">
            <TabsList>
              <TabsTrigger v-for="m in TOP_USERS_MODES" :key="m" :value="m">{{ TOP_USERS_MODE_LABEL[m] }}</TabsTrigger>
            </TabsList>
          </Tabs>
        </template>
        <template #empty-action>
          <TryLongerRangeButton />
        </template>
      </TopList>
      <TopList
        title="Top 5 models by cost"
        :items="topModels"
        empty-text="No traffic in this range."
        :loading="topModelsLoading"
        :loaded="topModelsLoaded"
        :error="topModelsError"
        :on-retry="fetchTopModels"
      >
        <template #empty-action>
          <TryLongerRangeButton />
        </template>
      </TopList>
    </section>

    <Card>
      <CardHeader>
        <CardTitle>Latest events</CardTitle>
      </CardHeader>
      <CardContent>
        <LoadStateView :state="eventsCardState" :error="events.error" :on-retry="events.refresh" skeleton="list" :rows="4" :empty="noEventsRecorded" empty-title="No events recorded yet.">
          <ol class="flex flex-col gap-2">
            <li
              v-for="(event, i) in latestEvents"
              :key="`${event.time}-${event.replica}-${i}`"
              class="flex items-start justify-between gap-3 text-sm"
            >
              <span class="flex min-w-0 items-center gap-2">
                <Badge :variant="eventKindVariant(event.kind)" class="shrink-0 font-normal">{{ kindLabel(event.kind) }}</Badge>
                <span class="min-w-0 truncate text-muted-foreground">{{ event.message }}</span>
              </span>
              <span class="shrink-0 tabular-nums text-muted-foreground">{{ formatTimestamp(event.time) }}</span>
            </li>
          </ol>
        </LoadStateView>
      </CardContent>
    </Card>
  </div>
</template>
