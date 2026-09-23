<script setup lang="ts">
import { Skeleton } from '@/components/ui/skeleton'

/**
 * SkeletonChart is a fixed-aspect placeholder matching
 * TimeSeriesChart.vue's own container exactly (`h-72 w-full`) — every
 * chart in this panel (Spend's breakdown, Reliability's per-provider
 * error rate, BurnDownChart, UserDetail's per-user charts) renders inside
 * that same fixed height, so swapping this in for the first load never
 * shifts the layout once the real chart mounts.
 */
</script>

<template>
  <div class="h-72 w-full">
    <Skeleton class="h-full w-full" />
  </div>
</template>
