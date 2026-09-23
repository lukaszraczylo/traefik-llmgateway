<script setup lang="ts">
import { computed } from 'vue'

import { Card, CardContent } from '@/components/ui/card'
import type { KpiTier } from '@/lib/kpi'
import type { PageId } from '@/lib/pages'
import { useNavStore } from '@/stores/nav'

/**
 * KpiTile (redesign-plan.md section 3.4) is the Home page's one KPI-tile
 * shape: a label, a headline value, an optional delta/detail line, and a
 * tier (lib/kpi.ts) that colors the value text — 'ok' (default
 * foreground), 'warn' (--status-warn), 'critical' (--destructive), the
 * same two semantic tokens the rest of this panel's badges/bars already
 * use, never a fourth ad-hoc color.
 *
 * Optionally a link: when `to` is set, the whole tile becomes a real
 * `<button>` that calls nav.goTo(to.page, to.params) — a budget tile
 * jumps to Spend, an open-breakers tile jumps to Reliability, and so on.
 * Without `to`, it renders as a plain, non-interactive Card (a tile with
 * nowhere useful to send the reader, e.g. "requests/min", stays inert
 * rather than a button that does nothing).
 */
const props = withDefaults(
  defineProps<{
    label: string
    value: string
    delta?: string
    tier?: KpiTier
    to?: { page: PageId; params?: Record<string, string> }
  }>(),
  { tier: 'ok' },
)

const nav = useNavStore()

const valueClass = computed<string>(() => {
  switch (props.tier) {
    case 'warn':
      return 'text-status-warn'
    case 'critical':
      return 'text-destructive'
    default:
      return 'text-foreground'
  }
})

function onClick(): void {
  if (!props.to) return
  nav.goTo(props.to.page, props.to.params ?? {})
}
</script>

<template>
  <component
    :is="to ? 'button' : 'div'"
    :type="to ? 'button' : undefined"
    class="block w-full rounded-xl text-left"
    :class="to ? 'cursor-pointer transition-colors hover:opacity-90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50' : undefined"
    @click="onClick"
  >
    <Card>
      <CardContent class="flex flex-col gap-1 px-4 py-3">
        <span class="text-xs font-medium text-muted-foreground">{{ label }}</span>
        <span class="text-2xl font-semibold tracking-tight tabular-nums" :class="valueClass">{{ value }}</span>
        <span v-if="delta" class="text-xs text-muted-foreground">{{ delta }}</span>
      </CardContent>
    </Card>
  </component>
</template>
