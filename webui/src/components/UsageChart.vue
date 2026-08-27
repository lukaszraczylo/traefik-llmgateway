<script setup lang="ts">
import type { ChartData, ChartOptions } from 'chart.js'
import { computed } from 'vue'
import { Bar } from 'vue-chartjs'

import '@/lib/chart-setup'
import { useThemeColors } from '@/composables/useThemeColors'
import { formatBucketLabel, formatCompactCount, formatExactInt } from '@/lib/format'
import type { TimeSeriesTab } from '@/stores/history'
import type { HistoryMetric, HistoryWindow, UsageHistoryPoint } from '@/types/api'

const props = defineProps<{
  tab: TimeSeriesTab
  window: HistoryWindow
  seriesByMetric: Partial<Record<HistoryMetric, UsageHistoryPoint[]>>
}>()

const { foreground, mutedForeground, border } = useThemeColors()

/** One dataset spec per tab: which metric(s), its label, and its token color. */
const DATASET_SPECS: Record<TimeSeriesTab, { metric: HistoryMetric; label: string; color: string }[]> = {
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

/**
 * canonicalBuckets is the union of every active dataset's bucket keys,
 * sorted. The "tokens" tab fetches tokin and tokout as two separate HTTP
 * requests (history.ts's metricsForTab) — not one atomic call — so a
 * window rollover landing between the two requests can leave one series
 * one bucket ahead of the other. Zipping the two arrays positionally
 * would silently mis-stack that refresh's chart. Bucket strings are
 * fixed-width, zero-padded digit strings (limits.go's windowKey: hour
 * "2006010215", day "20060102", month "200601"), so a plain lexicographic
 * sort is also a chronological sort — no date parsing needed to align
 * them.
 */
const canonicalBuckets = computed<string[]>(() => {
  const specs = DATASET_SPECS[props.tab]
  const bucketSet = new Set<string>()
  for (const spec of specs) {
    for (const p of props.seriesByMetric[spec.metric] ?? []) bucketSet.add(p.bucket)
  }
  return Array.from(bucketSet).sort()
})

const labels = computed<string[]>(() => canonicalBuckets.value.map((b) => formatBucketLabel(b, props.window)))

/** resolveToken reads one of main.css's --chart-* custom properties from :root, live — so a chart already on screen repaints on a `prefers-color-scheme` flip exactly like useThemeColors does for axis/legend text. */
function resolveToken(cssVar: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(cssVar).trim()
}

const chartData = computed<ChartData<'bar'>>(() => {
  // Touch the theme refs so this computed re-runs (and re-reads the CSS
  // custom properties via resolveToken) on a `prefers-color-scheme` flip —
  // resolveToken's own getComputedStyle read is not itself reactive.
  void foreground.value
  const buckets = canonicalBuckets.value
  return {
    labels: labels.value,
    datasets: DATASET_SPECS[props.tab].map((spec) => {
      // Map<bucket, value> per series, so each canonical bucket looks up
      // its own value rather than assuming index i means the same bucket
      // across every dataset (the mis-stacking canonicalBuckets exists to
      // avoid — see its own doc comment). A bucket this series has no
      // point for reads as 0, matching the zero-based axis: a genuinely
      // missing bucket (not yet reported by the store) is indistinguishable
      // from zero usage, which is the honest reading for a bar chart.
      const byBucket = new Map((props.seriesByMetric[spec.metric] ?? []).map((p) => [p.bucket, p.value]))
      return {
        label: spec.label,
        backgroundColor: resolveToken(spec.color),
        data: buckets.map((b) => toDisplayValue(spec.metric, byBucket.get(b) ?? 0)),
      }
    }),
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
        // Axis ticks stay compact (feature v0.23 addendum) — no room for
        // the exact figure on an axis label; the tooltip below carries
        // it. Cost keeps its existing money format, unaffected: a
        // dollar figure is not a "count" this formatter is meant for.
        callback: (value) => (props.tab === 'cost' ? `$${value}` : formatCompactCount(Number(value))),
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
        // Tooltip shows the compact form WITH the precise raw value
        // alongside (feature v0.23 addendum) — e.g. "Tokens in: 1.23M
        // (1,234,567)" — since a tooltip, unlike an axis tick, has room
        // for both and precision must never be lost. Cost stays its
        // existing $-with-4-decimals format, unchanged.
        label: (ctx) => {
          const value = ctx.parsed.y ?? 0
          if (props.tab === 'cost') return `${ctx.dataset.label}: $${value.toFixed(4)}`
          return `${ctx.dataset.label}: ${formatCompactCount(value)} (${formatExactInt(value)})`
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
