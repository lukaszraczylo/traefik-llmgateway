<script setup lang="ts">
import { computed, onMounted, onUnmounted, watch } from 'vue'

import AttributionDrilldown from '@/components/AttributionDrilldown.vue'
import BurnDownChart from '@/components/BurnDownChart.vue'
import CostAvoidedCard from '@/components/CostAvoidedCard.vue'
import CostForecastCard from '@/components/CostForecastCard.vue'
import PricingHealthTable from '@/components/PricingHealthTable.vue'
import TimeSeriesChart from '@/components/TimeSeriesChart.vue'
import type { TimeSeriesComparisonDataset, TimeSeriesDataset } from '@/components/TimeSeriesChart.vue'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useNow } from '@/composables/useNow'
import { cacheSavingsStatus } from '@/lib/cache-savings'
import { formatBucketLabel, formatCompactCount, formatCost } from '@/lib/format'
import { useCatalogStore } from '@/stores/catalog'
import { useDashboardStore } from '@/stores/dashboard'
import { useFiltersStore } from '@/stores/filters'
import { useNavStore } from '@/stores/nav'
import {
  isSpendBy,
  isSpendMetric,
  SPEND_BY,
  SPEND_METRIC_LABEL,
  SPEND_METRICS,
  useSpendStore,
} from '@/stores/spend'
import type { SpendBy } from '@/stores/spend'
import type { HistoryMetric, LimitsConfig } from '@/types/api'

const SPEND_BY_LABEL: Record<SpendBy, string> = { model: 'By model', provider: 'By provider', group: 'By group' }

const nav = useNavStore()
const filters = useFiltersStore()
const dashboard = useDashboardStore()
const catalog = useCatalogStore()
const spend = useSpendStore()
const { now, start: startClock, stop: stopClock } = useNow()

// --- page params (redesign-plan.md section 3.1: by/metric/ref/drill) ---

const by = computed<SpendBy>(() => (nav.params.by && isSpendBy(nav.params.by) ? nav.params.by : 'model'))
const metric = computed<HistoryMetric>(() => (nav.params.metric && isSpendMetric(nav.params.metric) ? nav.params.metric : 'cost'))
const refParam = computed<string>(() => nav.params.ref ?? '')
const drillParam = computed<string>(() => nav.params.drill ?? '')

/** updateParams merges one changed field over the current by/metric/ref/drill and writes the whole set back via nav.goTo (which REPLACES, never merges, stores/nav.ts's own doc comment) — a key is included only when it differs from its default, keeping the hash as clean as every other page's own param convention. */
function updateParams(next: Partial<{ by: SpendBy; metric: HistoryMetric; ref: string; drill: string }>): void {
  const merged = {
    by: next.by ?? by.value,
    metric: next.metric ?? metric.value,
    ref: next.ref ?? refParam.value,
    drill: next.drill ?? drillParam.value,
  }
  const params: Record<string, string> = {}
  if (merged.by !== 'model') params.by = merged.by
  if (merged.metric !== 'cost') params.metric = merged.metric
  if (merged.ref !== '') params.ref = merged.ref
  if (merged.drill !== '') params.drill = merged.drill
  nav.goTo('spend', params)
}

function onByChange(value: string | number): void {
  if (typeof value === 'string' && isSpendBy(value)) updateParams({ by: value })
}
function onMetricUpdate(value: unknown): void {
  if (typeof value === 'string' && isSpendMetric(value)) updateParams({ metric: value })
}
function onRefUpdate(value: string): void {
  updateParams({ ref: value })
}
function onDrillUpdate(value: string): void {
  updateParams({ drill: value })
}

// The breakdown-dimension selector has nothing left to choose once the
// global scope filter has already narrowed to one group/user — the chart
// then shows exactly that one entity's own line regardless of `by` (see
// stores/spend.ts's own doc comment on scope narrowing).
const byDisabled = computed<boolean>(() => filters.scope !== 'all')

// --- sync page params + global filters into the store ---

watch([by, metric, drillParam], ([b, m, d]) => {
  spend.setSelection({ by: b, metric: m, drill: d })
})
watch(
  () => [filters.range, filters.cmp, filters.scope],
  () => void spend.refresh(),
)

onMounted(() => {
  spend.$patch({ by: by.value, metric: metric.value, drill: drillParam.value })
  void catalog.ensureLoaded()
  spend.startPolling() // ticks once immediately — the page's initial fetch.
  startClock()
})
onUnmounted(() => {
  spend.stopPolling()
  stopClock()
})

// --- breakdown chart ---

