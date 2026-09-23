<script setup lang="ts">
import { computed, onMounted, watch } from 'vue'

import EventsView from '@/components/EventsView.vue'
import TimeSeriesChart from '@/components/TimeSeriesChart.vue'
import type { TimeSeriesDataset } from '@/components/TimeSeriesChart.vue'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { formatBucketLabel } from '@/lib/format'
import { formatErrorRatePercent } from '@/lib/provider-rate'
import { useDashboardStore } from '@/stores/dashboard'
import { useEventsStore } from '@/stores/events'
import { useFiltersStore } from '@/stores/filters'
import { useNavStore } from '@/stores/nav'
import { seriesColor, useReliabilityStore } from '@/stores/reliability'
import type { CountSeries, ProviderRatioSeries } from '@/stores/reliability'

/**
 * ReliabilityPage (redesign-plan.md section 3.4) — error-rate/failover/
 * timeout/429/402 series, plus the pre-redesign Events view reused as a
 * section. Every chart is an on-demand fetch (useReliabilityStore.fetch),
 * not part of the 5s dashboard poll — refetched on mount and whenever the
 * global range changes (Q12 DECISIONS).
 *
 * Page params (redesign-plan.md section 3.1): `provider` narrows every
 * per-provider chart to one provider's own line ('' / absent means every
 * configured provider, overlaid); `kind`/`user` seed the embedded Events
 * section's own filters (stores/events.ts's initFromParams). All three
 * round-trip through the URL hash the same way EntityLink.vue's params do
 * — nav.goTo on every local change, read back via nav.params on mount.
 */
/** '' is the "every provider" state (selectedProvider/filterSelected below). Radix/reka-ui Select items cannot use an empty-string value (EventsView.vue's own identical ALL_KINDS doc comment) — ALL_PROVIDERS_OPTION is the Select-only sentinel, mapped to/from '' at the template boundary. */
const ALL_PROVIDERS_OPTION = 'all'

const dashboard = useDashboardStore()
const events = useEventsStore()
const filters = useFiltersStore()
const nav = useNavStore()
const reliability = useReliabilityStore()

const selectedProvider = computed<string>(() => nav.params.provider ?? '')
const providerSelectValue = computed<string>(() => selectedProvider.value || ALL_PROVIDERS_OPTION)

function onProviderChange(value: unknown): void {
  if (typeof value !== 'string') return
  nav.goTo('reliability', { ...nav.params, provider: value === ALL_PROVIDERS_OPTION ? '' : value })
}

/** providerLabel strips the "provider:" scope prefix every reliability series carries — the display label is just the provider name. */
function providerLabel(scope: string): string {
  return scope.replace(/^provider:/, '')
}

function filterSelected<T extends { scope: string }>(series: T[]): T[] {
  if (!selectedProvider.value) return series
  return series.filter((s) => providerLabel(s.scope) === selectedProvider.value)
}

/**
 * NaN, not 0, marks a bucket with no attempts (errorRateSeries' own "null
 * means no traffic" convention) — chart.js draws a gap at a NaN point
 * rather than a misleading flat 0%, and NaN is still a plain `number`, so
 * TimeSeriesDataset's `data: number[]` needs no cast here.
 */
function toChartPoints(points: (number | null)[]): number[] {
  return points.map((p) => (p === null ? Number.NaN : p))
}

const errorRateDatasets = computed<TimeSeriesDataset[]>(() =>
  filterSelected<ProviderRatioSeries>(reliability.errorRate).map((s, i) => ({
    label: providerLabel(s.scope),
    data: toChartPoints(s.points),
    color: seriesColor(i),
  })),
)

function countDatasets(series: CountSeries[]): TimeSeriesDataset[] {
  return filterSelected(series).map((s, i) => ({ label: providerLabel(s.scope), data: s.points, color: seriesColor(i) }))
}

const timeoutDatasets = computed(() => countDatasets(reliability.timeouts))
const failoverDatasets = computed(() => countDatasets(reliability.failovers))

const rejectionDatasets = computed<TimeSeriesDataset[]>(() => [
  { label: '429 rejections', data: reliability.rejections, color: '--destructive' },
])
const unpricedDatasets = computed<TimeSeriesDataset[]>(() => [
  { label: '402 unpriced refusals', data: reliability.unpriced, color: '--status-warn' },
])

const bucketLabels = computed<string[]>(() => reliability.buckets.map((b) => formatBucketLabel(b, reliability.windowUsed)))

/**
 * errorRatePercent adapts TimeSeriesChart's plain `number` valueFormatter
 * contract (NaN marks a no-attempts bucket, toChartPoints' own doc
 * comment) to lib/provider-rate.ts's shared formatErrorRatePercent, which
 * takes `null` for the identical "nothing to report" case — the actual
 * ceiling-percent/"no data" wording lives in exactly one place, shared
 * with lib/model-table-columns.ts's per-model error-rate column.
 */
