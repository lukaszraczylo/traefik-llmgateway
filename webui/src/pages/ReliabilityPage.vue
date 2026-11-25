<script setup lang="ts">
import { Button, Card, CardAction, CardContent, CardHeader, CardTitle, LineChart, NativeSelect, Pagination, SegmentedControl, StatusBadge } from '@perfect-design/ui'
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'

import { useNow } from '@/composables/useNow'
import { thinLabels } from '@/lib/chart-labels'
import { formatBucketLabel, formatCompactCount, formatUntil } from '@/lib/format'
import { loadState } from '@/lib/load-state'
import { pageWindow } from '@/lib/paging'
import { sortThenSlice } from '@/lib/table-sort'
import type { SortSpec } from '@/lib/table-sort'
import { statusRank } from '@/lib/breaker-severity'
import { breakerRows, ratePercentSeries } from '@/lib/provider-rate'
import type { BreakerRow } from '@/lib/provider-rate'
import { scopeId } from '@/lib/scope'
import { wbColumns } from '@/lib/wb-columns'
import { useDashboardStore } from '@/stores/dashboard'
import { useFiltersStore } from '@/stores/filters'
import { useNavStore } from '@/stores/nav'
import { hourOrDayWindow, useReliabilityStore } from '@/stores/reliability'
import type { CountSeries } from '@/stores/reliability'
import StateView from '@/widgets/StateView.vue'
import type { ViewState } from '@/widgets/StateView.vue'
import WbTable from '@/widgets/WbTable.vue'

const SEGMENTED_MAX = 4
const BREAKER_PAGE_SIZE = 10
const BREAKER_PAGE_SIZE_PHONE = 5
const PHONE_QUERY = '(max-width: 639px)'
const MAX_AXIS_LABELS = 6

interface BreakerView extends BreakerRow {
  until: string
  status: string
  statusRank: number
}

const dashboard = useDashboardStore()
const filters = useFiltersStore()
const nav = useNavStore()
const reliability = useReliabilityStore()
const { now, start: startClock, stop: stopClock } = useNow()

const overview = computed(() => dashboard.overview)
const providers = computed(() => overview.value?.providers ?? [])
const latencyEnabled = computed(() => overview.value?.features.latencyStats ?? false)

const clampedNote = computed(() =>
  filters.window === 'month' ? `Reliability data is kept for 30 days. Showing the last 30 days, not ${filters.range}.` : null,
)

// --- Provider picker --------------------------------------------------------

function total(series: CountSeries | undefined): number {
  return series ? series.points.reduce((a, b) => a + b, 0) : 0
}

const defaultProvider = computed<string>(() => {
  const busiest = [...reliability.attempts].sort((a, b) => total(b) - total(a))[0]
  if (busiest && total(busiest) > 0) return scopeId(busiest.scope)
  const open = breakerRows(overview.value).find((r) => r.state !== 'closed')
  return open ? open.provider : (providers.value[0]?.name ?? '')
})
const selected = computed<string>(() => {
  const asked = nav.params.provider
  return asked && providers.value.some((p) => p.name === asked) ? asked : defaultProvider.value
})
const providerOptions = computed(() => providers.value.map((p) => ({ value: p.name, label: p.name })))

function pick(name: string): void {
  nav.goTo('reliability', { ...nav.params, provider: name })
}

// --- Charts -----------------------------------------------------------------

const bucketLabels = computed(() => thinLabels(reliability.buckets.map((b) => formatBucketLabel(b, reliability.windowUsed)), MAX_AXIS_LABELS))
const perfLabels = computed(() =>
  thinLabels(reliability.providerPerfBuckets.map((b) => formatBucketLabel(b, reliability.providerPerfWindowUsed)), MAX_AXIS_LABELS),
)

function pointsFor(series: CountSeries[], provider: string): number[] {
  const hit = series.find((s) => scopeId(s.scope) === provider)
  return hit ? hit.points : reliability.buckets.map(() => 0)
}

const selectedAttempts = computed(() => pointsFor(reliability.attempts, selected.value))
const attemptsTotal = computed(() => selectedAttempts.value.reduce((a, b) => a + b, 0))
const toGaps = (v: (number | null)[]): number[] => v.map((x) => x ?? Number.NaN)
const activityLines = computed(() => {
  const rate = (series: CountSeries[]) => toGaps(ratePercentSeries(pointsFor(series, selected.value), selectedAttempts.value))
  return [
    { name: 'Failures', data: rate(reliability.failures), tone: 'danger' as const },
    { name: 'Timeouts', data: rate(reliability.timeouts), tone: 'warning' as const },
    { name: 'Failovers', data: rate(reliability.failovers), tone: 'info' as const },
  ]
})
const ttfbLines = computed(() => [
  { name: 'TTFB p50', data: reliability.providerPerfPoints.map((p) => p.ttfbP50Ms ?? Number.NaN), tone: 'primary' as const },
  { name: 'TTFB p95', data: reliability.providerPerfPoints.map((p) => p.ttfbP95Ms ?? Number.NaN), tone: 'warning' as const },
])
const rejectionLines = computed(() => [
  { name: '429 rate limited', data: reliability.rejections, tone: 'danger' as const },
  { name: '402 unpriced', data: reliability.unpriced, tone: 'warning' as const },
])

