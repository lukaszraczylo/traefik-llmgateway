import { formatCompactCount } from '@/lib/format'

export const STORE_DOWN_HINT = 'usage store unavailable'

/** countOrUnknown is the counter, or null when the usage store is down and the zero would be a lie. */
export function countOrUnknown(n: number, storeDown: boolean | undefined): number | null {
  return storeDown ? null : n
}

/** formatCount renders a counter compactly, or "—" for an unknown one. */
export function formatCount(n: number | null): string {
  return n === null ? '—' : formatCompactCount(n)
}
