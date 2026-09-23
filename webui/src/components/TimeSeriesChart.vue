<script setup lang="ts">
import type { ChartData, ChartOptions } from 'chart.js'
import { computed, useId } from 'vue'
import { Line } from 'vue-chartjs'

import '@/lib/chart-setup'
import { useThemeColors } from '@/composables/useThemeColors'
import { formatExactInt } from '@/lib/format'

/** One line-chart series: a label, its values (index-aligned with the `labels` prop below), and which main.css --chart-* token colors it. */
export interface TimeSeriesDataset {
  label: string
  data: number[]
  /** A main.css --chart-* custom property name (e.g. '--chart-requests') — resolved live via getComputedStyle (this component's own resolveToken, below), so a chart already on screen repaints on a `prefers-color-scheme` flip. */
  color: string
}

/**
 * TimeSeriesComparisonDataset is one `comparisonDatasets` entry — a plain
 * TimeSeriesDataset plus an OPT-IN `labelSuffix`, appended to `label`
 * verbatim (undefined/empty appends nothing). Callers decide per dataset,
 * not this component for all of them: SpendPage.vue's own "vs previous
 * period" comparison sets `labelSuffix: ' (previous period)'` (Q5), while
 * BurnDownChart.vue's flat budget reference line — also rendered through
 * this same `comparisonDatasets` prop, since a budget ceiling is exactly
 * the "reference overlay, not a component of the current total" this prop
 * already means — sets none, so its line reads plain "Budget", never the
 * misleading "Budget (previous period)" (P2 item 5: a fixed
 * budget line is not a previous-period ANYTHING).
 */
export interface TimeSeriesComparisonDataset extends TimeSeriesDataset {
  labelSuffix?: string
}

/**
 * TimeSeriesChart (redesign-plan.md section 3.4) is the redesign's shared
 * line-chart primitive — Spend's cost breakdown (stacked), Reliability's
 * per-provider error rate (overlaid, not stacked), and any other
 * multi-series-over-time view build on this ONE component rather than
 * each growing its own vue-chartjs wiring. It is the only chart component
 * in this panel — lib/chart-setup.ts registers exactly the Line/Filler
 * pieces it uses, nothing more.
 *
 * `comparisonDatasets` (Q5/section 3.1 "vs previous period", and
 * BurnDownChart.vue's budget reference line) renders as a SEPARATE set of
 * dashed lines, one per entry, matched to its solid counterpart by array
 * index and color — never stacked, never filled, regardless of `stacked`,
 * since a comparison line is a reference overlay, not a component of the
 * current total.
 */
const props = withDefaults(
  defineProps<{
    labels: string[]
    datasets: TimeSeriesDataset[]
    /** A reference/overlay series, one dashed line per entry — omitted/empty draws no comparison. Each entry's own `labelSuffix` (TimeSeriesComparisonDataset) decides whether its label reads "vs previous period" or plain. */
    comparisonDatasets?: TimeSeriesComparisonDataset[]
    /** Stack every dataset into one filled total (Spend's cost breakdown) instead of overlaying them as independent lines (Reliability's per-provider error rate). */
    stacked?: boolean
    /** Formats a raw value for the tooltip and y-axis ticks — defaults to formatExactInt (lib/format.ts). Cost callers pass formatCost, a ratio series a percent formatter, and so on. */
    valueFormatter?: (value: number) => string
    /**
     * ariaLabel is this chart's accessible name (P3 item 16) — a bare
     * `<canvas>` has no text alternative at all for a screen reader, so
     * every call site supplies a short description of what the chart
     * shows (e.g. "Spend by model over time"). Required, not optional:
     * there is no safe generic default that would be meaningful across
     * this component's very different callers (Spend's breakdown,
     * BurnDownChart's burn-down, Reliability's five series charts,
     * UserDetail's two per-user charts).
     */
    ariaLabel: string
  }>(),
  {
    comparisonDatasets: () => [],
    stacked: false,
    valueFormatter: formatExactInt,
  },
)

const descId = `timeseries-chart-desc-${useId()}`

/**
 * srSummary is the sr-only fallback's own text content (P3 item 16's fix
 * instruction: "an sr-only summary (latest value, total)") — one short
 * sentence per dataset, latest bucket's value and the sum across the
 * whole range, run through the SAME valueFormatter the visible tooltip/
 * axis use, so a screen reader hears figures in the identical units a
 * sighted reader sees. Comparison datasets are left out: they are a
 * reference overlay of the SAME primary series shifted back a period,
 * not new information this summary needs to repeat.
 */
const srSummary = computed<string>(() => {
  if (props.datasets.length === 0 || props.labels.length === 0) return 'No data for this range.'
  return props.datasets
    .map((d) => {
      const latest = d.data.length > 0 ? d.data[d.data.length - 1] : 0
      const total = d.data.reduce((sum, v) => sum + v, 0)
      return `${d.label}: latest ${props.valueFormatter(latest)}, total ${props.valueFormatter(total)} over ${props.labels.length} buckets.`
    })
    .join(' ')
})

const { foreground, mutedForeground, border } = useThemeColors()

/** resolveToken reads one of main.css's --chart-* custom properties from :root, live — see this component's own doc comment on `color` above. */
function resolveToken(cssVar: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(cssVar).trim()
}

const chartData = computed<ChartData<'line'>>(() => {
  // Touch the theme ref so this computed re-runs (and re-reads the CSS
  // custom properties via resolveToken) on a `prefers-color-scheme` flip.
  void foreground.value
  const solid = props.datasets.map((d) => {
    const color = resolveToken(d.color)
    return {
      label: d.label,
      data: d.data,
      borderColor: color,
      backgroundColor: color,
      fill: props.stacked,
      stack: props.stacked ? 'main' : undefined,
      tension: 0.2,
      pointRadius: 2,
      borderWidth: 2,
    }
  })
  const comparison = props.comparisonDatasets.map((d) => {
    const color = resolveToken(d.color)
    return {
      label: `${d.label}${d.labelSuffix ?? ''}`,
      data: d.data,
      borderColor: color,
      backgroundColor: 'transparent',
      borderDash: [6, 4],
      fill: false,
      tension: 0.2,
      pointRadius: 0,
      borderWidth: 1.5,
    }
  })
  return { labels: props.labels, datasets: [...solid, ...comparison] }
})

const chartOptions = computed<ChartOptions<'line'>>(() => ({
  responsive: true,
  maintainAspectRatio: false,
  animation: false,
  interaction: { mode: 'index', intersect: false },
  scales: {
    x: {
      stacked: props.stacked,
      grid: { color: border.value, display: false },
      ticks: { color: mutedForeground.value },
    },
    y: {
      // Zero-based, honest axis (dataviz rule) — never a truncated y-scale that would exaggerate a small change.
      stacked: props.stacked,
      beginAtZero: true,
      grid: { color: border.value },
      ticks: { color: mutedForeground.value, callback: (value) => props.valueFormatter(Number(value)) },
    },
  },
  plugins: {
    legend: {
      display: props.datasets.length > 1 || props.comparisonDatasets.length > 0,
      labels: { color: foreground.value },
    },
    tooltip: {
      callbacks: {
        label: (ctx) => `${ctx.dataset.label}: ${props.valueFormatter(ctx.parsed.y ?? 0)}`,
      },
    },
  },
}))
</script>

<template>
  <div class="h-72 w-full" role="img" :aria-label="ariaLabel" :aria-describedby="descId">
    <Line :data="chartData" :options="chartOptions" />
    <p :id="descId" class="sr-only">{{ srSummary }}</p>
  </div>
</template>