function errorRatePercent(value: number): string {
  return formatErrorRatePercent(Number.isNaN(value) ? null : value)
}

const hasProviders = computed(() => (dashboard.overview?.providers.length ?? 0) > 0)
const noFailoverConfigured = computed(() => dashboard.overview !== null && dashboard.overview.features.failover === false)

// --- selected-provider performance (N3 fix, redesign-plan.md section
// 3.3: "performance series=1 for the selected provider") ---
const latencyStatsEnabled = computed(() => dashboard.overview?.features.latencyStats ?? false)
/** providerPerfAvailable gates the whole selected-provider chart section — both conditions the task names ("when a provider is selected and latency stats are enabled"), mirroring reliability.fetchProviderPerf's own short-circuit so the fetch and the render never disagree about when there is data to show. */
const providerPerfAvailable = computed(() => selectedProvider.value !== '' && latencyStatsEnabled.value)

const providerPerfBucketLabels = computed<string[]>(() =>
  reliability.providerPerfBuckets.map((b) => formatBucketLabel(b, reliability.providerPerfWindowUsed)),
)
/** p50/p95 read as NaN (a chart.js gap, toChartPoints' own convention above) for a bucket with no observations — AdminPerfRow's own doc comment: undefined never means 0ms. */
const providerLatencyDatasets = computed<TimeSeriesDataset[]>(() => [
  { label: 'p50', data: reliability.providerPerfPoints.map((p) => p.p50Ms ?? Number.NaN), color: '--chart-requests' },
  { label: 'p95', data: reliability.providerPerfPoints.map((p) => p.p95Ms ?? Number.NaN), color: '--chart-tokens-in' },
])
const providerFailureDatasets = computed<TimeSeriesDataset[]>(() => [
  { label: 'failures', data: reliability.providerPerfPoints.map((p) => p.failures), color: '--destructive' },
])

watch(
  () => [selectedProvider.value, filters.range] as const,
  ([provider]) => void reliability.fetchProviderPerf(provider),
  { immediate: true },
)

/**
 * monthRangeClamped is true whenever the global range is a month preset
 * (3mo/6mo/12mo) — stores/reliability.ts's own hourOrDayWindow has NO
 * month bucket at all for any reliability metric (its own doc comment:
 * "neither family has a month bucket at all, matching their own 48h/35d
 * TTLs"), so it substitutes a fixed 30-day day-window instead. Every
 * chart below silently shows that 30-day window regardless of which
 * month preset is selected — this note (P3 item 20) says so, rather than
 * letting a reader believe a "12mo" label over a chart that is actually
 * showing the most recent 30 days.
 */
const monthRangeClamped = computed(() => filters.window === 'month')

// Refetch whenever the global range changes (Q12 DECISIONS: "new endpoints
// on demand or 60s" — this is the "on demand" case, driven by the reader's
// own range choice rather than a timer) or once the dashboard's 5s poll
// first discovers which providers are configured (a page that mounts
// before the first overview poll lands would otherwise see hasProviders
// flip true with no series fetched yet to back it).
watch(
  () => [filters.range, dashboard.overview?.providers.length ?? 0],
  () => void reliability.fetch(),
)

// kind/user round-trip back into the URL hash (redesign-plan.md section
// 3.1's Reliability page params) whenever the reader changes the embedded
// Events section's own filters — mirrors `provider`'s own onProviderChange
// above, one nav.goTo call merging every current page param so neither
// write clobbers the other.
watch(
  () => [events.kindFilter, events.userFilter],
  ([kind, user]) => {
    nav.goTo('reliability', { ...nav.params, kind, user })
  },
)

// nav.params.kind/user react to a hash change this page's own Select/
// SearchInput did NOT originate — browser back/forward, a hand-edited URL,
// or returning here via the sidebar with the param cleared (P2 item 10:
// stores/events.ts's own initFromParams applies a param only when it is
// PRESENT, by design, for a one-shot seed — but called only on mount, that
// left a kind/user filter set on an earlier visit stuck in place when the
// reader returned with an empty hash). `immediate: true` covers the
// initial mount too, so this replaces that one-shot call outright.
// Undefined always resolves to '' ("no filter"), and this never loops
// against the watch above: assigning a store field its OWN current value
// is a same-value write, which Vue's reactivity does not re-trigger a
// dependent watch for.
watch(
  () => [nav.params.kind, nav.params.user] as const,
  ([kind, user]) => {
    events.setKindFilter(kind ?? '')
    events.setUserFilter(user ?? '')
  },
  { immediate: true },
)

onMounted(() => void reliability.fetch())
</script>

