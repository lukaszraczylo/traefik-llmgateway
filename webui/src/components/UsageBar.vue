<script setup lang="ts">
import type { HTMLAttributes } from 'vue'
import { computed } from 'vue'

import { cn } from '@/lib/utils'
import { ratioTier } from '@/lib/usage-bars'

/**
 * UsageBar renders one BudgetRatio (lib/usage-bars.ts) as a compact
 * horizontal meter — F1, usage-vs-limit bars. A real `role="meter"`
 * (not a `<progress>` element: a meter reports a measurement against a
 * range, which is exactly a used/limit reading, where `<progress>`
 * implies a task moving toward completion) carries the accessible
 * reading via `aria-valuenow`/`aria-valuemin`/`aria-valuemax`/
 * `aria-valuetext`, so a screen reader announces the same number a
 * sighted reader sees in the fill width and the `title` tooltip.
 *
 * Used two places (usage-columns.ts's numeric cells, UsageView.vue's
 * group detail grid) — both pass a fully-computed BudgetRatio plus the
 * caller's own human-readable `valueText`, so this component has no
 * formatting opinion of its own (formatCost/formatExactInt stay the
 * caller's job, same "logic lives in lib/, not the component" split
 * every other shared component in this panel follows).
 */
const props = defineProps<{
  /** used / limit — may exceed 1 (a scope's usage window can roll past its limit before the next request is rejected). */
  ratio: number
  /** The accessible name for this meter, e.g. "req/day" or "cost/month". */
  label: string
  /** The human-readable "used of limit" text — both the `title` tooltip and `aria-valuetext`, e.g. "500 / 1,000" or "$1.20 / $5.00". */
  valueText: string
  class?: HTMLAttributes['class']
}>()

const tier = computed(() => ratioTier(props.ratio))

/**
 * widthPct clamps the visible fill (and, via valueNow below, the
 * announced aria-valuenow) to 100% — a ratio above 1.0 (over the limit)
 * still reads as a full bar, with the RED tier and valueText's own real
 * "used / limit" numbers carrying the "how far over" detail instead of an
 * overflowing bar or an aria-valuenow exceeding its own declared
 * aria-valuemax (WAI-ARIA meter role: valuenow must stay within
 * valuemin/valuemax).
 */
const widthPct = computed(() => Math.min(100, Math.max(0, props.ratio * 100)))
const valueNow = computed(() => Math.round(widthPct.value))

const TIER_FILL_CLASS: Record<ReturnType<typeof ratioTier>, string> = {
  normal: 'bg-primary',
  amber: 'bg-bar-amber',
  red: 'bg-destructive',
}
</script>

<template>
  <span
    role="meter"
    :aria-label="label"
    :aria-valuenow="valueNow"
    aria-valuemin="0"
    aria-valuemax="100"
    :aria-valuetext="valueText"
    :title="valueText"
    :class="cn('relative inline-block h-1.5 w-14 shrink-0 overflow-hidden rounded-full bg-muted align-middle', props.class)"
  >
    <span
      class="absolute inset-y-0 left-0 rounded-full"
      :class="TIER_FILL_CLASS[tier]"
      :style="{ width: `${widthPct}%` }"
    />
  </span>
</template>
