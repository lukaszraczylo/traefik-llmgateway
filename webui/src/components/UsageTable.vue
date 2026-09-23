<script setup lang="ts">
import type { SortingState } from '@tanstack/vue-table'
import { computed } from 'vue'

import DataTable from '@/components/DataTable.vue'
import { usageColumns } from '@/lib/usage-columns'
import type { AdminUsageEntryView } from '@/types/api'

/**
 * UsageTable renders one scope kind's usage rows (users, or a group's
 * member users) as a sortable DataTable (shadcn-vue's DataTable pattern,
 * via the shared usageColumns() defs — vue.md: extract before you paste
 * it twice). The second column differs per kind: a user's row shows the
 * group it belongs to; a group's row shows its member count (looked up
 * from the Overview response, which is the only place member counts
 * live).
 */
const props = defineProps<{
  idLabel: string
  entries: AdminUsageEntryView[]
  secondaryColumnLabel: string
  secondaryValue: (entry: AdminUsageEntryView) => string
  /** Shown in the empty-state row when entries is empty — e.g. a filtered-search "no X match ..." message. Falls through to DataTable's own 'none' default when omitted. */
  emptyMessage?: string
  /** P1: the shared reactive clock (composables/useNow.ts) — threaded into usageColumns() below so this table's own costMonthProj column recomputes as the clock ticks, instead of freezing at whatever instant this computed first ran. */
  now: Date
  /** P6: optional controlled sort state, forwarded to DataTable's own `v-model:sorting` (see that component's identical prop doc comment) — omitted everywhere except UsageView.vue's top-level Users table, whose export must reflect the table's current sort. */
  sorting?: SortingState
}>()

const emit = defineEmits<{ 'update:sorting': [value: SortingState] }>()

const columns = computed(() => usageColumns(props.idLabel, props.secondaryColumnLabel, props.secondaryValue, props.now))
</script>

<template>
  <DataTable
    :columns="columns"
    :data="entries"
    :empty-message="emptyMessage"
    :row-class="(entry) => (entry.storeDown ? 'bg-destructive/10' : undefined)"
    :sorting="sorting"
    @update:sorting="(v) => emit('update:sorting', v)"
  />
</template>
