<script setup lang="ts">
import { StatBar } from '@perfect-design/ui'
import { computed } from 'vue'

const props = withDefaults(defineProps<{ value: number | null | undefined; label?: string }>(), { label: 'Share' })

const pct = computed(() => (Number.isFinite(props.value) ? Math.min(100, Math.max(0, (props.value as number) * 100)) : null))
const text = computed(() => (pct.value === null ? '—' : `${pct.value < 10 && pct.value > 0 ? pct.value.toFixed(1) : Math.round(pct.value)}%`))
</script>

<template>
  <StatBar
    class="wb-share-bar"
    :label="label"
    :value="pct ?? 0"
    :max="100"
    :value-text="text"
    tone="primary"
    size="sm"
    layout="inline"
    hide-label
  />
</template>
