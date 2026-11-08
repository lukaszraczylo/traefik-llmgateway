export interface PageWindow {
  /** Zero-based page index, clamped into range. */
  index: number
  count: number
  /** Zero-based start offset, inclusive. */
  from: number
  /** Zero-based end offset, exclusive. */
  to: number
}

/** pageWindow clamps `index` into [0, count-1] and returns the row range for it (formal/Gateway/Ui/Paging.lean). */
export function pageWindow(total: number, pageSize: number, index: number): PageWindow {
  const size = Math.max(1, Math.floor(pageSize))
  const count = Math.max(1, Math.ceil(total / size))
  const clamped = Math.min(Math.max(0, Math.floor(index)), count - 1)
  const from = clamped * size
  return { index: clamped, count, from, to: Math.min(total, from + size) }
}
