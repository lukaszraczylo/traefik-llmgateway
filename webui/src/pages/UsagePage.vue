<script setup lang="ts">
import { Badge, Button, Card, CardAction, CardContent, CardHeader, CardTitle, KpiTile, LineChart, Pagination, SegmentedControl, StatGroup } from '@perfect-design/ui'
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'

import { useEntityLinks } from '@/composables/useEntityLinks'
import { useNow } from '@/composables/useNow'
import { budgetLineVisible, cumulative, runOutDate } from '@/lib/burndown'
import { cacheSavingsStatus } from '@/lib/cache-savings'
import { thinLabels } from '@/lib/chart-labels'
import { costComparisons, freeRows } from '@/lib/cost-avoided'
import { downloadText } from '@/lib/download'
import type { DownloadOutcome } from '@/lib/download'
import { headroom, monthProgress, NOT_ENOUGH_DATA, projectMonthEnd } from '@/lib/forecast'
import { formatBucketLabel, formatCompactCount, formatCostDisplay, formatModelPriceShort, formatShortDate } from '@/lib/format'
import { joinPricingHealth, pricingHealthOrder } from '@/lib/pricing-health'
import type { PricingHealthRow } from '@/lib/pricing-health'
import { loadState } from '@/lib/load-state'
import { pageWindow } from '@/lib/paging'
import { createVisibilityPoller } from '@/lib/polling'
import type { VisibilityPoller } from '@/lib/polling'
import { deltaOf, sumPoints } from '@/lib/overview-metrics'
import type { ChartLine } from '@/lib/overview-metrics'
import { parseScope, scopeBudgetLimits, scopeId, scopeMtdMicros } from '@/lib/scope'
import { seriesToStacked } from '@/lib/series'
import { breakdownCsv } from '@/lib/usage-csv'
import { formatHitRate, promptCacheHitRate, promptCacheSavedMicros } from '@/lib/prompt-cache'
import { wbColumns } from '@/lib/wb-columns'
import { useCatalogStore } from '@/stores/catalog'
import { useDashboardStore } from '@/stores/dashboard'
import { useFiltersStore } from '@/stores/filters'
import { useNavStore } from '@/stores/nav'
import { isSpendMetric, SPEND_METRIC_LABEL, SPEND_METRICS, useSpendStore } from '@/stores/spend'
import { USERS_RANGE_TOO_LARGE, useUsageUsersStore } from '@/stores/usage-users'
import type { HistoryMetric } from '@/types/api'
import ModelName from '@/widgets/ModelName.vue'
import ShareCell from '@/widgets/ShareCell.vue'
import StateView from '@/widgets/StateView.vue'
import type { ViewState } from '@/widgets/StateView.vue'
import WbTable from '@/widgets/WbTable.vue'

const BREAKDOWNS = ['model', 'group', 'user'] as const
type Breakdown = (typeof BREAKDOWNS)[number]
const BREAKDOWN_LABEL: Record<Breakdown, string> = { model: 'Model', group: 'Group', user: 'User' }
const BREAKDOWN_ENTITY: Record<Breakdown, 'model' | 'group' | 'user'> = { model: 'model', group: 'group', user: 'user' }
const CHART_TOP = 5
const TABLE_PAGE_SIZE = 10
const HEALTH_PAGE_SIZE = 10
const USERS_POLL_MS = 60_000
const FEEDBACK_MS = 2500
const DETAIL_NOTICE = 'Per-model tokens and cost are not available for this range: the server limits detail to shorter ranges.'

const nav = useNavStore()
const filters = useFiltersStore()
const dashboard = useDashboardStore()
const catalog = useCatalogStore()
const spend = useSpendStore()
const users = useUsageUsersStore()
const { openEntity } = useEntityLinks()
const { now, start: startClock, stop: stopClock } = useNow()

// --- Page params (hash): by, metric ------------------------------------

const by = computed<Breakdown>(() => (BREAKDOWNS as readonly string[]).includes(nav.params.by ?? '') ? (nav.params.by as Breakdown) : 'model')
const metric = computed<HistoryMetric>(() => (nav.params.metric && isSpendMetric(nav.params.metric) ? nav.params.metric : 'cost'))

