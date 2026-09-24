<script setup lang="ts">
import { computed } from 'vue'

import StatItem from '@/components/StatItem.vue'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { headroom, monthProgress, NOT_ENOUGH_DATA, projectMonthEnd, WILL_EXCEED_TITLE } from '@/lib/forecast'
import { formatCost } from '@/lib/format'
import type { LimitsConfig } from '@/types/api'

/**
 * CostForecastCard renders a fleet- or scope-wide cost projection — F7,
 * originally the pre-redesign Usage tab's own card, now used by both
 * SpendPage.vue (the global range's own scope, under the burn-down chart)
 * and ConsumerDirectory.vue (fleet-wide, under the Total row). All math is
 * delegated to lib/forecast.ts's pure functions; this component only
 * formats and lays the result out.
 *
 * `now` is a REQUIRED prop, not read from `new Date()` inside this
 * component (coordinator brief: "now passed as prop for determinism") —
 * every caller supplies it from its OWN composables/useNow.ts instance,
 * keeping every `Date.now()`-flavored read in this feature at the
 * boundary rather than buried inside a component, same discipline
 * lib/forecast.ts's own functions already follow by taking
 * `now`/`elapsedFraction` as plain arguments.
 *
 * P1 review fix: this used to be optional with a `withDefaults(...,
 * { now: () => new Date() })` default. Vue caches a prop's default value
 * ONCE per component instance (propsDefaults) — the very first time it is
 * read, not on every render — so a caller that omitted `now` froze this
 * card's projection at whatever instant it happened to mount, never
 * advancing again for as long as the page stayed open. `now` is now
 * required: every caller passes the ONE shared reactive clock its own
 * useNow() call returns, so there is no default left to accidentally
 * freeze.
 */
const props = defineProps<{
  /** usage.total.costPerMonthMicroUsd — fleet-wide month-to-date spend. */
  mtdMicros: number
  /** usage.total.limits — costPerMonthUSD is the only field this card reads. */
  limits?: LimitsConfig
  /** The shared reactive clock (composables/useNow.ts) — never defaulted here, see this comment block above. */
  now: Date
}>()

const elapsedFraction = computed(() => monthProgress(props.now))
const projectedMicros = computed(() => projectMonthEnd(props.mtdMicros, elapsedFraction.value))
const forecast = computed(() => headroom(projectedMicros.value, props.limits?.costPerMonthUSD))
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>Cost forecast</CardTitle>
      <CardDescription>Projected month-end spend, extrapolated from month-to-date usage (UTC calendar month).</CardDescription>
    </CardHeader>
    <CardContent class="grid grid-cols-2 gap-4 text-sm sm:grid-cols-3">
      <StatItem label="Month to date">{{ formatCost(mtdMicros) }}</StatItem>
      <StatItem
        label="Projected month-end"
        :tone="forecast.willExceed ? 'destructive' : 'default'"
        :title="forecast.willExceed ? WILL_EXCEED_TITLE : undefined"
        :hint="projectedMicros === null ? NOT_ENOUGH_DATA : undefined"
      >
        {{ projectedMicros === null ? '' : formatCost(projectedMicros) }}
      </StatItem>
      <StatItem v-if="forecast.limitMicros !== null" label="Limit">{{ formatCost(forecast.limitMicros) }}</StatItem>
    </CardContent>
  </Card>
</template>
