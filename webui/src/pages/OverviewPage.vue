<script setup lang="ts">
import { Button, Card, CardAction, CardContent, CardHeader, CardTitle, KpiTile, LineChart, StatGroup } from '@perfect-design/ui'
import { computed, onMounted, onUnmounted, watch } from 'vue'

import { useEntityLinks } from '@/composables/useEntityLinks'
import { deriveAttention } from '@/lib/attention'
import { formatBucketLabel, formatCompactCount, formatCostDisplay } from '@/lib/format'
import { errorRateTier, fleetErrorRate } from '@/lib/kpi'
import { loadState } from '@/lib/load-state'
import { countThrottled, deltaOf, rangeMs, sumPoints, trafficSeries } from '@/lib/overview-metrics'
import { formatErrorRatePercent } from '@/lib/provider-rate'
import { TOP_USERS_RANK_LIMIT, rankTopUsers } from '@/lib/top-users'
import { wbColumns } from '@/lib/wb-columns'
import { useDashboardStore } from '@/stores/dashboard'
import { useEventsStore } from '@/stores/events'
import { useFiltersStore } from '@/stores/filters'
import { useNavStore } from '@/stores/nav'
import { useOverviewStore } from '@/stores/overview'
import HealthStrip from '@/widgets/HealthStrip.vue'
import ModelName from '@/widgets/ModelName.vue'
import NeedsAttention from '@/widgets/NeedsAttention.vue'
import RecentEvents from '@/widgets/RecentEvents.vue'
import StateView from '@/widgets/StateView.vue'
import type { ViewState } from '@/widgets/StateView.vue'
import WbTable from '@/widgets/WbTable.vue'

const EVENT_ROWS = 6
const MIN_SPARK_POINTS = 2

interface RankRow {
  id: string
  name: string
  value: number
  share: number | null
}

const dashboard = useDashboardStore()
const events = useEventsStore()
const filters = useFiltersStore()
const nav = useNavStore()
const data = useOverviewStore()
const { openEntity } = useEntityLinks()

const overview = computed(() => dashboard.overview)
const pageState = computed<ViewState>(() => {
  const s = loadState({ loading: true, hasData: dashboard.overview !== null && dashboard.usage !== null, error: dashboard.error })
  return s === 'skeleton' ? 'loading' : s === 'ready' ? 'ready' : 'error'
})

function sectionState(loaded: boolean, error: string, empty: boolean): ViewState {
  if (loaded) return empty ? 'empty' : 'ready'
  return error ? 'error' : 'loading'
}

// --- Health + attention ---------------------------------------------------

const eventsSource = computed(() => (events.lastUpdated ? events.source : null))
const attention = computed(() =>
  deriveAttention(
    overview.value,
    dashboard.usage,
    events.lastUpdated ? { source: events.source, events: events.events } : null,
    overview.value?.routers ?? [],
  ),
)

// --- KPI tiles --------------------------------------------------------------

const trafficPending = computed(() => !data.trafficLoaded && !data.error.traffic)
const rangeLabel = computed(() => `last ${filters.range}`)

function kpi(points: number[], previous: number[] | undefined) {
  const total = data.trafficLoaded ? sumPoints(points) : null
  const delta = total !== null && previous ? deltaOf(total, sumPoints(previous)) : null
  return { total, delta, spark: points.length >= MIN_SPARK_POINTS && points.some((v) => v > 0) ? points : undefined }
}

const requests = computed(() => kpi(data.totals.req, data.previous?.req))
const tokensIn = computed(() => kpi(data.totals.tokin, data.previous?.tokin))
const spend = computed(() => kpi(data.totals.cost, data.previous?.cost))
const tokensOut = computed(() => (data.trafficLoaded ? sumPoints(data.totals.tokout) : null))
const monthSpend = computed(() => dashboard.usage?.total.costPerMonthMicroUsd ?? null)

const errorRate = computed(() => fleetErrorRate(overview.value?.providers ?? []))
const errorTone = computed(() => ({ ok: undefined, warn: 'warning', critical: 'danger' }) as const)
const throttled = computed(() => countThrottled(events.events, Date.now(), rangeMs(filters.window, filters.span)))
const throttledTotal = computed(() => (events.lastUpdated ? throttled.value.rateLimited + throttled.value.paced : null))