function updateParams(next: Partial<{ by: Breakdown; metric: HistoryMetric }>): void {
  const merged = { by: next.by ?? by.value, metric: next.metric ?? metric.value }
  const params: Record<string, string> = {}
  if (merged.by !== 'model') params.by = merged.by
  if (merged.metric !== 'cost') params.metric = merged.metric
  nav.goTo('usage', params)
}

const metricOptions = SPEND_METRICS.map((m) => ({ value: m, label: SPEND_METRIC_LABEL[m] }))
const byOptions = BREAKDOWNS.map((b) => ({ value: b, label: BREAKDOWN_LABEL[b] }))

function onMetric(v: string): void {
  if (isSpendMetric(v)) updateParams({ metric: v })
}
function onBy(v: string): void {
  if ((BREAKDOWNS as readonly string[]).includes(v)) updateParams({ by: v as Breakdown })
}

// --- Data wiring -------------------------------------------------------------

const userMode = computed(() => by.value === 'user' && filters.scope === 'all')
let usersPoller: VisibilityPoller | undefined

function syncStore(): void {
  spend.setSelection({ by: by.value === 'group' ? 'group' : 'model', metric: metric.value, drill: '' })
  if (by.value === 'user') void users.refresh(metric.value)
}

watch([by, metric], syncStore)
watch(
  () => [filters.range, filters.cmp, filters.scope],
  () => {
    void spend.refresh()
    if (by.value === 'user') void users.refresh(metric.value)
  },
)

onMounted(() => {
  spend.$patch({ by: by.value === 'group' ? 'group' : 'model', metric: metric.value, drill: '' })
  void catalog.ensureLoaded()
  spend.startPolling()
  startClock()
  if (by.value === 'user') void users.refresh(metric.value)
  usersPoller = createVisibilityPoller({ intervalMs: USERS_POLL_MS, tick: () => (by.value === 'user' ? users.refresh(metric.value) : undefined) })
  usersPoller.start()
})
onUnmounted(() => {
  spend.stopPolling()
  usersPoller?.stop()
  stopClock()
})

// --- Formatting -------------------------------------------------------------

const fmt = computed<(n: number) => string>(() => (metric.value === 'cost' ? formatCostDisplay : formatCompactCount))
const fmtValue = (v: unknown): string => (typeof v === 'number' && Number.isFinite(v) ? fmt.value(v) : '—')

// --- Chart -----------------------------------------------------------------

const labels = computed(() => spend.buckets.map((b) => formatBucketLabel(b, filters.window)))

function addPoints(a: number[] | null, b: number[] | null): number[] | null {
  if (!a) return b
  if (!b) return a
  return a.map((v, i) => v + (b[i] ?? 0))
}

const lines = computed<ChartLine[]>(() => {
  const src = userMode.value ? { series: users.series, other: users.otherPoints } : { series: spend.breakdown, other: spend.otherPoints }
  const stacked = seriesToStacked(src.series, CHART_TOP)
  const out: ChartLine[] = stacked.series.map((s) => ({ name: scopeId(s.scope), data: s.points }))
  const rest = addPoints(stacked.other, src.other)
  if (rest?.some((v) => v > 0)) out.push({ name: 'Other', data: rest, tone: 'muted' })
  if (out.length === 0) return []
  if (spend.comparisonPoints) out.push({ name: 'Previous period', data: spend.comparisonPoints, tone: 'primary', stack: false })
  return out
})

const chartState = computed<ViewState>(() => {
  const s = loadState({ loading: spend.loading, hasData: spend.buckets.length > 0, error: spend.breakdownError })
  if (s === 'ready') return lines.value.some((l) => l.data.some((v) => v > 0)) ? 'ready' : 'empty'
  return s === 'skeleton' ? 'loading' : s
})

const totalInRange = computed(() => sumPoints(spend.totalPoints))
const previousTotal = computed(() => (spend.comparisonPoints ? sumPoints(spend.comparisonPoints) : null))
const rangeDelta = computed(() => deltaOf(totalInRange.value, previousTotal.value))
const sparkline = computed(() => (spend.totalPoints.length >= 2 && spend.totalPoints.some((v) => v > 0) ? spend.totalPoints : undefined))
const totalReady = computed(() => spend.buckets.length > 0)

