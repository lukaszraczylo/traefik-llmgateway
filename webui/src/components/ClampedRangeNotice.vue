<script setup lang="ts">
import { computed } from 'vue'

import { Alert, AlertDescription } from '@/components/ui/alert'
import { dayOrMonthClampNotice } from '@/lib/range'
import type { HistoryWindow } from '@/types/api'

/**
 * ClampedRangeNotice (N3 fix, verify-redesign-final.md) is the ONE shared
 * "this view silently clamped your range" banner for every day/month-only
 * endpoint reached through lib/range.ts's dayOrMonthWindow:
 *   - UserDetail.vue's "Models used" table (usermodel totals)
 *   - AttributionDrilldown.vue's usermodel level (the Spend drilldown)
 *   - TargetCallers.vue (targetcaller totals)
 * An hour-resolution global range (24h/48h) has no day/month equivalent
 * for any of these — dayOrMonthWindow silently substitutes a fixed
 * 7-day day-window instead, and this renders nothing (dayOrMonthClampNotice
 * returns null) unless that substitution actually happened, so a
 * day/month-window reader never sees a spurious banner.
 *
 * Distinct from ReliabilityPage.vue's own inline month-clamp Alert — a
 * DIFFERENT clamp (hour/day -> a fixed 30-day window, stores/
 * reliability.ts's hourOrDayWindow) that stays its own local computed,
 * not this component.
 */
const props = defineProps<{
  window: HistoryWindow
  span: number
}>()

const note = computed<string | null>(() => dayOrMonthClampNotice(props.window, props.span))
</script>

<template>
  <Alert v-if="note" variant="warn">
    <AlertDescription>{{ note }}</AlertDescription>
  </Alert>
</template>
