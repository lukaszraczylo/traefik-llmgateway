<script setup lang="ts">
import { computed } from 'vue'

import { Badge } from '@/components/ui/badge'
import { dayRateTitle, formatRatePercent, minuteRateTitle, providerRateStatus, providerSuccessRate } from '@/lib/provider-rate'

/**
 * ProviderRateBadge is the Providers tab's success-rate badge (Feature A,
 * v0.22) — reused for both a provider's own row (ProvidersView.vue's
 * accordion trigger, always shown) and a single degraded model beside its
 * ModelChip (ProvidersView.vue's accordion content, shown only when
 * isModelDegraded). attemptsMinute/failuresMinute are OPTIONAL (SHOULD-2,
 * v0.22 review round): the provider-row call site passes them (admin.go's
 * adminProviderView carries minute counters); the per-model call site
 * does not (adminModelRateView no longer does — a model scope's minute
 * window is never fetched at all, see lib/provider-rate.ts's own
 * dayRateTitle). Their absence, not a passed-through 0, is what tells
 * this component to fall back to a day-window-only detail sentence — a
 * real 0-attempts minute and "no minute data at all" are different
 * things, and treating them as the identical "0" would silently claim a
 * live "0 attempts in the last minute" reading a model badge never
 * actually measured.
 */
const props = defineProps<{
  attemptsDay: number
  failuresDay: number
  attemptsMinute?: number
  failuresMinute?: number
}>()

const rate = computed(() => providerSuccessRate(props.attemptsDay, props.failuresDay))
const status = computed(() => providerRateStatus(rate.value))

const label = computed(() => (status.value === 'no-traffic' ? 'no traffic' : formatRatePercent(rate.value as number)))

/**
 * detail carries the "right now" (provider) or "today" (model) counts —
 * the spec's minute-window requirement where that data exists at all
 * (minuteRateTitle), falling back to dayRateTitle when it does not. Used
 * as BOTH the badge's title (mouse hover) and, via the sr-only span in
 * the template, its accessible name for keyboard/screen-reader users
 * (folded review minor, v0.22 review round: title alone is mouse-only).
 *
 * Review fix (v0.23 round): this used to sit directly on `:aria-label`
 * on the Badge/`<span>` element, which axe flags as `aria-prohibited-
 * attr` — a `<span>`'s implicit role ("generic") does not support name
 * computation. See CompactNumber.vue's own doc comment for the identical
 * fix and the shared visible-abbreviation-plus-sr-only-full-text
 * pattern this component now uses too.
 */
const detail = computed(() =>
  props.attemptsMinute === undefined || props.failuresMinute === undefined
    ? dayRateTitle(props.attemptsDay, props.failuresDay)
    : minuteRateTitle(props.attemptsMinute, props.failuresMinute),
)

// Badge has no dedicated "amber"/"muted" variant (ui/badge/index.ts) —
// severe reuses the built-in `destructive` variant exactly like every
// other error state in this panel (e.g. ProvidersView's own lastErr
// icon); healthy/no-traffic reuse `secondary`/`outline`, the panel's
// existing "quiet" idioms (ProvidersView's provider-type badge is
// `variant="secondary"`); degraded borrows the semantic --status-warn
// token (main.css — MUST-1, v0.22 review round: --chart-tokens-out, this
// component's original choice, measures ~2.22:1 against --card in light
// mode, a WCAG 1.4.3 text-contrast failure on exactly the "something's
// wrong" tier; --status-warn is tuned for body-text contrast instead,
// ~5.06:1 light / ~8.97:1 dark).
const variant = computed(() => (status.value === 'severe' ? 'destructive' : status.value === 'healthy' ? 'secondary' : 'outline'))
const extraClass = computed(() => {
  if (status.value === 'degraded') return 'border-status-warn/40 text-status-warn'
  if (status.value === 'no-traffic') return 'text-muted-foreground'
  return ''
})
</script>

<template>
  <Badge as="span" :variant="variant" :class="extraClass" class="font-normal tabular-nums" :title="detail">
    <span aria-hidden="true">{{ label }}</span>
    <span class="sr-only">{{ detail }}</span>
  </Badge>
</template>
