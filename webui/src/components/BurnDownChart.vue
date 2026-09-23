<script setup lang="ts">
import { computed } from 'vue'

import ErrorState from '@/components/ErrorState.vue'
import SkeletonChart from '@/components/SkeletonChart.vue'
import TimeSeriesChart from '@/components/TimeSeriesChart.vue'
import type { TimeSeriesDataset } from '@/components/TimeSeriesChart.vue'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { cumulative, runOutDate } from '@/lib/burndown'
import { formatBucketLabel, formatCost } from '@/lib/format'
import { loadState } from '@/lib/load-state'

/**
 * BurnDownChart (redesign-plan.md section 3.4) renders the current UTC
 * calendar month's cumulative spend against its configured budget: a
 * solid line (lib/burndown.ts's `cumulative`, run over the store's own raw
 * per-day points — see stores/spend.ts's own doc comment on why the
 * running-sum step lives here rather than in the store) plus, when a
 * budget is configured, a dashed reference line at that flat limit
 * (TimeSeriesChart's own `comparisonDatasets` — a budget ceiling is
 * exactly the "reference overlay, not a component of the current total"
 * that prop already documents) and the projected run-out date
 * (lib/burndown.ts's runOutDate, MTD average daily rate).
 *
 * `now` is a required prop, not read from `new Date()` internally —
 * CostForecastCard.vue's own doc comment explains why a component-local
 * default silently freezes at first mount (Vue caches a prop default
 * once per instance): the caller passes the SAME shared clock
 * (composables/useNow.ts) every other time-dependent read on the page
 * shares.
 */
const props = defineProps<{
  /** Day-window bucket keys, oldest first (limits.go's "20060102" format — lib/format.ts's formatBucketLabel). */
  buckets: string[]
  /** Raw (not cumulative) per-day cost in micro-USD, index-aligned with `buckets`. */
  points: number[]
  /** The scope's configured cost/month budget in micro-USD, or null when none is configured. */
  budgetMicros: number | null
  now: Date
  /** spend.burndownLoading (verify-ui-states.md #7 fix, Spend page gap) — this card had no loading/error input at all before: `buckets` starts as [], so the very first Spend load, and any failed burn-down fetch, rendered a flat all-zero chart with no indication either was happening. */
  loading?: boolean
  error?: string
  /** Renders ErrorState's own "Retry" button — SpendPage.vue's own spend.refresh (fetchBurndown has no standalone public retry of its own). */
  onRetry?: () => void | Promise<void>
}>()

const state = computed(() => loadState({ loading: props.loading ?? false, hasData: props.buckets.length > 0, error: props.error ?? '' }))

const labels = computed<string[]>(() => props.buckets.map((b) => formatBucketLabel(b, 'day')))
const cumulativePoints = computed<number[]>(() => cumulative(props.points))
const spentMicros = computed<number>(() => cumulativePoints.value.at(-1) ?? 0)

const datasets = computed<TimeSeriesDataset[]>(() => [{ label: 'Spent (MTD)', data: cumulativePoints.value, color: '--chart-cost' }])

const comparisonDatasets = computed<TimeSeriesDataset[]>(() => {
  if (props.budgetMicros === null) return []
  const flat = new Array<number>(cumulativePoints.value.length).fill(props.budgetMicros)
  return [{ label: 'Budget', data: flat, color: '--destructive' }]
})

const runOut = computed<Date | null>(() => (props.budgetMicros === null ? null : runOutDate(spentMicros.value, props.budgetMicros, props.now)))

const willExceed = computed<boolean>(() => props.budgetMicros !== null && spentMicros.value >= props.budgetMicros)
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>Burn-down</CardTitle>
      <CardDescription>Cumulative spend this UTC calendar month against the configured budget.</CardDescription>
    </CardHeader>
    <CardContent class="flex flex-col gap-4">
      <SkeletonChart v-if="state === 'skeleton'" />
      <ErrorState v-else-if="state === 'error'" :message="error ?? ''" :on-retry="onRetry" />
      <template v-else>
        <div class="grid grid-cols-2 gap-4 text-sm sm:grid-cols-3">
          <div>
            <p class="text-muted-foreground">Spent so far</p>
            <p class="text-lg font-semibold tabular-nums" :class="willExceed ? 'text-destructive' : undefined">
              {{ formatCost(spentMicros) }}
            </p>
          </div>
          <div v-if="budgetMicros !== null">
            <p class="text-muted-foreground">Budget</p>
            <p class="text-lg font-semibold tabular-nums">{{ formatCost(budgetMicros) }}</p>
          </div>
          <div v-if="budgetMicros !== null">
            <p class="text-muted-foreground">Projected run-out</p>
            <p v-if="runOut === null" class="text-sm text-muted-foreground">not enough data yet</p>
            <p v-else class="text-lg font-semibold tabular-nums" :class="willExceed ? 'text-destructive' : undefined">
              {{ runOut.toLocaleDateString(undefined, { month: 'short', day: 'numeric' }) }}
            </p>
          </div>
        </div>
        <p v-if="budgetMicros === null" class="text-sm text-muted-foreground">No budget configured for this scope.</p>
        <TimeSeriesChart
          :labels="labels"
          :datasets="datasets"
          :comparison-datasets="comparisonDatasets"
          :value-formatter="formatCost"
          ariaLabel="Cumulative spend this month against the configured budget"
        />
      </template>
    </CardContent>
  </Card>
</template>
