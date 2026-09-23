<script setup lang="ts">
import type { HTMLAttributes } from 'vue'

import { cn } from '@/lib/utils'

/**
 * Skeleton is shadcn-vue's own loading-placeholder primitive
 * (shadcn-vue.com/docs/components/skeleton): a plain pulsing block, sized
 * by whatever `class` the caller passes (e.g. `h-4 w-24`). Every composed
 * skeleton in this panel (SkeletonKpiTile, SkeletonTable, SkeletonChart,
 * SkeletonList) is built from one or more of these, each sized to match
 * the final layout's own dimensions so data arriving causes no layout
 * shift (states-plan.md item 1). `motion-reduce:animate-none` honors
 * prefers-reduced-motion instead of pulsing regardless.
 */
const props = defineProps<{ class?: HTMLAttributes['class'] }>()
</script>

<template>
  <div data-slot="skeleton" :class="cn('animate-pulse rounded-md bg-muted motion-reduce:animate-none', props.class)" />
</template>