const count = (n: number | null) => (n === null ? null : formatCompactCount(n))

// --- Traffic chart ----------------------------------------------------------

const lines = computed(() => trafficSeries(data.topModelSeries, data.totals.req, filters.cmpSpec.requested ? (data.previous?.req ?? null) : null))
const labels = computed(() => data.buckets.map((b) => formatBucketLabel(b, filters.window)))
const trafficState = computed(() => sectionState(data.trafficLoaded, data.error.traffic, lines.value.length === 0))

// --- Top lists --------------------------------------------------------------

const userColumns = wbColumns<RankRow>([
  { key: 'name', header: 'User', kind: 'text', sortable: false },
  { key: 'value', header: 'Requests', kind: 'number', sortable: false },
  { key: 'share', header: 'Share', kind: 'share', sortable: false },
])
const modelColumns = wbColumns<RankRow>([
  { key: 'name', header: 'Model', kind: 'text', sortable: false },
  { key: 'value', header: 'Requests', kind: 'number', sortable: false },
  { key: 'share', header: 'Share', kind: 'share', sortable: false },
])

function share(value: number, total: number): number | null {
  return total > 0 ? Math.min(1, value / total) : null
}

const topUsers = computed<RankRow[]>(() => {
  const total = sumPoints(data.totals.req)
  return rankTopUsers(data.topUserRows, 'req', TOP_USERS_RANK_LIMIT).map((r) => ({ id: r.id, name: r.id, value: r.value, share: share(r.value, total) }))
})
const topModels = computed<RankRow[]>(() => {
  const total = sumPoints(data.totals.req)
  return data.topModels.filter((m) => m.value > 0).map((m) => ({ id: m.id, name: m.id, value: m.value, share: share(m.value, total) }))
})
const usersState = computed(() => sectionState(data.usersLoaded, data.error.users, topUsers.value.length === 0))
const modelsState = computed(() => sectionState(data.trafficLoaded, data.error.traffic, topModels.value.length === 0))

// --- Recent events ----------------------------------------------------------

const recent = computed(() => events.events.slice(0, EVENT_ROWS))
const eventsState = computed<ViewState>(() => {
  const s = loadState({ loading: events.refreshing, hasData: events.lastUpdated !== null, error: events.error })
  if (s === 'ready') return recent.value.length === 0 ? 'empty' : 'ready'
  return s === 'skeleton' ? 'loading' : s === 'error' ? 'error' : 'empty'
})

// --- Lifecycle --------------------------------------------------------------

watch(
  () => [filters.range, filters.cmp],
  () => void data.refresh(),
)
onMounted(() => {
  events.startPolling()
  data.startPolling()
})
onUnmounted(() => {
  events.stopPolling()
  data.stopPolling()
})
</script>

