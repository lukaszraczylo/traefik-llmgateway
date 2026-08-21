<script setup lang="ts" generic="TData">
import type { ColumnDef, SortingState } from '@tanstack/vue-table'
import { faSort, faSortDown, faSortUp } from '@fortawesome/free-solid-svg-icons'
import { FlexRender, getCoreRowModel, getSortedRowModel, useVueTable } from '@tanstack/vue-table'
import { ref } from 'vue'

import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
  valueUpdater,
} from '@/components/ui/table'

/**
 * DataTable is the shared sortable-table shape every real tabular surface
 * in the panel uses (users usage, a group's nested member-user rows,
 * aliases) — the shadcn-vue DataTable pattern
 * (shadcn-vue.com/docs/components/data-table: useVueTable +
 * getSortedRowModel + FlexRender), factored into one reusable component
 * instead of re-writing the same table setup and sortable-header-button
 * boilerplate at every call site (vue.md: "if you've written it twice,
 * you owe an abstraction"). Each column stays a plain TanStack
 * `ColumnDef` — a string `header` label plus an optional custom `cell`
 * renderer — and this component supplies the uniform
 * clickable-header-with-asc/desc-icon chrome for every column that does
 * not explicitly set `enableSorting: false`. `meta: { align: 'right' }`
 * (declared in ui/table/utils.ts) right-aligns a numeric column's header
 * button and cell content with tabular-nums, matching this panel's
 * existing numeric-column convention.
 */
const props = defineProps<{
  columns: ColumnDef<TData, unknown>[]
  data: TData[]
  /** Extra classes for one row, keyed by its own data (e.g. a destructive tint for a storeDown entry). */
  rowClass?: (row: TData) => string | undefined
  /** Shown in the empty-state row when data is empty. */
  emptyMessage?: string
}>()

const sorting = ref<SortingState>([])

const table = useVueTable({
  get data() {
    return props.data
  },
  get columns() {
    return props.columns
  },
  getCoreRowModel: getCoreRowModel(),
  getSortedRowModel: getSortedRowModel(),
  onSortingChange: (updater) => valueUpdater(updater, sorting),
  state: {
    get sorting() {
      return sorting.value
    },
  },
})

/** ariaSort maps a column's current sort direction to the `aria-sort` value native to a real <th> — set only on the sorted column, per spec. */
function ariaSort(dir: false | 'asc' | 'desc'): 'ascending' | 'descending' | undefined {
  if (dir === 'asc') return 'ascending'
  if (dir === 'desc') return 'descending'
  return undefined
}
</script>

<template>
  <Table>
    <TableHeader>
      <TableRow v-for="headerGroup in table.getHeaderGroups()" :key="headerGroup.id">
        <TableHead
          v-for="header in headerGroup.headers"
          :key="header.id"
          :class="header.column.columnDef.meta?.align === 'right' ? 'text-right' : undefined"
          :aria-sort="header.column.getCanSort() ? (ariaSort(header.column.getIsSorted()) ?? 'none') : undefined"
        >
          <button
            v-if="header.column.getCanSort()"
            type="button"
            class="-mx-1.5 inline-flex items-center gap-1 rounded px-1.5 py-0.5 hover:text-foreground focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:outline-1 focus-visible:outline-ring"
            @click="header.column.toggleSorting(header.column.getIsSorted() === 'asc')"
          >
            <FlexRender v-if="!header.isPlaceholder" :render="header.column.columnDef.header" :props="header.getContext()" />
            <FontAwesomeIcon
              :icon="header.column.getIsSorted() === 'asc' ? faSortUp : header.column.getIsSorted() === 'desc' ? faSortDown : faSort"
              class="size-3 shrink-0"
              :class="header.column.getIsSorted() ? 'text-foreground' : 'text-muted-foreground/60'"
              aria-hidden="true"
            />
          </button>
          <FlexRender v-else-if="!header.isPlaceholder" :render="header.column.columnDef.header" :props="header.getContext()" />
        </TableHead>
      </TableRow>
    </TableHeader>
    <TableBody>
      <TableEmpty v-if="table.getRowModel().rows.length === 0" :colspan="columns.length" class="text-muted-foreground">
        {{ emptyMessage ?? 'none' }}
      </TableEmpty>
      <TableRow v-for="row in table.getRowModel().rows" :key="row.id" :class="rowClass?.(row.original)">
        <TableCell
          v-for="cell in row.getVisibleCells()"
          :key="cell.id"
          :class="cell.column.columnDef.meta?.align === 'right' ? 'text-right tabular-nums' : undefined"
        >
          <FlexRender :render="cell.column.columnDef.cell" :props="cell.getContext()" />
        </TableCell>
      </TableRow>
    </TableBody>
  </Table>
</template>
