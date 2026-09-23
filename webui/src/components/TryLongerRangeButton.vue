<script setup lang="ts">
import { computed } from 'vue'

import { Button } from '@/components/ui/button'
import { RANGE_KEYS } from '@/lib/range'
import { useFiltersStore } from '@/stores/filters'

/**
 * TryLongerRangeButton is the shared action every "No traffic in this
 * range" empty state offers (states-plan.md item 2: Spend/Models/Home
 * lists) — steps the global range to the NEXT LONGER preset in
 * RANGE_KEYS (verify-ui-states.md #7 fix), via stores/filters.ts, rather
 * than each empty state re-deriving its own inline click handler
 * (vue.md: "if you've written it twice, you owe an abstraction" — this
 * one appears on Home's two TopLists, the Spend page's Attribution
 * drilldown, and PricingHealthTable).
 *
 * Previously this always jumped straight to a fixed '30d' — on 3mo/6mo/
 * 12mo (lib/range.ts's RANGE_KEYS, already LONGER than 30d) that
 * SHORTENED the range instead of lengthening it, and did nothing at all
 * when already on 30d itself. Stepping to `RANGE_KEYS[currentIndex + 1]`
 * is always strictly longer than the current range, and the button hides
 * itself entirely at '12mo' (the longest preset — there is nothing
 * longer left to try).
 */
const filters = useFiltersStore()

const nextLongerRange = computed<string | null>(() => {
  const index = RANGE_KEYS.indexOf(filters.range)
  const next = RANGE_KEYS[index + 1]
  return next ?? null
})

function onClick(): void {
  if (nextLongerRange.value) filters.setFilters({ range: nextLongerRange.value as (typeof RANGE_KEYS)[number] })
}
</script>

<template>
  <Button v-if="nextLongerRange" type="button" variant="outline" size="sm" @click="onClick">Try a longer range</Button>
</template>
