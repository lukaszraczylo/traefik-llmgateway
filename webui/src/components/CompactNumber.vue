<script setup lang="ts">
import { computed } from 'vue'

import { formatCompactCount, formatExactInt } from '@/lib/format'

/**
 * CompactNumber renders a large raw token/request count in its compact
 * SI-style form ("1.23M") while carrying the exact value via both
 * `title` (mouse hover) and `aria-label` (keyboard/screen-reader parity
 * — the same established pattern ProviderRateBadge.vue and ModelChip.vue
 * already apply to their own hover detail). Used anywhere a usage count
 * renders: DataTable numeric columns (usage-columns.ts, target-
 * columns.ts, via `h(CompactNumber, { value })`), the Usage tab's Total
 * card, and a group accordion's trigger/detail stat grid (UsageView.vue).
 *
 * Cost figures are NOT this component's concern — formatCost already
 * shows full 4-decimal precision, so there is nothing to compact or
 * disambiguate with a title there; only token/request COUNTS use this.
 */
const props = defineProps<{ value: number }>()

const compact = computed(() => formatCompactCount(props.value))
const exact = computed(() => formatExactInt(props.value))
</script>

<template>
  <span :title="exact" :aria-label="exact">{{ compact }}</span>
</template>