const scopeNote = computed(() => (filters.scope === 'all' ? '' : `Chart shows ${filters.scope} directly; clear the scope filter to break it down by ${by.value}.`))
const compareNote = computed(() => (filters.cmpSpec.requested ? spend.comparisonUnavailableReason : null))

// --- Breakdown table -------------------------------------------------------

interface BreakdownRow {
  id: string
  name: string
  value: number
  share: number | null
  /** Set for a zero-valued model row with detail: what to show instead of a zero cost. */
  zeroText: string | null
  /** Prompt-cache hit rate; null when unknown or not a model row. */
  cacheRate: number | null
}

const columns = computed(() =>
  wbColumns<BreakdownRow>([
    { key: 'name', header: BREAKDOWN_LABEL[by.value], kind: 'text', sortable: false },
    { key: 'value', header: SPEND_METRIC_LABEL[metric.value], kind: 'number', sortable: false },
    { key: 'share', header: 'Share', kind: 'share', sortable: false },
    ...(by.value === 'model' && spend.detailAvailable ? [{ key: 'cacheRate', header: 'Prompt cache', kind: 'number' as const, sortable: false }] : []),
  ]),
)

const sourceRows = computed<{ id: string; value: number; free?: boolean; cacheRate?: number | null }[]>(() => {
  if (by.value === 'model') return spend.modelRanking.map((m) => ({ id: m.id, value: m.value, free: m.free, cacheRate: promptCacheHitRate(m) }))
  if (by.value === 'group') return spend.drilldownRows.map((r) => ({ id: r.id, value: r.values[metric.value] ?? 0 }))
  return users.rows
})

function zeroText(r: { value: number; free?: boolean }): string | null {
  if (by.value !== 'model' || !spend.detailAvailable || r.value !== 0) return null
  return r.free && metric.value === 'cost' ? 'free' : '—'
}

const tableRows = computed<BreakdownRow[]>(() => {
  const rows = sourceRows.value.filter((r) => r.value > 0 || by.value === 'model').sort((a, b) => b.value - a.value)
  const sum = rows.reduce((acc, r) => acc + r.value, 0)
  const total = filters.scope === 'all' && totalInRange.value > 0 ? totalInRange.value : sum
  return rows.map((r) => ({ id: r.id, name: r.id, value: r.value, share: total > 0 ? Math.min(1, r.value / total) : null, zeroText: zeroText(r), cacheRate: r.cacheRate ?? null }))
})

const tablePage = ref(1)
const tableWindow = computed(() => pageWindow(tableRows.value.length, TABLE_PAGE_SIZE, tablePage.value - 1))
const pagedRows = computed(() => tableRows.value.slice(tableWindow.value.from, tableWindow.value.to))
watch([by, metric, () => filters.range, () => filters.scope], () => (tablePage.value = 1))

const tableError = computed(() => {
  if (by.value === 'model') return spend.rankingError
  if (by.value === 'group') return spend.drilldownError
  return users.error
})
const tableState = computed<ViewState>(() => {
  const loading = by.value === 'model' ? spend.loading : by.value === 'group' ? spend.drilldownLoading : !users.loaded
  const hasData = by.value === 'user' ? users.loaded : by.value === 'group' ? spend.drilldownLoaded : spend.modelRanking.length > 0 || (spend.rankingError === '' && !spend.loading)
  const s = loadState({ loading, hasData, error: tableError.value })
  if (s === 'ready') return tableRows.value.length === 0 ? 'empty' : 'ready'
  return s === 'skeleton' ? 'loading' : s
})
const tableRetryable = computed(() => tableError.value !== USERS_RANGE_TOO_LARGE)

function retryTable(): void {
  if (by.value === 'user') void users.refresh(metric.value)
  else void spend.refresh()
}

// --- Burn-down + forecast --------------------------------------------------

const forecastAvailable = computed(() => {
  const parsed = parseScope(filters.scope)
  return !parsed || parsed.kind === 'group' || parsed.kind === 'user'
})
const mtdMicros = computed(() => scopeMtdMicros(filters.scope, dashboard.usage))
const limits = computed(() => scopeBudgetLimits(filters.scope, dashboard.overview?.groups ?? [], dashboard.usage?.users ?? []))
const projected = computed(() => projectMonthEnd(mtdMicros.value, monthProgress(now.value)))
const room = computed(() => headroom(projected.value, limits.value?.costPerMonthUSD))

