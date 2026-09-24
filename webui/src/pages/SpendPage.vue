<script setup lang="ts">
import { computed, onMounted, onUnmounted, watch } from 'vue'

import AttributionDrilldown from '@/components/AttributionDrilldown.vue'
import BurnDownChart from '@/components/BurnDownChart.vue'
import ChartCard from '@/components/ChartCard.vue'
import CostAvoidedCard from '@/components/CostAvoidedCard.vue'
import CostForecastCard from '@/components/CostForecastCard.vue'
import LabeledSelect from '@/components/LabeledSelect.vue'
import PricingHealthTable from '@/components/PricingHealthTable.vue'
import type { TimeSeriesComparisonDataset, TimeSeriesDataset } from '@/components/TimeSeriesChart.vue'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useNow } from '@/composables/useNow'
import { cacheSavingsStatus } from '@/lib/cache-savings'
import { BREAKDOWN_PALETTE, seriesColor } from '@/lib/chart-palette'
import { formatBucketLabel, formatCompactCount, formatCost } from '@/lib/format'
import { loadState } from '@/lib/load-state'
import { parseScope, scopeBudgetLimits, scopeId } from '@/lib/scope'
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
const metricOptions = SPEND_METRICS.map((m) => ({ value: m, label: SPEND_METRIC_LABEL[m] }))

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

/** breakdownLabel strips a series' own scope-kind prefix ("model:"/"group:"/"provider:") for the chart legend (lib/scope.ts's shared scopeId, reuse-audit.md F5) — sumByProvider's own output already has no prefix (lib/series.ts). */
function breakdownLabel(scope: string): string {
  return scopeId(scope)
}

// The breakdown chart's own window (filters.window) decides the bucket
// label format (lib/format.ts's formatBucketLabel), same as every other
// chart in this panel.
const breakdownLabels = computed<string[]>(() => spend.buckets.map((b) => formatBucketLabel(b, filters.window)))