<template>
  <StateView
    :state="pageState"
    :updated-at="dashboard.lastUpdated"
    error-title="Could not load the overview"
    :error-message="dashboard.error"
    @retry="dashboard.refresh()"
  >
    <div v-if="overview" class="flex flex-col gap-4">
      <HealthStrip :providers="overview.providers" :redis="overview.redis" :events-source="eventsSource" />

      <StatGroup :columns="5" gap="sm" aria-label="Key metrics">
        <KpiTile
          label="Requests"
          size="compact"
          :value="count(requests.total)"
          :loading="trafficPending"
          :delta="requests.delta?.text"
          :trend="requests.delta?.trend"
          good-when="none"
          :sparkline="requests.spark"
          :hint="rangeLabel"
        />
        <KpiTile
          label="Input tokens"
          size="compact"
          :value="count(tokensIn.total)"
          :loading="trafficPending"
          :delta="tokensIn.delta?.text"
          :trend="tokensIn.delta?.trend"
          good-when="none"
          :sparkline="tokensIn.spark"
          :hint="tokensOut === null ? rangeLabel : `${formatCompactCount(tokensOut)} output`"
        />
        <KpiTile
          label="Spend"
          size="compact"
          :value="spend.total === null ? null : formatCostDisplay(spend.total)"
          :loading="trafficPending"
          :delta="spend.delta?.text"
          :trend="spend.delta?.trend"
          good-when="down"
          :sparkline="spend.spark"
          :hint="monthSpend === null ? rangeLabel : `${formatCostDisplay(monthSpend)} this month`"
        />
        <KpiTile
          label="Error rate"
          size="compact"
          :value="formatErrorRatePercent(errorRate.rate, 'no traffic')"
          :tone="errorTone[errorRateTier(errorRate.rate)]"
          good-when="down"
          :hint="`${errorRate.failures}/${errorRate.attempts} attempts, last minute`"
        />
        <KpiTile
          label="Throttled"
          size="compact"
          :value="count(throttledTotal)"
          :loading="events.lastUpdated === null && !events.error"
          good-when="down"
          :hint="`${throttled.rateLimited} rate limited · ${throttled.paced} paced, recent events`"
        />
      </StatGroup>

      <div class="grid grid-cols-1 gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,22rem)]">
        <Card size="sm">
          <CardHeader><CardTitle>Traffic by model</CardTitle></CardHeader>
          <CardContent>
            <StateView
              :state="trafficState"
              empty-title="No traffic in this range"
              :error-message="data.error.traffic"
              @retry="data.refresh()"
            >
              <div data-wb-ignore-numeric>
                <LineChart
                  :labels="labels"
                  :series="lines"
                  variant="area"
                  stacked
                  :height="240"
                  :value-formatter="formatCompactCount"
                  label="Requests per bucket by model"
                />
              </div>
            </StateView>
          </CardContent>
        </Card>
        <Card size="sm">
          <CardHeader><CardTitle>Needs attention</CardTitle></CardHeader>
          <CardContent><NeedsAttention :items="attention" /></CardContent>
        </Card>
      </div>

      <div class="grid grid-cols-1 items-start gap-4 lg:grid-cols-3">
        <Card size="sm">
          <CardHeader class="items-center">
            <CardTitle>Top users</CardTitle>
            <CardAction class="self-center"><Button variant="ghost" size="sm" @click="nav.goTo('users', {}, { replace: false })">View all</Button></CardAction>
          </CardHeader>
          <CardContent>
            <StateView
              :state="usersState"
              empty-title="No user traffic in this range"
              :error-message="data.error.users"
              :retryable="data.usersLoaded || data.error.users !== ''"
              @retry="data.refresh()"
            >
              <WbTable
                class="wb-hide-share-sm wb-clip"
                :columns="userColumns"
                :rows="topUsers"
                row-key="id"
                label="Top users by requests"
                @row-activate="(row: RankRow) => openEntity('user', row.id)"
              >
                <template #cell-value="{ value }">{{ formatCompactCount(value as number) }}</template>
              </WbTable>
            </StateView>
          </CardContent>
        </Card>
        <Card size="sm">
          <CardHeader class="items-center">
            <CardTitle>Top models</CardTitle>
            <CardAction class="self-center"><Button variant="ghost" size="sm" @click="nav.goTo('models', {}, { replace: false })">View all</Button></CardAction>
          </CardHeader>
          <CardContent>
            <StateView
              :state="modelsState"
              empty-title="No model traffic in this range"
              :error-message="data.error.traffic"
              @retry="data.refresh()"
            >
              <WbTable
                class="wb-hide-share-sm wb-clip"
                :columns="modelColumns"
                :rows="topModels"
                row-key="id"
                label="Top models by requests"
                @row-activate="(row: RankRow) => openEntity('model', row.id)"
              >
                <template #cell-name="{ value }"><ModelName :id="value as string" /></template>
                <template #cell-value="{ value }">{{ formatCompactCount(value as number) }}</template>
              </WbTable>
            </StateView>
          </CardContent>
        </Card>
        <Card size="sm">
          <CardHeader class="items-center">
            <CardTitle>Recent events</CardTitle>
            <CardAction class="self-center"><Button variant="ghost" size="sm" @click="nav.goTo('events', {}, { replace: false })">View all</Button></CardAction>
          </CardHeader>
          <CardContent>
            <StateView :state="eventsState" empty-title="No events recorded" :error-message="events.error" @retry="events.refresh()">
              <RecentEvents :events="recent" />
            </StateView>
          </CardContent>
        </Card>
      </div>
    </div>
  </StateView>
</template>
