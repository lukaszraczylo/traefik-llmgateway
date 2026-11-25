<script setup lang="ts" generic="T extends object">
import { DataTable } from '@perfect-design/ui'
import type { DataTableCellSlotProps } from '@perfect-design/ui'

import LimitBadge from '@/widgets/LimitBadge.vue'
import ShareCell from '@/widgets/ShareCell.vue'
import type { WbColumn } from '@/lib/wb-columns'

type RowId = string | number

defineProps<{
  columns: WbColumn<T>[]
  rows: T[]
  rowKey?: keyof T | ((row: T, index: number) => RowId)
  label: string
  loading?: boolean
}>()
const activeKey = defineModel<RowId | null | undefined>('activeKey')
const emit = defineEmits<{ 'row-activate': [row: T, key: RowId] }>()

function num(v: unknown): string {
  return typeof v === 'number' && Number.isFinite(v) ? v.toLocaleString('en-US') : v == null || v === '' ? '—' : String(v)
}
</script>

<template>
  <DataTable
    v-model:active-key="activeKey"
    class="wb-table"
    density="compact"
    size="sm"
    sticky-header
    layout="cards"
    :columns="columns"
    :rows="rows"
    :row-key="rowKey"
    :label="label"
    :loading="loading"
    @row-activate="(row, key) => emit('row-activate', row, key)"
  >
    <template v-for="col in columns" :key="col.key" #[`cell-${col.key}`]="p: DataTableCellSlotProps<T>">
      <span v-if="col.kind === 'number'" data-numeric class="wb-num"><slot :name="`cell-${col.key}`" v-bind="p">{{ num(p.value) }}</slot></span>
      <slot v-else-if="col.kind === 'share'" :name="`cell-${col.key}`" v-bind="p"><ShareCell :value="p.value as number" /></slot>
      <slot v-else-if="col.kind === 'limit'" :name="`cell-${col.key}`" v-bind="p"><LimitBadge :limit="p.value as number" /></slot>
      <slot v-else :name="`cell-${col.key}`" v-bind="p">{{ p.value ?? '—' }}</slot>
    </template>
    <template #empty><slot name="empty" /></template>
  </DataTable>
</template>