const formatPercent = (n: number): string => `${Number(n.toFixed(2))}%`
const formatMs = (n: number): string => `${Math.round(n).toLocaleString('en-US')} ms`

const seriesState = computed<ViewState>(() => {
  if (providers.value.length === 0 && overview.value !== null) return 'empty'
  const s = loadState({ loading: reliability.loading || reliability.lastUpdated === null, hasData: reliability.lastUpdated !== null, error: reliability.error })
  return s === 'skeleton' ? 'loading' : s === 'error' ? 'error' : 'ready'
})
const activityState = computed<ViewState>(() => (seriesState.value === 'ready' && attemptsTotal.value === 0 ? 'empty' : seriesState.value))
const rejectionState = computed<ViewState>(() =>
  seriesState.value === 'ready' && rejectionLines.value.every((l) => l.data.every((v) => v === 0)) ? 'empty' : seriesState.value,
)
const ttfbState = computed<ViewState>(() => {
  if (!latencyEnabled.value) return 'empty'
  if (reliability.providerPerfError) return 'error'
  if (reliability.providerPerfLoadedFor !== selected.value) return 'loading'
  return ttfbLines.value.every((l) => l.data.every((v) => Number.isNaN(v))) ? 'empty' : 'ready'
})

// --- Breaker table ----------------------------------------------------------

const breakerColumns = wbColumns<BreakerView>([
  { key: 'provider', header: 'Provider', kind: 'text' },
  { key: 'status', header: 'State', kind: 'status', sortable: true },
  { key: 'until', header: 'Opens until', kind: 'time', sortable: false },
  { key: 'lastErr', header: 'Last error', kind: 'text', sortable: false },
])

const STATE_TONE = { closed: 'success', open: 'danger', 'half-open': 'warning', degraded: 'warning' } as const

const breakers = computed<BreakerView[]>(() => {
  void now.value
  return breakerRows(overview.value).map((r) => ({
    ...r,
    until: r.state === 'open' ? formatUntil(r.openUntil) || 'due' : '',
    status: r.degraded ? 'degraded' : r.state,
    statusRank: statusRank(r.degraded ? 'degraded' : r.state),
  }))
})
const phone = ref(false)
let phoneMq: MediaQueryList | undefined
const onPhone = (e: MediaQueryListEvent): void => {
  phone.value = e.matches
}
const breakerPageSize = computed(() => (phone.value ? BREAKER_PAGE_SIZE_PHONE : BREAKER_PAGE_SIZE))
const breakerPage = ref(1)
const breakerWindow = computed(() => pageWindow(breakers.value.length, breakerPageSize.value, breakerPage.value - 1))
const breakerSort = ref<SortSpec | null>(null)
const breakerSortSpec = computed<SortSpec | null>(() => (breakerSort.value?.key === 'status' ? { ...breakerSort.value, key: 'statusRank' } : breakerSort.value))
const breakerPaged = computed(() => sortThenSlice(breakers.value, breakerSortSpec.value, breakerPageSize.value, breakerWindow.value.index))
function onBreakerSort(next: SortSpec | null): void {
  breakerSort.value = next
  breakerPage.value = 1
}
const breakerState = computed<ViewState>(() => (overview.value === null ? 'loading' : breakers.value.length === 0 ? 'empty' : 'ready'))
const openCount = computed(() => breakers.value.filter((r) => r.state !== 'closed').length)
const degradedCount = computed(() => breakers.value.filter((r) => r.degraded).length)
const breakerSummary = computed(() => `${openCount.value} open · ${degradedCount.value} degraded`)

// --- Lifecycle --------------------------------------------------------------

watch(
  () => [filters.range, providers.value.length],
  () => void reliability.fetch(),
)
watch(
  () => [selected.value, filters.range, latencyEnabled.value] as const,
  ([provider]) => void reliability.fetchProviderPerf(provider),
  { immediate: true },
)
onMounted(() => {
  phoneMq = window.matchMedia(PHONE_QUERY)
  phone.value = phoneMq.matches
  phoneMq.addEventListener('change', onPhone)
  startClock()
  void reliability.fetch()
})
onUnmounted(() => {
  phoneMq?.removeEventListener('change', onPhone)
  stopClock()
})

const windowUsed = computed(() => hourOrDayWindow(filters.window, filters.span))
</script>

