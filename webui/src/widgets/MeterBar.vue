<script setup lang="ts">
import { StatBar } from '@perfect-design/ui'
import { computed } from 'vue'

import { formatCompactCount } from '@/lib/format'
import type { Meter } from '@/lib/meters'

const props = defineProps<{ meter: Meter }>()

function fmt(n: number): string {
  return Number.isInteger(n) || n >= 10 ? formatCompactCount(Math.round(n)) : String(Number(n.toFixed(1)))
}

const pacing = computed(() => props.meter.markers.find((m) => m.tone === 'warning'))
const warnAt = computed(() => (pacing.value ? pacing.value.value / props.meter.max : 0.8))
const valueText = computed(() => `${fmt(props.meter.value)} / ${fmt(props.meter.max)}`)
const markers = computed(() => props.meter.markers.map((m) => ({ ...m, valueText: fmt(m.value) })))
</script>

<template>
  <div class="flex flex-col gap-1" data-testid="meter">
    <StatBar :label="meter.label" :value="meter.value" :max="meter.max" :value-text="valueText" tone="auto" :warn-at="warnAt" :danger-at="1" size="md" :markers="markers" />
    <p class="text-muted-foreground m-0 flex flex-wrap gap-x-4 text-xs">
      <span v-for="m in markers" :key="m.label" data-testid="meter-marker">{{ m.label }} <span class="font-mono tabular-nums">{{ m.valueText }}</span></span>
    </p>
  </div>
</template>
