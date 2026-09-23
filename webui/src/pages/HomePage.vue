<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'

import KpiTile from '@/components/KpiTile.vue'
import TopList from '@/components/TopList.vue'
import type { TopListItem } from '@/components/TopList.vue'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { adminFetch } from '@/lib/api'
import { EVENT_KIND_LABEL, eventKindVariant } from '@/lib/events-filter'
import { formatCompactCount, formatCost, formatTimestamp } from '@/lib/format'
import { headroom, monthProgress, projectMonthEnd } from '@/lib/forecast'
import {
  budgetRatio,
  budgetTier,
  errorRateTier,
  fleetErrorRate,
  openBreakerCount,
  sumGroupBudgetMicros,
} from '@/lib/kpi'
import { createVisibilityPoller } from '@/lib/polling'
import type { VisibilityPoller } from '@/lib/polling'
import { totalsUrl } from '@/lib/range'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { useEventsStore } from '@/stores/events'
import { useFiltersStore } from '@/stores/filters'
import type { AdminEventKind, AdminTotalsResponse, AdminUsageModelsResponse } from '@/types/api'

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
  headroom(projectedMicros.value, budgetMicros.value > 0 ? budgetMicros.value / 1_000_000 : undefined),
)
const projectedValue = computed<string>(() =>
  projectedMicros.value === null ? 'not enough data yet' : formatCost(projectedMicros.value),
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
const errorValue = computed<string>(() => {
  const rate = errorRate.value.rate
  if (rate === null) return 'no traffic'
  if (rate <= 0) return '0%'
  // Ceiling, not the usual floor: a genuinely non-zero error rate must
  // never round down to a misleadingly clean "0%" (the opposite honesty
  // direction from a SUCCESS rate, which floors so it never overstates
  // — see lib/provider-rate.ts's own formatRatePercent doc comment).
  return `${Math.ceil(rate * 100)}%`
})
const errorDelta = computed<string>(() => `${errorRate.value.failures}/${errorRate.value.attempts} attempts in the last minute`)

// --- Open breakers / warnings / unpriced ---------------------------------

const breakerCount = computed<number>(() => openBreakerCount(dashboard.overview?.providers ?? []))
const warningCount = computed<number>(() => dashboard.overview?.warnings.length ?? 0)
const unpricedCount = computed<number>(() => dashboard.overview?.pricing.unpriced ?? 0)

// --- Top 5 users by cost / top 5 models by cost --------------------------

const topUsers = ref<TopListItem[]>([])
const topModels = ref<TopListItem[]>([])
const topListsError = ref('')
/** topListsReqId guards against an out-of-order response overwriting a newer one — the 60s visibility-poller tick and a range change fire fetchTopLists() independently, so two calls can be in flight together (P2 item 7). */
let topListsReqId = 0

async function fetchTopLists(): Promise<void> {
  if (!auth.isAuthenticated) return
  const requestId = ++topListsReqId
  try {
    const [usersRes, modelsRes] = await Promise.all([
      adminFetch<AdminTotalsResponse>(
        totalsUrl({ kind: 'user', window: filters.window, span: filters.span, metrics: ['cost'], limit: TOP_LIST_LIMIT }),
      ),
      adminFetch<AdminUsageModelsResponse>(
        `/admin/api/usage/models?metric=cost&window=${filters.window}&span=${filters.span}&limit=${TOP_LIST_LIMIT}`,
      ),
    ])
    if (requestId !== topListsReqId) return
    topUsers.value = usersRes.rows.map((row) => ({
      id: row.id,
      label: row.id,
      value: formatCost(row.values.cost ?? 0),
      kind: 'user' as const,
    }))
    topModels.value = modelsRes.models.map((m) => ({
      id: m.id,
      label: m.id,
      value: formatCost(m.value),
      kind: 'model' as const,
    }))
    topListsError.value = ''
  } catch (err) {
    if (requestId !== topListsReqId) return
    topListsError.value = err instanceof Error ? err.message : String(err)
  }
}

let topListsPoller: VisibilityPoller | undefined
watch(
  () => filters.range,
  () => void fetchTopLists(),
)

// --- Latest 8 events -------------------------------------------------------

/** kindLabel falls back to the raw kind string for a kind EVENT_KIND_LABEL does not know (a server newer than this webui build) — same forward-compat convention lib/events-columns.ts's own identical helper documents. */
function kindLabel(kind: string): string {
  return EVENT_KIND_LABEL[kind as AdminEventKind] ?? kind
}

const latestEvents = computed(() => events.events.slice(0, 8))

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
    </section>

    <p v-if="topListsError" class="text-sm text-destructive">Top lists failed to load: {{ topListsError }}</p>

    <section aria-label="Top spenders and models" class="grid grid-cols-1 gap-3 lg:grid-cols-2">
      <TopList title="Top 5 users by cost" :items="topUsers" empty-text="No spend in this range yet." />
      <TopList title="Top 5 models by cost" :items="topModels" empty-text="No spend in this range yet." />
    </section>

    <Card>
      <CardHeader>
        <CardTitle>Latest events</CardTitle>
      </CardHeader>
      <CardContent>
        <p v-if="latestEvents.length === 0" class="text-sm text-muted-foreground">No events recorded yet.</p>
        <ol v-else class="flex flex-col gap-2">
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
      </CardContent>
    </Card>
  </div>
</template>
