<script setup lang="ts">
import { Skeleton } from '@/components/ui/skeleton'

/**
 * SkeletonList mirrors TopList.vue's own ranked-row shape (rank number,
 * label, right-aligned value) — `rows` pulsing rows, defaulting to 5 to
 * match "top 5" lists, so a list card swapping this in for its first load
 * causes no layout shift once real rows arrive.
 */
withDefaults(defineProps<{ rows?: number }>(), { rows: 5 })
</script>

<template>
  <ol class="flex flex-col gap-1.5">
    <li v-for="r in rows" :key="r" class="flex items-center justify-between gap-3">
      <span class="flex min-w-0 flex-1 items-center gap-2">
        <Skeleton class="h-3 w-4 shrink-0" />
        <Skeleton class="h-4 w-full max-w-40" />
      </span>
      <Skeleton class="h-4 w-12 shrink-0" />
    </li>
  </ol>
</template>