<template>
  <div class="flex flex-col gap-6">
    <Card>
      <CardHeader class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <CardTitle>Reliability</CardTitle>
          <CardDescription>Provider error rate, failovers, timeouts, rate-limit rejections, and 402 refusals.</CardDescription>
        </div>
        <div class="flex flex-col gap-1">
          <label id="reliability-provider-label" class="text-xs font-medium text-muted-foreground">Provider</label>
          <Select :model-value="providerSelectValue" @update:model-value="onProviderChange">
            <SelectTrigger aria-labelledby="reliability-provider-label" class="w-48">
              <SelectValue placeholder="All providers" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem :value="ALL_PROVIDERS_OPTION">All providers</SelectItem>
              <SelectItem v-for="p in dashboard.overview?.providers ?? []" :key="p.name" :value="p.name">{{ p.name }}</SelectItem>
            </SelectContent>
          </Select>
        </div>
      </CardHeader>
      <CardContent>
        <p v-if="reliability.error" class="text-sm text-destructive">{{ reliability.error }}</p>
        <p v-else-if="!hasProviders" class="text-sm text-muted-foreground">No providers configured.</p>
      </CardContent>
    </Card>

    <Alert v-if="monthRangeClamped" variant="warn">
      <AlertDescription>
        Reliability data has no monthly retention — every chart below shows the most recent 30 days, not the full
        {{ filters.range }} range selected above.
      </AlertDescription>
    </Alert>

    <Card v-if="hasProviders">
      <CardHeader>
        <CardTitle>Error rate by provider</CardTitle>
        <CardDescription>Failed attempts / total attempts per bucket. A gap means no attempts in that bucket.</CardDescription>
      </CardHeader>
      <CardContent>
        <TimeSeriesChart :labels="bucketLabels" :datasets="errorRateDatasets" :value-formatter="errorRatePercent" ariaLabel="Error rate by provider over time" />
      </CardContent>
    </Card>

    <Card v-if="hasProviders">
      <CardHeader>
        <CardTitle>Selected provider performance</CardTitle>
        <CardDescription>Latency (p50/p95) and failures per bucket for one provider.</CardDescription>
      </CardHeader>
      <CardContent class="flex flex-col gap-4">
        <p v-if="selectedProvider === ''" class="text-sm text-muted-foreground">
          Select a provider above to see its latency and failure breakdown per bucket.
        </p>
        <p v-else-if="!latencyStatsEnabled" class="text-sm text-muted-foreground">
          Enable <code class="font-mono text-xs">admin.stats.latency</code> in the plugin config to see per-bucket latency for
          {{ selectedProvider }}.
        </p>
        <template v-else-if="providerPerfAvailable">
          <p v-if="reliability.providerPerfError" class="text-sm text-destructive">{{ reliability.providerPerfError }}</p>
          <div class="grid gap-6 lg:grid-cols-2">
            <div>
              <p class="mb-2 text-sm font-medium text-muted-foreground">Latency (ms)</p>
              <TimeSeriesChart
                :labels="providerPerfBucketLabels"
                :datasets="providerLatencyDatasets"
                :ariaLabel="`p50/p95 latency for ${selectedProvider} over time`"
              />
            </div>
            <div>
              <p class="mb-2 text-sm font-medium text-muted-foreground">Failures</p>
              <TimeSeriesChart
                :labels="providerPerfBucketLabels"
                :datasets="providerFailureDatasets"
                :ariaLabel="`Failures for ${selectedProvider} over time`"
              />
            </div>
          </div>
        </template>
      </CardContent>
    </Card>

    <Card v-if="hasProviders">
      <CardHeader>
        <CardTitle>Timeouts by provider</CardTitle>
      </CardHeader>
      <CardContent>
        <TimeSeriesChart :labels="bucketLabels" :datasets="timeoutDatasets" ariaLabel="Timeouts by provider over time" />
      </CardContent>
    </Card>

    <Card v-if="hasProviders">
      <CardHeader>
        <CardTitle>Failovers by provider</CardTitle>
        <CardDescription v-if="noFailoverConfigured">This deployment has no failover chains configured.</CardDescription>
      </CardHeader>
      <CardContent>
        <TimeSeriesChart :labels="bucketLabels" :datasets="failoverDatasets" ariaLabel="Failovers by provider over time" />
      </CardContent>
    </Card>

    <div class="grid gap-6 lg:grid-cols-2">
      <Card>
        <CardHeader>
          <CardTitle>Rate-limit rejections (429)</CardTitle>
          <CardDescription>Fleet-wide, every scope combined.</CardDescription>
        </CardHeader>
        <CardContent>
          <TimeSeriesChart :labels="bucketLabels" :datasets="rejectionDatasets" ariaLabel="Rate-limit rejections over time, fleet-wide" />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Unpriced refusals (402)</CardTitle>
          <CardDescription>Fleet-wide requests refused for having no resolvable price.</CardDescription>
        </CardHeader>
        <CardContent>
          <TimeSeriesChart :labels="bucketLabels" :datasets="unpricedDatasets" ariaLabel="Unpriced refusals (402) over time, fleet-wide" />
        </CardContent>
      </Card>
    </div>

    <EventsView />
  </div>
</template>
