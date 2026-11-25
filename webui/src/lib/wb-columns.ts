import type { DataTableColumn } from '@perfect-design/ui'

export type WbKind = 'text' | 'number' | 'share' | 'limit' | 'status' | 'time'

export interface WbColumnDef {
  key: string
  header: string
  kind: WbKind
  sortable?: boolean
  width?: string
}

/** A DataTable column that remembers its Workbench kind, so WbTable can pick the cell renderer. */
export type WbColumn<T = unknown> = DataTableColumn<T> & { kind: WbKind }

const KIND_CLASS: Record<WbKind, string> = {
  text: 'wb-text',
  number: 'wb-num',
  share: 'wb-share',
  limit: 'wb-limit',
  status: 'wb-status',
  time: 'wb-time',
}

// Share bars and status chips carry no useful order of their own.
const UNSORTABLE_BY_DEFAULT: ReadonlySet<WbKind> = new Set(['share', 'status'])

/** Expands compact column definitions into DataTable columns with the Workbench alignment rules. */
export function wbColumns<T = unknown>(defs: WbColumnDef[]): WbColumn<T>[] {
  return defs.map((d) => {
    const col: WbColumn<T> = {
      key: d.key,
      header: d.header,
      kind: d.kind,
      sortable: d.sortable ?? !UNSORTABLE_BY_DEFAULT.has(d.kind),
      class: KIND_CLASS[d.kind],
    }
    if (d.width) col.width = d.width
    if (d.kind === 'number') {
      col.numeric = true
      col.align = 'right'
    }
    return col
  })
}
