<script setup lang="ts">
import type { IconDefinition } from '@fortawesome/free-solid-svg-icons'

import EmptyState from '@/components/EmptyState.vue'
import ErrorState from '@/components/ErrorState.vue'
import SkeletonChart from '@/components/SkeletonChart.vue'
import SkeletonList from '@/components/SkeletonList.vue'
import SkeletonTable from '@/components/SkeletonTable.vue'
import type { LoadState } from '@/lib/load-state'

/**
 * LoadStateView (reuse-audit.md F2) is the ONE skeleton/error/(empty)/ready
 * state branch for a list/table card with no Card wrapper of its own
 * opinions (ChartCard.vue is this same idea for a chart card) — TargetCallers,
 * HomePage's Latest-events card, and PricingHealthTable each used to
 * hand-roll this identical `v-if="state==='skeleton'" / v-else-if=
 * "state==='error'" / v-else-if="<own empty boolean>" / v-else` chain.
 * `state` decides skeleton/error/ready; the 'empty' branch is opt-in only —
 * it renders when the caller passes `empty: true` explicitly, never merely
 * because `state === 'empty'`. Every pre-existing call site already derived
 * its own "nothing to show" from its own data length (e.g. a settled fetch
 * that came back with zero rows), exactly like their old hand-rolled
 * chains did — `state === 'empty'` alone (no data ever loaded, not
 * loading, no error) must fall through to the ready branch (`v-else`), so
 * a caller that reaches this component only for its own
 * table/list-with-empty-message body (e.g. UsageTable, DataTable) keeps
 * rendering that body — and that body's own empty-message — unchanged,
 * instead of losing it to a generic "Nothing to show." here. `#empty`
 * overrides the default EmptyState entirely (TryLongerRangeButton, a
 * custom icon) when a plain title/description is not enough.
 */
const props = withDefaults(
  defineProps<{
    state: LoadState
    error?: string
    onRetry?: () => void | Promise<void>
    skeleton: 'table' | 'list' | 'chart'
    rows?: number
    cols?: number
    /** The ONLY switch for the empty branch — see this component's own doc comment. `state === 'empty'` alone never triggers it. */
    empty?: boolean
    emptyIcon?: IconDefinition
    emptyTitle?: string
    emptyDescription?: string
  }>(),
  {
    error: '',
    onRetry: undefined,
    rows: 5,
    cols: 4,
    empty: false,
    emptyIcon: undefined,
    emptyTitle: 'Nothing to show.',
    emptyDescription: undefined,
  },
)
</script>

<template>
  <SkeletonTable v-if="state === 'skeleton' && skeleton === 'table'" :rows="rows" :cols="cols" />
  <SkeletonList v-else-if="state === 'skeleton' && skeleton === 'list'" :rows="rows" />
  <SkeletonChart v-else-if="state === 'skeleton'" />
  <ErrorState v-else-if="state === 'error'" :message="error" :on-retry="onRetry" />
  <slot v-else-if="empty" name="empty">
    <EmptyState :icon="emptyIcon" :title="emptyTitle" :description="emptyDescription" />
  </slot>
  <slot v-else />
</template>
