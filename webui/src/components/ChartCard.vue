<script setup lang="ts">
import EmptyState from '@/components/EmptyState.vue'
import ErrorState from '@/components/ErrorState.vue'
import SkeletonChart from '@/components/SkeletonChart.vue'
import TimeSeriesChart from '@/components/TimeSeriesChart.vue'
import type { TimeSeriesComparisonDataset, TimeSeriesDataset } from '@/components/TimeSeriesChart.vue'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { formatExactInt } from '@/lib/format'
import type { LoadState } from '@/lib/load-state'

/**
 * ChartCard (reuse-audit.md F1) is the ONE `Card > CardHeader(Title,
 * Description?) > CardContent > skeleton|error|(empty)|chart` shell every
 * time-series chart card in this panel used to hand-roll — ReliabilityPage
 * (5 real chart cards + 4 skeleton-only loading cards), UserDetail
 * (requests/cost), SpendPage (breakdown), BurnDownChart. `state` drives the
 * skeleton/error/ready branch (lib/load-state.ts's LoadState — the SAME
 * precedence every other card in this panel already follows); `error` and
 * `onRetry` forward straight to ErrorState. The default slot overrides the
 * ready-state body for a chart that needs more than a bare TimeSeriesChart
 * (BurnDownChart's own stat row above its chart) — omitted, this renders
 * TimeSeriesChart directly from `labels`/`datasets`/etc. `#header-extra`
 * adds header content beyond title/description (e.g. a per-card control)
 * without needing the caller to rebuild CardHeader itself.
 *
 * `showEmpty` (default false, opt-in) is the ONLY thing that turns
 * `state === 'empty'` into a distinct EmptyState render instead of falling
 * through to the ready branch (the default slot, or the bare
 * TimeSeriesChart). None of today's call sites had an "empty" concept
 * before this component existed — they always rendered their chart (or,
 * for BurnDownChart, its stat row above the chart) for any non-skeleton,
 * non-error state, even with zero data points. Defaulting `showEmpty` to
 * false keeps that exact behaviour; a future card that genuinely wants
 * "No data for this range." text can opt in per-instance.
 *
 * `bare` (true) skips the Card/CardHeader/CardTitle chrome entirely,
 * rendering only the state-branch content — SpendPage's breakdown chart
 * has its own bespoke header (Tabs, a metric Select, a running total) and,
 * today, no Card wrapper at all around the chart; forcing one on would be
 * a visible layout change this refactor must not make (behaviour-
 * preserving rule), so `bare` lets it reuse the skeleton/error/ready
 * branch without the surrounding card border/padding.
 */
const props = withDefaults(
  defineProps<{
    title?: string
    description?: string
    state: LoadState
    error?: string
    onRetry?: () => void | Promise<void>
    emptyTitle?: string
    emptyDescription?: string
    /** Opt-in: renders EmptyState instead of the ready branch when `state === 'empty'` — see this component's own doc comment. Defaults to false (behaviour-preserving: 'empty' falls through to ready everywhere until a caller explicitly asks for the distinct empty render). */
    showEmpty?: boolean
    bare?: boolean
    /** Omit when this ChartCard is only ever used in a non-'ready' state (e.g. one of the 4 skeleton-only loading cards below) — never read outside the 'ready' branch. */
    labels?: string[]
    datasets?: TimeSeriesDataset[]
    comparisonDatasets?: TimeSeriesComparisonDataset[]
    stacked?: boolean
    valueFormatter?: (value: number) => string
    /** Required whenever `state` can reach 'ready' with the default TimeSeriesChart body (TimeSeriesChart's own ariaLabel doc comment: no safe generic default) — optional here only so a skeleton-only ChartCard is not forced to pass a meaningless placeholder. */
    ariaLabel?: string
  }>(),
  {
    description: undefined,
    error: '',
    onRetry: undefined,
    emptyTitle: 'No data for this range.',
    emptyDescription: undefined,
    showEmpty: false,
    bare: false,
    labels: () => [],
    datasets: () => [],
    comparisonDatasets: () => [],
    stacked: false,
    valueFormatter: formatExactInt,
    ariaLabel: '',
  },
)
</script>

<template>
  <template v-if="bare">
    <SkeletonChart v-if="state === 'skeleton'" />
    <ErrorState v-else-if="state === 'error'" :message="error" :on-retry="onRetry" />
    <EmptyState v-else-if="state === 'empty' && showEmpty" :title="emptyTitle" :description="emptyDescription" />
    <slot v-else>
      <TimeSeriesChart
        :labels="labels"
        :datasets="datasets"
        :comparison-datasets="comparisonDatasets"
        :stacked="stacked"
        :value-formatter="valueFormatter"
        :ariaLabel="ariaLabel"
      />
    </slot>
  </template>
  <Card v-else>
    <CardHeader>
      <CardTitle>{{ title }}</CardTitle>
      <CardDescription v-if="description">{{ description }}</CardDescription>
      <slot name="header-extra" />
    </CardHeader>
    <CardContent>
      <SkeletonChart v-if="state === 'skeleton'" />
      <ErrorState v-else-if="state === 'error'" :message="error" :on-retry="onRetry" />
      <EmptyState v-else-if="state === 'empty' && showEmpty" :title="emptyTitle" :description="emptyDescription" />
      <slot v-else>
        <TimeSeriesChart
          :labels="labels"
          :datasets="datasets"
          :comparison-datasets="comparisonDatasets"
          :stacked="stacked"
          :value-formatter="valueFormatter"
          :ariaLabel="ariaLabel"
        />
      </slot>
    </CardContent>
  </Card>
</template>
