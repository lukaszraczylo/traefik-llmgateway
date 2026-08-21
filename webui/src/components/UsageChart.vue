<script setup lang="ts">
import type { ChartData, ChartOptions } from 'chart.js'
import { computed } from 'vue'
import { Bar } from 'vue-chartjs'

import '@/lib/chart-setup'
import { useThemeColors } from '@/composables/useThemeColors'
import { formatBucketLabel } from '@/lib/format'
import type { ChartTab } from '@/stores/history'
import type { HistoryMetric, HistoryWindow, UsageHistoryPoint } from '@/types/api'

const props = defineProps<{
  tab: ChartTab
  window: HistoryWindow
  seriesByMetric: Partial<Record<HistoryMetric, UsageHistoryPoint[]>>
}>()

const { foreground, mutedForeground, border } = useThemeColors()

/** One dataset spec per tab: which metric(s), its label, and its token color. */
const DATASET_SPECS: Record<ChartTab, { metric: HistoryMetric; label: string; color: string }[]> = {
  requests: [{ metric: 'req', label: 'Requests', color: '--chart-requests' }],
  tokens: [
    { metric: 'tokin', label: 'Tokens in', color: '--chart-tokens-in' },
    { metric: 'tokout', label: 'Tokens out', color: '--chart-tokens-out' },
  ],
  cost: [{ metric: 'cost', label: 'Cost (USD)', color: '--chart-cost' }],
}

/** cost is stored as micro-USD (limits.go's account/history convention) — every other metric is already the display unit. */
function toDisplayValue(metric: HistoryMetric, raw: number): number {
  return metric === 'cost' ? raw / 1_000_000 : raw
}

const labels = computed<string[]>(() => {
  const specs = DATASET_SPECS[props.tab]
  const points = props.seriesByMetric[specs[0]!.metric] ?? []
  return points.map((p) => formatBucketLabel(p.bucket, props.window))
})

/** resolveToken reads one of main.css's --chart-* custom properties from :root, live — so a chart already on screen repaints on a `prefers-color-scheme` flip exactly like useThemeColors does for axis/legend text. */
function resolveToken(cssVar: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(cssVar).trim()
}

const chartData = computed<ChartData<'bar'>>(() => {
  // Touch the theme refs so this computed re-runs (and re-reads the CSS
  // custom properties via resolveToken) on a `prefers-color-scheme` flip —
  // resolveToken's own getComputedStyle read is not itself reactive.
  void foreground.value
  return {
    labels: labels.value,
    datasets: DATASET_SPECS[props.tab].map((spec) => ({
      label: spec.label,
      backgroundColor: resolveToken(spec.color),
      data: (props.seriesByMetric[spec.metric] ?? []).map((p) => toDisplayValue(spec.metric, p.value)),
    })),
  }
})

const stacked = computed<boolean>(() => DATASET_SPECS[props.tab].length > 1)

const chartOptions = computed<ChartOptions<'bar'>>(() => ({
  responsive: true,
  maintainAspectRatio: false,
  animation: false,
  scales: {
    x: {
      stacked: stacked.value,
      grid: { color: border.value, display: false },
      ticks: { color: mutedForeground.value },
    },
    y: {
      // Zero-based, honest axis (dataviz rule): a bar chart's y-axis never
      // starts anywhere but 0, so bar length always means what it looks
      // like it means.
      stacked: stacked.value,
      beginAtZero: true,
      grid: { color: border.value },
      ticks: {
        color: mutedForeground.value,
        callback: (value) => (props.tab === 'cost' ? `$${value}` : String(value)),
      },
    },
  },
  plugins: {
    legend: {
      display: stacked.value,
      labels: { color: foreground.value },
    },
    tooltip: {
      callbacks: {
        label: (ctx) => {
          const value = ctx.parsed.y ?? 0
          return `${ctx.dataset.label}: ${props.tab === 'cost' ? `$${value.toFixed(4)}` : value}`
        },
      },
    },
  },
}))
</script>

<template>
  <div class="h-72 w-full">
    <Bar :data="chartData" :options="chartOptions" />
  </div>
</template>