const burnLabels = computed(() => thinLabels(spend.burndownBuckets.map((b) => formatBucketLabel(b, 'day'))))
const burnSpent = computed(() => cumulative(spend.burndownPoints))
const burnTotal = computed(() => burnSpent.value.at(-1) ?? 0)
const burnLines = computed<ChartLine[]>(() => {
  if (burnSpent.value.length === 0) return []
  const out: ChartLine[] = [{ name: 'Spent (MTD)', data: burnSpent.value, tone: 'primary' }]
  const budget = spend.burndownBudgetMicros
  if (budget !== null && budgetLineVisible(burnTotal.value, budget)) out.push({ name: 'Budget', data: burnSpent.value.map(() => budget), tone: 'danger' })
  return out
})
const runOut = computed(() => (spend.burndownBudgetMicros === null ? null : runOutDate(burnTotal.value, spend.burndownBudgetMicros, now.value)))
const burnState = computed<ViewState>(() => {
  const s = loadState({ loading: spend.burndownLoading, hasData: spend.burndownBuckets.length > 0, error: spend.burndownError })
  return s === 'skeleton' ? 'loading' : s
})

// --- Cost avoided -----------------------------------------------------------

const cacheSavings = computed(() => cacheSavingsStatus(filters.window))
const cacheEnabled = computed(() => dashboard.overview?.features.cacheStats ?? false)
const freeUsage = computed(() => freeRows(spend.modelRanking))
const freeTokens = computed(() => ({
  in: freeUsage.value.reduce((acc, r) => acc + r.tokensIn, 0),
  out: freeUsage.value.reduce((acc, r) => acc + r.tokensOut, 0),
}))
const comparisons = computed(() => costComparisons(freeUsage.value, catalog.modelsById))
const rateText = (inP: number | null, outP: number | null): string => (inP === null || outP === null ? 'n/a' : `$${inP} / $${outP} per 1M`)
const promptCacheSaved = computed(() => promptCacheSavedMicros(spend.modelRanking))
const cacheSavedMicros = computed(() => spend.modelRanking.reduce((acc, m) => acc + (m.cacheSavedMicroUsd ?? 0), 0))
const avoidedState = computed<ViewState>(() => {
  if (spend.rankingError && spend.modelRanking.length === 0) return 'error'
  return spend.modelRanking.length === 0 && spend.loading ? 'loading' : 'ready'
})

// --- Pricing health ----------------------------------------------------------

const healthColumns = computed(() =>
  wbColumns<PricingHealthRow>([
    { key: 'id', header: 'Model', kind: 'text', sortable: false },
    { key: 'priceSource', header: 'Source', kind: 'status', sortable: false },
    { key: 'inputPerMTokUsd', header: 'Price in / out per MTok', kind: 'text', sortable: false },
    ...(spend.pricingDetailAvailable
      ? [
          { key: 'requests', header: 'Requests', kind: 'number' as const, sortable: false },
          { key: 'costMicroUsd', header: 'Cost', kind: 'number' as const, sortable: false },
        ]
      : []),
  ]),
)
const healthRows = computed(() => pricingHealthOrder(joinPricingHealth(spend.pricingHealthRanking, catalog.modelsById)))
const healthPage = ref(1)
const healthWindow = computed(() => pageWindow(healthRows.value.length, HEALTH_PAGE_SIZE, healthPage.value - 1))
const healthPaged = computed(() => healthRows.value.slice(healthWindow.value.from, healthWindow.value.to))
watch(() => [filters.range, filters.scope], () => (healthPage.value = 1))
const healthError = computed(() => spend.pricingHealthError || catalog.error)
const healthState = computed<ViewState>(() => {
  const s = loadState({ loading: spend.pricingHealthLoading || catalog.loading, hasData: spend.pricingHealthLoaded && !catalog.error, error: healthError.value })
  if (s === 'ready') return healthRows.value.length === 0 ? 'empty' : 'ready'
  return s === 'skeleton' ? 'loading' : s
})
const unpricedCount = computed(() => healthRows.value.filter((r) => r.priceSource === 'unpriced').length)

