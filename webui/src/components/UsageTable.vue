<script setup lang="ts">
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
}>()

const columns = computed(() => usageColumns(props.idLabel, props.secondaryColumnLabel, props.secondaryValue))
</script>

<template>
  <DataTable
    :columns="columns"
    :data="entries"
    :row-class="(entry) => (entry.storeDown ? 'bg-destructive/10' : undefined)"
  />
</template>
