<script setup lang="ts">
import { computed } from 'vue'

import { Badge } from '@/components/ui/badge'
import { formatRatePercent, minuteRateTitle, providerRateStatus, providerSuccessRate } from '@/lib/provider-rate'

/**
 * ProviderRateBadge is the Providers tab's success-rate badge (Feature A,
 * v0.22) — reused for both a provider's own row (ProvidersView.vue's
 * accordion trigger, always shown) and a single degraded model beside its
 * ModelChip (ProvidersView.vue's accordion content, shown only when
 * isModelDegraded). Both call sites pass the identical four counters
 * (admin.go's adminProviderView / adminModelRateView field shape), so one
 * component covers both without a provider/model-specific prop.
 */
const props = defineProps<{
  attemptsDay: number
  failuresDay: number
  attemptsMinute: number
  failuresMinute: number
}>()

const rate = computed(() => providerSuccessRate(props.attemptsDay, props.failuresDay))
const status = computed(() => providerRateStatus(rate.value))

const label = computed(() => (status.value === 'no-traffic' ? 'no traffic' : formatRatePercent(rate.value as number)))

/** title carries the minute-window "right now" detail (spec) regardless of tier — even a healthy badge's title shows the live minute count, not just the day-window percent the badge text itself displays. */
const title = computed(() => minuteRateTitle(props.attemptsMinute, props.failuresMinute))

// Badge has no dedicated "amber"/"muted" variant (ui/badge/index.ts) —
// severe reuses the built-in `destructive` variant exactly like every
// other error state in this panel (e.g. ProvidersView's own lastErr
// icon); healthy/no-traffic reuse `secondary`/`outline`, the panel's
// existing "quiet" idioms (ProvidersView's provider-type badge is
// `variant="secondary"`); degraded is the one tier with no built-in
// variant, so it borrows the chart palette's amber token
// (--color-chart-tokens-out, main.css) the same way the Redis/Cache/Retry
// status cards above already borrow --color-chart-requests for "ok".
const variant = computed(() => (status.value === 'severe' ? 'destructive' : status.value === 'healthy' ? 'secondary' : 'outline'))
const extraClass = computed(() => {
  if (status.value === 'degraded') return 'border-chart-tokens-out/40 text-chart-tokens-out'
  if (status.value === 'no-traffic') return 'text-muted-foreground'
  return ''
})
</script>

<template>
  <Badge as="span" :variant="variant" :class="extraClass" class="font-normal tabular-nums" :title="title">
    {{ label }}
  </Badge>
</template>
