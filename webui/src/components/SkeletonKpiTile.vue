<script setup lang="ts">
import { Card, CardContent } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'

/**
 * SkeletonKpiTile mirrors KpiTile.vue's own Card/CardContent structure
 * and spacing exactly — label, value, and delta lines at the same
 * heights — so a KPI grid swapping this in for real tiles on first load
 * causes no layout shift once the real values arrive.
 */
</script>

<template>
  <Card>
    <CardContent class="flex flex-col gap-1 px-4 py-3">
      <Skeleton class="h-3 w-20" />
      <Skeleton class="h-7 w-16" />
      <Skeleton class="h-3 w-28" />
    </CardContent>
  </Card>
</template>