function priceText(row: PricingHealthRow): string {
  if (row.priceSource === 'unpriced' || row.inputPerMTokUsd === undefined || row.outputPerMTokUsd === undefined) return '—'
  return formatModelPriceShort(row.inputPerMTokUsd, row.outputPerMTokUsd)
}
function retryHealth(): void {
  void spend.fetchPricingHealthRanking()
  if (catalog.error) void catalog.fetch()
}

// --- CSV export ---------------------------------------------------------------

const feedback = ref<DownloadOutcome | null>(null)
const FEEDBACK_LABEL: Record<DownloadOutcome, string> = { downloaded: 'Downloaded', copied: 'Copied', selected: 'Selected', failed: 'Export failed' }
let feedbackTimer: ReturnType<typeof setTimeout> | undefined

function buildCsv(): string {
  return breakdownCsv(by.value, metric.value, tableRows.value)
}

async function exportCsv(): Promise<void> {
  feedback.value = await downloadText(`usage-${by.value}.csv`, buildCsv())
  if (feedbackTimer !== undefined) clearTimeout(feedbackTimer)
  feedbackTimer = setTimeout(() => (feedback.value = null), FEEDBACK_MS)
}
onUnmounted(() => {
  if (feedbackTimer !== undefined) clearTimeout(feedbackTimer)
})

const rangeLabel = computed(() => `last ${filters.range}`)
const money = (micros: number | null): string | null => (micros === null ? null : formatCostDisplay(micros))
</script>