/**
 * BREAKDOWN_PALETTE cycles a colorblind-safe (Okabe & Ito, 2008) 8-color
 * qualitative set across the breakdown chart's per-model/provider/group
 * series. The first four reuse main.css's own existing --chart-* tokens
 * (already that same palette's requests/tokens-in/tokens-out/cost
 * members); the remaining four (--chart-series-a..d) extend it, defined
 * alongside them in assets/main.css — main.css's original four tokens
 * were sized for four FIXED semantic metrics, not an open-ended
 * breakdown series count, so this page needed four more, but every chart
 * color token lives in the one design-tokens file (Tailwind-only rule),
 * never a component-local <style> block. Beyond 8 series (e.g. a fleet
 * with more than 8 configured groups) colors repeat — an accepted
 * qualitative-palette limit, not a bug: legend labels remain the
 * disambiguator past that point, the same tradeoff any categorical chart
 * palette makes once it runs out of distinguishable hues.
 */
const BREAKDOWN_PALETTE = [
  '--chart-requests',
  '--chart-tokens-in',
  '--chart-tokens-out',
  '--chart-cost',
  '--chart-series-a',
  '--chart-series-b',
  '--chart-series-c',
  '--chart-series-d',
]

function paletteColor(index: number): string {
  return BREAKDOWN_PALETTE[index % BREAKDOWN_PALETTE.length]!
}

/** breakdownLabel strips a series' own scope-kind prefix ("model:"/"group:") for the chart legend — sumByProvider's own output already has no prefix (lib/series.ts). */
function breakdownLabel(scope: string): string {
  if (scope.startsWith('model:')) return scope.slice('model:'.length)
  if (scope.startsWith('group:')) return scope.slice('group:'.length)
  if (scope.startsWith('provider:')) return scope.slice('provider:'.length)
  return scope
}

// The breakdown chart's own window (filters.window) decides the bucket
// label format (lib/format.ts's formatBucketLabel), same as every other
// chart in this panel.
const breakdownLabels = computed<string[]>(() => spend.buckets.map((b) => formatBucketLabel(b, filters.window)))

const breakdownDatasets = computed<TimeSeriesDataset[]>(() => {
  const charted: TimeSeriesDataset[] = spend.breakdown.map((s, i) => ({
    label: breakdownLabel(s.scope),
    data: s.points,
    color: paletteColor(i),
  }))
  if (spend.otherPoints) charted.push({ label: 'Other', data: spend.otherPoints, color: '--muted-foreground' })
  return charted
})

// This IS a real "vs previous period" comparison (Q5) — unlike
// BurnDownChart.vue's own flat budget reference line, which reuses the
// same comparisonDatasets prop but opts OUT of the suffix (P2 item 5).
const comparisonDatasets = computed<TimeSeriesComparisonDataset[]>(() =>
  spend.comparisonPoints ? [{ label: 'Total', data: spend.comparisonPoints, color: '--foreground', labelSuffix: ' (previous period)' }] : [],
)

const valueFormatter = computed<(value: number) => string>(() => (metric.value === 'cost' ? formatCost : formatCompactCount))

const totalInRange = computed<number>(() => spend.totalPoints.reduce((a, b) => a + b, 0))

// --- forecast / burn-down scope derivation (Q2/DECISIONS: fleet budget = sum of group costPerMonthUSD) ---

const scopeMtdMicros = computed<number>(() => {
  if (filters.scope === 'all') return dashboard.usage?.total.costPerMonthMicroUsd ?? 0
  if (filters.scope.startsWith('group:')) {
    const name = filters.scope.slice('group:'.length)
    return dashboard.usage?.groups.find((g) => g.id === name)?.costPerMonthMicroUsd ?? 0
  }
  if (filters.scope.startsWith('user:')) {
    const name = filters.scope.slice('user:'.length)
    return dashboard.usage?.users.find((u) => u.id === name)?.costPerMonthMicroUsd ?? 0
  }
  return 0
})

const scopeLimits = computed<LimitsConfig | undefined>(() => {
  if (filters.scope === 'all') {
    const groups = dashboard.overview?.groups ?? []
    const budgeted = groups.filter((g) => (g.limits?.costPerMonthUSD ?? 0) > 0)
    if (budgeted.length === 0) return undefined
    const totalUsd = budgeted.reduce((sum, g) => sum + (g.limits?.costPerMonthUSD ?? 0), 0)
    return { costPerMonthUSD: totalUsd }
  }
  if (filters.scope.startsWith('group:')) {
    const name = filters.scope.slice('group:'.length)
    return dashboard.overview?.groups.find((g) => g.name === name)?.limits
  }
  if (filters.scope.startsWith('user:')) {
    const name = filters.scope.slice('user:'.length)
    return dashboard.usage?.users.find((u) => u.id === name)?.limits
  }
  return undefined
})

const forecastAvailable = computed<boolean>(() => filters.scope === 'all' || filters.scope.startsWith('group:') || filters.scope.startsWith('user:'))

// --- attribution drilldown gating ---

const userModelStatsEnabled = computed<boolean>(() => dashboard.overview?.features.userModelStats ?? false)
const cacheStatsEnabled = computed<boolean>(() => dashboard.overview?.features.cacheStats ?? false)