const breakdownDatasets = computed<TimeSeriesDataset[]>(() => {
  const charted: TimeSeriesDataset[] = spend.breakdown.map((s, i) => ({
    label: breakdownLabel(s.scope),
    data: s.points,
    color: seriesColor(i, BREAKDOWN_PALETTE),
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
  const parsed = parseScope(filters.scope)
  if (!parsed) return dashboard.usage?.total.costPerMonthMicroUsd ?? 0
  if (parsed.kind === 'group') return dashboard.usage?.groups.find((g) => g.id === parsed.id)?.costPerMonthMicroUsd ?? 0
  if (parsed.kind === 'user') return dashboard.usage?.users.find((u) => u.id === parsed.id)?.costPerMonthMicroUsd ?? 0
  return 0
})

/** scopeLimits resolves the current global scope's own cost/month LimitsConfig via lib/scope.ts's shared scopeBudgetLimits (reuse-audit.md F5) — the fleet-wide sum-of-group-budgets 'all' case and the per-group/user lookup both used to be reimplemented here, disagreeing in spots with stores/spend.ts's own third copy. */
const scopeLimits = computed<LimitsConfig | undefined>(() =>
  scopeBudgetLimits(filters.scope, dashboard.overview?.groups ?? [], dashboard.usage?.users ?? []),
)

/** forecastAvailable is true for every scope scopeLimits/scopeMtdMicros can resolve a figure for — 'all', a group, or a user; false only for a provider scope, which has no fleet-relative budget concept. */
const forecastAvailable = computed<boolean>(() => {
  const parsed = parseScope(filters.scope)
  return !parsed || parsed.kind === 'group' || parsed.kind === 'user'
})

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

// --- skeleton/error states (verify-ui-states.md #7: "Spend and Reliability
// pages got no skeletons or ErrorState") -----------------------------------

/** breakdownState gates the main breakdown chart's own skeleton/error — hasData is bucket presence (a real response landed), not "every value is non-zero", so a genuinely zero-traffic range still reads 'ready' and the chart draws its own honest zero line. BurnDownChart.vue computes its own identical state internally now (it owns its own Card/CardHeader, verify-ui-states.md fix) — SpendPage.vue just forwards spend.burndownLoading/spend.burndownError as props. */
const breakdownState = computed(() => loadState({ loading: spend.loading, hasData: spend.buckets.length > 0, error: spend.breakdownError }))

/**
 * retryPricingHealthTable (verify-ui-states-2.md #9 fix) is
 * PricingHealthTable's own Retry action — the table joins
 * spend.pricingHealthRanking with catalog.modelsById, so a failure whose
 * ROOT CAUSE is the catalog fetch (catalog.error, shown alongside
 * spend.pricingHealthError below) never gets fixed by re-running
 * spend.fetchPricingHealthRanking alone. Always re-runs the pricing fetch;
 * additionally re-runs catalog.fetch() when the catalog is the one
 * currently failing.
 */
function retryPricingHealthTable(): Promise<void> {
  const tasks: Promise<void>[] = [spend.fetchPricingHealthRanking()]
  if (catalog.error) tasks.push(catalog.fetch())
  return Promise.all(tasks).then(() => undefined)
}
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
        <LabeledSelect label="Metric" :model-value="metric" :options="metricOptions" trigger-class="w-40" @update:model-value="onMetricUpdate" />
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
    <!-- verify-ui-states-2.md #3: spend.breakdownError still renders via
    the breakdown chart's own ErrorState below when there is no data at all
    (breakdownState === 'error') — never a second, duplicate line for that
    case (the item-8-style duplicate-error bug this fix set also corrected
    for ModelsPage.vue). verify-ui-states-3.md NEW-1 adds the ONE case
    ErrorState can't cover: a background refetch (poll tick, or a
    metric/by/range/scope change) fails while a PREVIOUS successful result
    is still on screen (breakdownState === 'ready', loadState's own 'ready'
    precedence) — the failure was otherwise invisible. See the paragraph
    below the chart. -->
    <!-- P3 item 23: catalog.error was never rendered — PricingHealthTable/
    CostAvoidedCard both depend on catalog.modelsById and silently showed
    empty/"no priced model" with no indication the catalog fetch itself
    had failed. -->
    <p v-if="catalog.error" class="text-sm break-words text-destructive">{{ catalog.error }}</p>
    <!-- verify-ui-states-3.md pre-existing fix: fetchModelRanking's own
    failure has its own field (rankingError), never cross-cleared by a
    later, unrelated fetchBreakdown success — shown unconditionally next to
    the chart since nothing else surfaces it (by=model then silently renders
    100% "Other" off a still-empty ranking). -->
    <p v-if="spend.rankingError" class="text-sm break-words text-destructive">{{ spend.rankingError }}</p>
    <!-- P3 item 19 / Q5: a requested-but-unavailable comparison (a
    single-provider scope) used to leave the "vs previous period" toggle
    pressed with no dashed line and no explanation. -->
    <p v-if="filters.cmpSpec.requested && spend.comparisonUnavailableReason" class="text-sm text-muted-foreground">
      {{ spend.comparisonUnavailableReason }}
    </p>

    <ChartCard
      bare
      :state="breakdownState"
      :error="spend.breakdownError"
      :on-retry="spend.refresh"
      :labels="breakdownLabels"
      :datasets="breakdownDatasets"
      :comparison-datasets="comparisonDatasets"
      stacked
      :value-formatter="valueFormatter"
      :aria-label="`${SPEND_METRIC_LABEL[metric]} breakdown ${SPEND_BY_LABEL[by].toLowerCase()} over time`"
    />
    <!-- verify-ui-states-3.md NEW-1: a background refetch failure (poll
    tick, or a metric/by/range/scope change) while the PREVIOUS selection's
    data is still on screen — breakdownState stays 'ready' (loadState's own
    precedence), so the ErrorState above never mounts. Without this line the
    failure was invisible: the chart kept the old series under the new
    labels/formatter with no indication anything had gone wrong. -->
    <p v-if="spend.breakdownError && breakdownState === 'ready'" class="text-sm break-words text-destructive">{{ spend.breakdownError }}</p>

    <div class="grid grid-cols-1 gap-4 lg:grid-cols-2">
      <BurnDownChart
        v-if="spend.burndownAvailable"
        :buckets="spend.burndownBuckets"
        :points="spend.burndownPoints"
        :budget-micros="spend.burndownBudgetMicros"
        :now="now"
        :loading="spend.burndownLoading"
        :error="spend.burndownError"
        :on-retry="spend.refresh"
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

    <PricingHealthTable
      :models="spend.pricingHealthRanking"
      :catalog="catalog.modelsById"
      :loading="spend.pricingHealthLoading"
      :loaded="spend.pricingHealthLoaded"
      :error="spend.pricingHealthError || catalog.error"
      :on-retry="retryPricingHealthTable"
    />

    <AttributionDrilldown
      :rows="spend.drilldownRows"
      :metrics="spend.drilldownMetrics"
      :drill="drillParam"
      :disabled="spend.drilldownDisabled"
      :user-model-stats-enabled="userModelStatsEnabled"
      :window="filters.window"
      :span="filters.span"
      :loading="spend.drilldownLoading"
      :loaded="spend.drilldownLoaded"
      :error="spend.drilldownError"
      :on-retry="spend.fetchDrilldown"
      @update:drill="onDrillUpdate"
    />
  </div>
</template>
