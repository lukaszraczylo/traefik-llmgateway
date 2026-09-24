<script setup lang="ts">
import UsageBar from '@/components/UsageBar.vue'
import { BUDGET_RATIO_LABEL, budgetValueText } from '@/lib/usage-bars'
import type { BudgetRatio } from '@/lib/usage-bars'

/**
 * BudgetRatioBars (reuse-audit.md F7) is the ONE budget-ratio bar row —
 * one labelled UsageBar per configured limit — UserDetail.vue's Headroom
 * card and ConsumerDirectory.vue's group accordion both hand-rolled: a
 * label (BUDGET_RATIO_LABEL) beside each lib/usage-bars.ts budgetRatios()
 * entry, its UsageBar sized to that ratio, and its "used / limit" text
 * (the shared budgetValueText).
 */
defineProps<{
  ratios: BudgetRatio[]
}>()
</script>

<template>
  <div class="flex flex-wrap items-center gap-x-4 gap-y-1.5">
    <span v-for="ratio in ratios" :key="ratio.id" class="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
      {{ BUDGET_RATIO_LABEL[ratio.id] }}
      <UsageBar :ratio="ratio.ratio" :label="BUDGET_RATIO_LABEL[ratio.id]" :value-text="budgetValueText(ratio)" />
    </span>
  </div>
</template>