<template>
  <div class="flex flex-col gap-4">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div class="flex flex-wrap items-center gap-3">
        <SegmentedControl :model-value="metric" :options="metricOptions" size="sm" aria-label="Metric" @update:model-value="onMetric" />
        <SegmentedControl :model-value="by" :options="byOptions" size="sm" aria-label="Break down by" @update:model-value="onBy" />
      </div>
      <Button variant="outline" size="sm" @click="exportCsv">{{ feedback ? FEEDBACK_LABEL[feedback] : 'Export CSV' }}</Button>
    </div>

    <StatGroup :columns="4" gap="sm" aria-label="Spend summary">
      <KpiTile
        :label="`${SPEND_METRIC_LABEL[metric]} in range`"
        size="compact"
        :value="totalReady ? fmt(totalInRange) : null"
        :loading="!totalReady && !spend.breakdownError"
        :delta="rangeDelta?.text"
        :trend="rangeDelta?.trend"
        :good-when="metric === 'cost' ? 'down' : 'none'"
        :sparkline="sparkline"
        :hint="rangeLabel"
      />
      <KpiTile
        label="Month to date"
        size="compact"
        :value="forecastAvailable && dashboard.usage ? money(mtdMicros) : null"
        :loading="forecastAvailable && !dashboard.usage"
        good-when="down"
        :hint="forecastAvailable ? 'UTC calendar month' : 'no budget for a provider scope'"
      />
      <KpiTile
        label="Projected month-end"
        size="compact"
        :value="forecastAvailable && dashboard.usage ? (projected === null ? NOT_ENOUGH_DATA : money(projected)) : null"
        :loading="forecastAvailable && !dashboard.usage"
        :tone="room.willExceed ? 'danger' : undefined"
        good-when="down"
        :hint="room.limitMicros !== null ? `limit ${formatCostDisplay(room.limitMicros)}` : 'no limit configured'"
      />
      <KpiTile
        label="Budget run-out"
        size="compact"
        :value="spend.burndownBudgetMicros === null ? 'none' : runOut === null ? NOT_ENOUGH_DATA : formatShortDate(runOut)"
        :loading="spend.burndownLoading && spend.burndownBuckets.length === 0"
        :tone="runOut !== null && spend.burndownBudgetMicros !== null && burnTotal >= spend.burndownBudgetMicros ? 'danger' : undefined"
        good-when="none"
        :hint="spend.burndownBudgetMicros === null ? 'no budget for this scope' : `budget ${formatCostDisplay(spend.burndownBudgetMicros)}`"
      />
    </StatGroup>

    <p v-if="scopeNote" class="text-muted-foreground m-0 text-sm">{{ scopeNote }}</p>
    <p v-if="compareNote" class="text-muted-foreground m-0 text-sm">{{ compareNote }}</p>

    <Card size="sm">
      <CardHeader><CardTitle>{{ SPEND_METRIC_LABEL[metric] }} over time</CardTitle></CardHeader>
      <CardContent>
        <StateView :state="chartState" empty-title="No usage in this range" :error-message="spend.breakdownError" @retry="spend.refresh()">
          <div data-wb-ignore-numeric>
            <LineChart
              :labels="labels"
              :series="lines"
              variant="area"
              stacked
              :height="240"
              :value-formatter="fmt"
              :label="`${SPEND_METRIC_LABEL[metric]} per bucket by ${by}`"
            />
          </div>
        </StateView>
      </CardContent>
    </Card>

    <div class="grid grid-cols-1 items-start gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(0,24rem)]">
      <Card size="sm">
        <CardHeader><CardTitle>By {{ BREAKDOWN_LABEL[by].toLowerCase() }}</CardTitle></CardHeader>
        <CardContent class="flex flex-col gap-2">
          <StateView
            :state="tableState"
            :empty-title="`No ${by} usage in this range`"
            :error-message="tableError"
            :retryable="tableRetryable"
            @retry="retryTable"
          >
            <WbTable
                :columns="columns"
              :rows="pagedRows"
              row-key="id"
              :label="`${SPEND_METRIC_LABEL[metric]} by ${by}`"
              @row-activate="(row: BreakdownRow) => openEntity(BREAKDOWN_ENTITY[by], row.id)"
            >
              <template #cell-name="{ value }">
                <ModelName v-if="by === 'model'" :id="value as string" />
                <template v-else>{{ value }}</template>
              </template>
              <template #cell-value="{ value, row }">{{ (row as BreakdownRow).zeroText ?? fmtValue(value) }}</template>
              <template #cell-cacheRate="{ value }">
                <span class="font-mono tabular-nums">{{ formatHitRate(value as number | null) }}</span>
              </template>
              <template #cell-share="{ value, row }">
                <ShareCell v-if="!(row as BreakdownRow).zeroText" :value="value as number" />
                <span v-else aria-hidden="true" />
              </template>
            </WbTable>
            <div v-if="tableRows.length > TABLE_PAGE_SIZE" class="flex items-center justify-between gap-2">
              <span class="text-muted-foreground text-xs">{{ tableRows.length.toLocaleString('en-US') }} rows</span>
              <Pagination v-model:page="tablePage" :total="tableRows.length" :page-size="TABLE_PAGE_SIZE" variant="compact" size="sm" />
            </div>
          </StateView>
        </CardContent>
      </Card>

      <div class="flex flex-col gap-4">
        <Card size="sm">
          <CardHeader><CardTitle>Burn-down</CardTitle></CardHeader>
          <CardContent>
            <p v-if="!spend.burndownAvailable" class="text-muted-foreground m-0 text-sm">Not available for a provider scope.</p>
            <StateView v-else :state="burnState" empty-title="No spend this month" :error-message="spend.burndownError" @retry="spend.fetchBurndown()">
              <div data-wb-ignore-numeric>
                <LineChart
                  :labels="burnLabels"
                  :series="burnLines"
                  variant="line"
                  :height="160"
                  :value-formatter="formatCostDisplay"
                  label="Cumulative spend this month against the budget"
                />
              </div>
              <p v-if="spend.burndownBudgetMicros === null" class="text-muted-foreground mt-2 mb-0 text-xs">No budget configured for this scope.</p>
            </StateView>
          </CardContent>
        </Card>

        <Card size="sm">
          <CardHeader>
            <CardTitle>Cost avoided</CardTitle>
          </CardHeader>
          <CardContent class="flex flex-col gap-3">
            <StateView :state="avoidedState" :error-message="spend.rankingError" @retry="spend.refresh()">
              <div v-if="spend.detailAvailable" class="flex flex-col gap-1" data-wb-ignore-numeric>
                <p class="text-muted-foreground m-0 text-xs">
                  Free-tier traffic: <span class="font-mono tabular-nums">{{ formatCompactCount(freeTokens.in) }}</span> in ·
                  <span class="font-mono tabular-nums">{{ formatCompactCount(freeTokens.out) }}</span> out. Cost at:
                </p>
                <ul class="m-0 flex list-none flex-col p-0" aria-label="Cost avoided by reference model">
                  <li
                    v-for="c in comparisons"
                    :key="c.id"
                    class="border-border grid grid-cols-[minmax(0,1fr)_auto] items-baseline gap-x-3 border-t py-2 first:border-t-0"
                  >
                    <span class="min-w-0 truncate text-sm"><ModelName :id="c.id" /></span>
                    <span class="text-right font-mono text-sm font-medium tabular-nums">{{ money(c.avoidedMicros) ?? 'n/a' }}</span>
                    <span class="text-muted-foreground text-xs" :title="c.priceLabel">{{ c.priceLabel }}</span>
                    <span class="text-muted-foreground text-right font-mono text-xs tabular-nums">{{ rateText(c.inPerMTok, c.outPerMTok) }}</span>
                  </li>
                </ul>
              </div>
              <StatGroup v-if="cacheEnabled || spend.detailAvailable" :columns="1" gap="sm" aria-label="Savings">
                <KpiTile
                  v-if="cacheEnabled"
                  label="Cache savings"
                  size="compact"
                  :value="spend.detailAvailable && cacheSavings.available ? formatCostDisplay(cacheSavedMicros) : 'n/a'"
                  good-when="none"
                  :hint="cacheSavings.note ?? rangeLabel"
                />
                <KpiTile
                  v-if="spend.detailAvailable"
                  label="Prompt-cache savings"
                  size="compact"
                  :value="formatCostDisplay(promptCacheSaved)"
                  good-when="none"
                  hint="Paid providers: cached input vs full price"
                />
              </StatGroup>
              <p v-if="!spend.detailAvailable" class="text-muted-foreground m-0 text-xs">{{ DETAIL_NOTICE }}</p>
            </StateView>
          </CardContent>
        </Card>
      </div>
    </div>

    <Card size="sm">
      <CardHeader class="items-center">
        <CardTitle>Pricing health</CardTitle>
        <CardAction v-if="healthState === 'ready'" class="self-center">
          <Badge variant="outline" size="sm" :tone="unpricedCount > 0 ? 'danger' : 'neutral'">{{ unpricedCount }} unpriced</Badge>
        </CardAction>
      </CardHeader>
      <CardContent class="flex flex-col gap-2">
        <StateView :state="healthState" empty-title="No served models in this range" :error-message="healthError" @retry="retryHealth">
          <p v-if="!spend.pricingDetailAvailable" class="text-muted-foreground m-0 text-xs">{{ DETAIL_NOTICE }}</p>
          <WbTable
            :columns="healthColumns"
            :rows="healthPaged"
            row-key="id"
            label="Pricing health by model"
            @row-activate="(row: PricingHealthRow) => openEntity('model', row.id)"
          >
            <template #cell-id="{ value }"><ModelName :id="value as string" /></template>
            <template #cell-priceSource="{ value }">
              <Badge variant="outline" size="sm" :tone="value === 'unpriced' ? 'danger' : 'neutral'">{{ value }}</Badge>
            </template>
            <template #cell-inputPerMTokUsd="{ row }"><span class="font-mono text-xs">{{ priceText(row as PricingHealthRow) }}</span></template>
            <template #cell-requests="{ value }">{{ formatCompactCount(value as number) }}</template>
            <template #cell-costMicroUsd="{ value, row }">{{ (row as PricingHealthRow).priceSource === 'unpriced' ? '—' : formatCostDisplay(value as number) }}</template>
          </WbTable>
          <div v-if="healthRows.length > HEALTH_PAGE_SIZE" class="flex items-center justify-between gap-2">
            <span class="text-muted-foreground text-xs">{{ healthRows.length.toLocaleString('en-US') }} models</span>
            <Pagination v-model:page="healthPage" :total="healthRows.length" :page-size="HEALTH_PAGE_SIZE" variant="compact" size="sm" />
          </div>
        </StateView>
      </CardContent>
    </Card>
  </div>
</template>
