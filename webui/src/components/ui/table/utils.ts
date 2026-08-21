import type { RowData, Updater } from '@tanstack/vue-table'
import type { Ref } from 'vue'

/**
 * valueUpdater applies one TanStack Table `Updater<T>` — either a new
 * value, or a function from the old value to the new one — to a plain Vue
 * ref. This is the standard shadcn-vue glue between TanStack Table's
 * callback-based state updates (`onSortingChange`, etc.) and Vue's
 * ref-based reactivity (shadcn-vue docs: /docs/components/data-table).
 */
export function valueUpdater<T>(updaterOrValue: Updater<T>, ref: Ref<T>): void {
  ref.value = typeof updaterOrValue === 'function' ? (updaterOrValue as (old: T) => T)(ref.value) : updaterOrValue
}

declare module '@tanstack/vue-table' {
  interface ColumnMeta<TData extends RowData, TValue> {
    /** Right-align this column's header button and cell content, with tabular-nums — set on numeric columns (DataTable.vue reads this). */
    align?: 'right'
  }
}