<template>
  <div class="flex flex-col gap-4">
    <div
      v-if="clampedNote"
      role="note"
      class="border-border bg-card text-muted-foreground rounded-md border px-3 py-2 text-sm"
      data-testid="clamped-range"
    >
      {{ clampedNote }}
    </div>

    <div class="flex flex-wrap items-center justify-between gap-3">
      <div class="flex min-w-0 flex-wrap items-center gap-3">
        <span class="text-muted-foreground text-xs">Provider</span>
        <SegmentedControl
          v-if="providerOptions.length > 0 && providerOptions.length <= SEGMENTED_MAX"
          :model-value="selected"
          :options="providerOptions"
          size="sm"
          aria-label="Provider"
          @update:model-value="pick"
        />
        <NativeSelect
          v-else-if="providerOptions.length > SEGMENTED_MAX"
          class="max-w-56"
          :model-value="selected"
          :options="providerOptions"
          size="sm"
          aria-label="Provider"
          @update:model-value="pick"
        />
      </div>
      <Button variant="outline" size="sm" :disabled="!selected" @click="nav.goTo('events', { provider: selected }, { replace: false })">
        Events for {{ selected || 'provider' }}
      </Button>
    </div>

    <div class="grid grid-cols-1 gap-4 lg:grid-cols-2">
      <Card size="sm">
        <CardHeader class="items-center">
          <CardTitle>Failure, timeout and failover rate</CardTitle>
          <CardAction class="text-muted-foreground self-center text-xs">{{ formatCompactCount(attemptsTotal) }} attempts</CardAction>
        </CardHeader>
        <CardContent>
          <StateView
            :state="activityState"
            empty-title="No attempts in this range"
            :error-message="reliability.error"
            @retry="reliability.fetch()"
          >
            <div data-wb-ignore-numeric>
              <LineChart
                :labels="bucketLabels"
                :series="activityLines"
                :height="220"
                :value-formatter="formatPercent"
                :label="`Failure, timeout and failover rate as percent of attempts for ${selected} per ${windowUsed.window}`"
              />
            </div>
          </StateView>
        </CardContent>
      </Card>

      <Card size="sm">
        <CardHeader><CardTitle>Time to first byte</CardTitle></CardHeader>
        <CardContent>
          <StateView
            :state="ttfbState"
            :empty-title="latencyEnabled ? 'No latency samples in this range' : 'Latency statistics are off'"
            :empty-description="latencyEnabled ? undefined : 'Enable admin.stats.latency to chart time to first byte.'"
            :error-message="reliability.providerPerfError"
            @retry="reliability.fetchProviderPerf(selected)"
          >
            <div data-wb-ignore-numeric data-testid="ttfb-chart">
              <LineChart
                :labels="perfLabels"
                :series="ttfbLines"
                :height="220"
                :value-formatter="formatMs"
                :label="`Time to first byte p50 and p95 for ${selected}`"
              />
            </div>
          </StateView>
        </CardContent>
      </Card>
    </div>

    <div class="grid grid-cols-1 items-start gap-4 lg:grid-cols-2">
      <Card size="sm">
        <CardHeader class="items-center">
          <CardTitle>Circuit breakers</CardTitle>
          <CardAction class="text-muted-foreground self-center text-xs">{{ breakerSummary }}</CardAction>
        </CardHeader>
        <CardContent class="flex flex-col gap-2">
          <StateView :state="breakerState" empty-title="No providers configured" :error-message="dashboard.error" @retry="dashboard.refresh()">
            <WbTable
              class="wb-clip"
              :columns="breakerColumns"
              :rows="breakerPaged"
              :sort="breakerSort"
              row-key="provider"
              label="Provider circuit breakers"
              @update:sort="onBreakerSort"
              @row-activate="(row: BreakerView) => nav.openPanel('provider', row.provider)"
            >
              <template #cell-status="{ value }">
                <StatusBadge :status="value as string" :tones="STATE_TONE" size="sm" />
              </template>
              <template #cell-until="{ value }">{{ value || '—' }}</template>
              <template #cell-lastErr="{ value }">
                <span class="wb-err" :title="(value as string) || undefined">{{ value || '—' }}</span>
              </template>
            </WbTable>
            <div v-if="breakers.length > breakerPageSize" class="flex items-center justify-between gap-2">
              <span class="text-muted-foreground text-xs">{{ breakers.length.toLocaleString('en-US') }} providers</span>
              <Pagination v-model:page="breakerPage" :total="breakers.length" :page-size="breakerPageSize" variant="compact" size="sm" />
            </div>
          </StateView>
        </CardContent>
      </Card>

      <Card size="sm">
        <CardHeader><CardTitle>Rejections · all providers</CardTitle></CardHeader>
        <CardContent>
          <StateView
            :state="rejectionState"
            empty-title="No rejections in this range"
            :error-message="reliability.error"
            @retry="reliability.fetch()"
          >
            <div data-wb-ignore-numeric>
              <LineChart
                :labels="bucketLabels"
                :series="rejectionLines"
                :height="220"
                :value-formatter="formatCompactCount"
                label="Rejections by 429 rate limit and 402 unpriced refusal, fleet-wide"
              />
            </div>
          </StateView>
        </CardContent>
      </Card>
    </div>
  </div>
</template>
