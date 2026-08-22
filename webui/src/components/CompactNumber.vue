<script setup lang="ts">
import { computed } from 'vue'

import { formatCompactCount, formatExactInt } from '@/lib/format'

/**
 * CompactNumber renders a large raw token/request count in its compact
 * SI-style form ("1.23M") while carrying the exact value via `title`
 * (mouse hover) and a visually-hidden sibling span (keyboard/screen-
 * reader parity). Used anywhere a usage count renders: DataTable numeric
 * columns (usage-columns.ts, target-columns.ts, via
 * `h(CompactNumber, { value })`), the Usage tab's Total card, and a
 * group accordion's trigger/detail stat grid (UsageView.vue).
 *
 * Review fix (folded minor, ARIA): `aria-label` on a plain `<span>` is
 * an axe `aria-prohibited-attr` violation — a `<span>`'s implicit ARIA
 * role ("generic") does not support name computation at all, so
 * `aria-label`/`aria-labelledby` there is simply invalid, not merely
 * discouraged. The visible, compact text is marked `aria-hidden="true"`
 * so assistive tech skips the abbreviated form, and a `sr-only` sibling
 * span (visually hidden, still in the accessibility tree) carries the
 * exact value instead — the same visible-abbreviation-plus-hidden-full-
 * text pattern ProviderRateBadge.vue now applies too.
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
  <span :title="exact">
    <span aria-hidden="true">{{ compact }}</span>
    <span class="sr-only">{{ exact }}</span>
  </span>
</template>