/**
 * cacheSavingsStatusResult (N2 fix, verify-redesign-final.md — replaces
 * the P3-item-15 note this superseded, which described an already-fixed
 * backend bug): lib/cache-savings.ts's own doc comment has the full,
 * CURRENT backend-behaviour explanation this page passes down as plain
 * props. `available` gates whether CostAvoidedCard.vue renders a dollar
 * figure at all (false for an hour-window range — the backend never
 * writes a per-hour cache-savings bucket, so there is no number to sum,
 * not a genuine $0) or a caveat-annotated figure (month-window ranges,
 * capped at the day counter's own 35-day retention).
 */
const cacheSavingsStatusResult = computed(() => cacheSavingsStatus(filters.window))
const cacheSavingsAvailable = computed<boolean>(() => cacheSavingsStatusResult.value.available)
const cacheSavingsNote = computed<string | null>(() => cacheSavingsStatusResult.value.note)
</script>

<template>
  <div class="flex flex-col gap-4">
    <div class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
      <div class="flex flex-wrap items-center gap-3">
        <div class="flex flex-col gap-1">
          <span class="text-xs font-medium text-muted-foreground">Breakdown</span>
          <Tabs :model-value="by" @update:model-value="onByChange">
            <TabsList>
              <TabsTrigger v-for="key in SPEND_BY" :key="key" :value="key" :disabled="byDisabled">
                {{ SPEND_BY_LABEL[key] }}
              </TabsTrigger>
            </TabsList>
          </Tabs>
        </div>
        <div class="flex flex-col gap-1">
          <label id="spend-metric-label" class="text-xs font-medium text-muted-foreground">Metric</label>
          <Select :model-value="metric" @update:model-value="onMetricUpdate">
            <SelectTrigger aria-labelledby="spend-metric-label" class="w-40">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem v-for="m in SPEND_METRICS" :key="m" :value="m">{{ SPEND_METRIC_LABEL[m] }}</SelectItem>
            </SelectContent>
          </Select>
        </div>
      </div>
      <div class="text-right">
        <p class="text-xs font-medium text-muted-foreground">{{ SPEND_METRIC_LABEL[metric] }} in range</p>
        <p class="text-2xl font-semibold tabular-nums">{{ valueFormatter(totalInRange) }}</p>
      </div>
    </div>
    <p v-if="byDisabled" class="text-sm text-muted-foreground">
      Showing <span class="font-mono text-xs">{{ filters.scope }}</span> directly — clear the scope filter to break spend down by
      model, provider, or group.
    </p>
    <p v-if="spend.error" class="text-sm text-destructive">{{ spend.error }}</p>
    <!-- P3 item 23: catalog.error was never rendered — PricingHealthTable/
    CostAvoidedCard both depend on catalog.modelsById and silently showed
    empty/"no priced model" with no indication the catalog fetch itself
    had failed. -->
    <p v-if="catalog.error" class="text-sm text-destructive">{{ catalog.error }}</p>
    <!-- P3 item 19 / Q5: a requested-but-unavailable comparison (a
    single-provider scope) used to leave the "vs previous period" toggle
    pressed with no dashed line and no explanation. -->
    <p v-if="filters.cmpSpec.requested && spend.comparisonUnavailableReason" class="text-sm text-muted-foreground">
      {{ spend.comparisonUnavailableReason }}
    </p>

    <TimeSeriesChart
      :labels="breakdownLabels"
      :datasets="breakdownDatasets"
      :comparison-datasets="comparisonDatasets"
      stacked
      :value-formatter="valueFormatter"
      :ariaLabel="`${SPEND_METRIC_LABEL[metric]} breakdown ${SPEND_BY_LABEL[by].toLowerCase()} over time`"
    />

    <div class="grid grid-cols-1 gap-4 lg:grid-cols-2">
      <BurnDownChart
        v-if="spend.burndownAvailable"
        :buckets="spend.burndownBuckets"
        :points="spend.burndownPoints"
        :budget-micros="spend.burndownBudgetMicros"
        :now="now"
      />
      <CostForecastCard v-if="forecastAvailable" :mtd-micros="scopeMtdMicros" :limits="scopeLimits" :now="now" />
    </div>

    <CostAvoidedCard
      :models="spend.modelRanking"
      :catalog="catalog.modelsById"
      :ref-model="refParam"
      :cache-stats-enabled="cacheStatsEnabled"
      :cache-savings-available="cacheSavingsAvailable"
      :cache-savings-note="cacheSavingsNote"
      @update:ref-model="onRefUpdate"
    />

    <PricingHealthTable :models="spend.pricingHealthRanking" :catalog="catalog.modelsById" />

    <AttributionDrilldown
      :rows="spend.drilldownRows"
      :metrics="spend.drilldownMetrics"
      :drill="drillParam"
      :disabled="spend.drilldownDisabled"
      :user-model-stats-enabled="userModelStatsEnabled"
      :window="filters.window"
      :span="filters.span"
      @update:drill="onDrillUpdate"
    />
  </div>
</template>
