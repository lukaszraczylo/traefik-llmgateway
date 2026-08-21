<script setup lang="ts" generic="TData">
import type { Header } from '@tanstack/vue-table'
import { faSort, faSortDown, faSortUp } from '@fortawesome/free-solid-svg-icons'
import { FlexRender } from '@tanstack/vue-table'
import { computed } from 'vue'

/**
 * SortHeaderButton is the one clickable, sortable-column control every
 * sortable header in the panel uses — DataTable.vue's real `<th>` header
 * buttons AND UsageView.vue's Groups sort toolbar (which has no `<table>`
 * to put a `<th>` in — see that view's own doc comment for why).
 * Extracted so the button markup, sort-icon logic, click handler, and
 * accessible name exist in exactly one place (vue.md: "if you've written
 * it twice, you owe an abstraction"). Extra classes on this component
 * (e.g. DataTable.vue's `-mx-1.5` cell-edge alignment) fall through onto
 * the root `<button>` via Vue's default attribute inheritance.
 */
const props = defineProps<{
  header: Header<TData, unknown>
}>()

const direction = computed(() => props.header.column.getIsSorted())

const icon = computed(() => (direction.value === 'asc' ? faSortUp : direction.value === 'desc' ? faSortDown : faSort))

/** headerLabel reads the column's own string header when it has one (every column def in this app uses a plain string) — falling back to the column id so the accessible name is never empty even for a hypothetical future function-header column. */
const headerLabel = computed(() => {
  const def = props.header.column.columnDef.header
  return typeof def === 'string' ? def : props.header.column.id
})

const ariaLabel = computed(() => {
  const state = direction.value === 'asc' ? 'ascending' : direction.value === 'desc' ? 'descending' : 'none'
  return `Sort by ${headerLabel.value}, currently ${state}`
})

function onClick(): void {
  props.header.column.toggleSorting(direction.value === 'asc')
}
</script>

<template>
  <button
    type="button"
    :aria-label="ariaLabel"
    class="inline-flex items-center gap-1 rounded px-1.5 py-0.5 hover:text-foreground focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:outline-1 focus-visible:outline-ring"
    @click="onClick"
  >
    <FlexRender v-if="!header.isPlaceholder" :render="header.column.columnDef.header" :props="header.getContext()" />
    <FontAwesomeIcon
      :icon="icon"
      class="size-3 shrink-0"
      :class="direction ? 'text-foreground' : 'text-muted-foreground/60'"
      aria-hidden="true"
    />
  </button>
</template>
