<script setup lang="ts">
import type { ChartData, ChartOptions } from 'chart.js'
import { computed } from 'vue'
import { Bar } from 'vue-chartjs'

import '@/lib/chart-setup'
import { useThemeColors } from '@/composables/useThemeColors'
import { formatCompactCount, formatExactInt } from '@/lib/format'
import { MODEL_METRIC_LABEL, type ModelMetric } from '@/stores/history'
import type { AdminUsageModelEntry } from '@/types/api'

const props = defineProps<{
  metric: ModelMetric
  models: AdminUsageModelEntry[]
}>()

const { foreground, mutedForeground, border } = useThemeColors()

/**
 * One token per rankable metric, reusing the SAME --chart-* custom
 * properties the time-series chart uses (UsageChart.vue's DATASET_SPECS),
 * so a model's bar is the colour that metric already has elsewhere in the
 * view rather than a second, conflicting palette.
 */
const METRIC_COLOR: Record<ModelMetric, string> = {
  req: '--chart-requests',
  tokin: '--chart-tokens-in',
  tokout: '--chart-tokens-out',
  cost: '--chart-cost',
}

/** Cost is stored as micro-USD (limits.go's account/history convention); every other metric is already the display unit. */
function toDisplayValue(metric: ModelMetric, raw: number): number {
  return metric === 'cost' ? raw / 1_000_000 : raw
}

/** resolveToken reads a --chart-* custom property from :root, live — same approach (and same reason) as UsageChart.vue. */
function resolveToken(cssVar: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(cssVar).trim()
}

/**
 * The server already returns the ranking sorted, descending, and capped —
 * this component neither re-sorts nor re-filters it. Chart.js draws the
 * first category at the TOP of a horizontal bar chart, so the busiest
 * model lands where a reader looks first, with no reversal needed.
 */
const chartData = computed<ChartData<'bar'>>(() => {
  void foreground.value // re-run on a prefers-color-scheme flip (see UsageChart.vue)
  return {
    labels: props.models.map((m) => m.id),
    datasets: [
      {
        label: MODEL_METRIC_LABEL[props.metric],
        backgroundColor: resolveToken(METRIC_COLOR[props.metric]),
        data: props.models.map((m) => toDisplayValue(props.metric, m.value)),
      },
    ],
  }
})

/**
 * Height grows with the row count so bars keep a readable thickness
 * instead of compressing into hairlines at 20 models, and so a two-model
 * ranking does not stretch each bar across a third of the screen.
 */
const chartHeight = computed<string>(() => `${Math.max(180, props.models.length * 28 + 60)}px`)

const chartOptions = computed<ChartOptions<'bar'>>(() => ({
  indexAxis: 'y',
  responsive: true,
  maintainAspectRatio: false,
  animation: false,
  scales: {
    x: {
      // Zero-based, honest axis: bar length is the whole comparison here.
      beginAtZero: true,
      grid: { color: border.value },
      ticks: {
        color: mutedForeground.value,
        callback: (value) => (props.metric === 'cost' ? `$${value}` : formatCompactCount(Number(value))),
      },
    },
    y: {
      grid: { display: false },
      ticks: { color: mutedForeground.value, autoSkip: false },
    },
  },
  plugins: {
    legend: { display: false },
    tooltip: {
      callbacks: {
        // Compact figure plus the exact one, matching UsageChart.vue's
        // tooltip contract — a tooltip has room for both, and precision
        // must never be lost to the axis's compact form.
        label: (ctx) => {
          const value = ctx.parsed.x ?? 0
          if (props.metric === 'cost') return `${MODEL_METRIC_LABEL[props.metric]}: $${value.toFixed(4)}`
          return `${MODEL_METRIC_LABEL[props.metric]}: ${formatCompactCount(value)} (${formatExactInt(value)})`
        },
      },
    },
  },
}))
</script>

<template>
  <p v-if="models.length === 0" class="py-8 text-center text-sm text-muted-foreground">
    No model recorded any usage in this window.
  </p>
  <div v-else class="w-full" :style="{ height: chartHeight }">
    <Bar :data="chartData" :options="chartOptions" />
  </div>
</template>
