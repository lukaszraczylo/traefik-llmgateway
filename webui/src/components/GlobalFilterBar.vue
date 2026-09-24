<script setup lang="ts">
import { faArrowRightArrowLeft } from '@fortawesome/free-solid-svg-icons'
import { computed, ref, watch } from 'vue'

import LabeledSelect from '@/components/LabeledSelect.vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { SCOPE_PATTERN } from '@/lib/hash-state'
import { RANGE_KEYS } from '@/lib/range'
import type { RangeKey } from '@/lib/range'
import { DEFAULT_SCOPE, useFiltersStore } from '@/stores/filters'

/** RANGE_LABEL is the range Select's display text per lib/range.ts's RangeKey — kept here (not lib/range.ts) since it is pure presentation, not logic a store or another page's fetch needs. */
const RANGE_LABEL: Record<RangeKey, string> = {
  '24h': 'Last 24 hours',
  '48h': 'Last 48 hours',
  '7d': 'Last 7 days',
  '14d': 'Last 14 days',
  '30d': 'Last 30 days',
  '3mo': 'Last 3 months',
  '6mo': 'Last 6 months',
  '12mo': 'Last 12 months',
}
const RANGE_OPTIONS = RANGE_KEYS.map((key) => ({ value: key, label: RANGE_LABEL[key] }))

/**
 * GlobalFilterBar (redesign-plan.md section 3.1) is the shell's one
 * control surface for the three global filters (stores/filters.ts):
 * time range, "vs previous period" comparison, and an optional
 * group/user/provider scope narrowing. Every page reads the SAME filters
 * store; this bar is mounted once, in App.vue's header, not per-page.
 */
const filters = useFiltersStore()

function onRangeUpdate(value: unknown): void {
  if (typeof value !== 'string') return
  filters.setFilters({ range: value as RangeKey })
}

/**
 * toggleCmp (P3 item 19): turning the comparison OFF is always allowed,
 * even when the CURRENT range does not support one — a `cmp=prev` value
 * restored from the hash (e.g. loaded directly onto a 48h/30d/12mo range,
 * where 2*span exceeds the retention ceiling) used to leave the button
 * permanently pressed-and-disabled, with no way to turn it off short of
 * changing the range first. filters.cmpSpec.requested already gates the
 * actual fetch on availability (stores/filters.ts's own doc comment), so
 * this was purely a stuck AFFORDANCE, never a bad request. Turning it ON
 * still requires availability — there is nothing to compare against
 * otherwise.
 */
function toggleCmp(): void {
  if (filters.cmp === 'prev') {
    filters.setFilters({ cmp: 'none' })
    return
  }
  if (!filters.cmpSpec.available) return
  filters.setFilters({ cmp: 'prev' })
}

/** cmpDisabled only blocks turning the toggle ON when unavailable — turning an already-on comparison OFF is never blocked (toggleCmp's own doc comment). */
const cmpDisabled = computed<boolean>(() => filters.cmp !== 'prev' && !filters.cmpSpec.available)

const cmpTitle = computed<string>(() => {
  if (filters.cmp === 'prev' && !filters.cmpSpec.available) {
    return `Comparison unavailable for this range (${filters.cmpSpec.reason}) — click to turn off`
  }
  if (filters.cmpSpec.available) return 'Compare against the previous equal-length period'
  return `Comparison unavailable for this range: ${filters.cmpSpec.reason}`
})

// scopeDraft is a local editing buffer, not bound straight to
// filters.scope: a partially-typed value ("group:") must not spam
// setFilters/refetch every store on every keystroke, and an invalid
// commit (onBlur/Enter, below) must revert the FIELD, not the store —
// the store keeps whatever scope was last valid.
const scopeDraft = ref(filters.scope)
watch(
  () => filters.scope,
  (scope) => {
    scopeDraft.value = scope
  },
)

function commitScope(): void {
  const value = scopeDraft.value.trim() || DEFAULT_SCOPE
  if (SCOPE_PATTERN.test(value)) {
    filters.setFilters({ scope: value })
  } else {
    // Invalid — revert the field to the store's own current value rather than keeping the bad text on screen.
    scopeDraft.value = filters.scope
  }
}
</script>

<template>
  <div class="flex flex-wrap items-end gap-3">
    <LabeledSelect label="Range" :model-value="filters.range" :options="RANGE_OPTIONS" trigger-class="w-36" @update:model-value="onRangeUpdate" />

    <Button
      type="button"
      variant="outline"
      :aria-pressed="filters.cmp === 'prev'"
      :disabled="cmpDisabled"
      :title="cmpTitle"
      @click="toggleCmp"
    >
      <FontAwesomeIcon :icon="faArrowRightArrowLeft" class="size-3.5" aria-hidden="true" />
      vs previous period
    </Button>

    <div class="flex flex-col gap-1">
      <label for="global-filter-scope" class="text-xs font-medium text-muted-foreground">Scope</label>
      <Input
        id="global-filter-scope"
        v-model="scopeDraft"
        class="w-48"
        placeholder="all"
        title="all, group:<name>, user:<name>, or provider:<name>"
        @keyup.enter="commitScope"
        @blur="commitScope"
      />
    </div>
  </div>
</template>
