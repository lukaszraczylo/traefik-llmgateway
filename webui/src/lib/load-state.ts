/**
 * LoadState is the four states any data-driven view in this panel
 * renders in place of its real content — the single vocabulary
 * EmptyState.vue/ErrorState.vue/the Skeleton* components all key off of,
 * so every page decides "skeleton vs error vs empty vs ready" the same
 * way instead of each growing its own ad-hoc v-if chain.
 */
export type LoadState = 'skeleton' | 'error' | 'empty' | 'ready'

/** LoadStateInput is loadState's own three signals — every store in this panel already exposes something that maps onto these (a `loading`/`lastUpdated` flag, a data field's null-ness or length, an `error` string). */
export interface LoadStateInput {
  /** Whether a fetch for the CURRENT selection is in flight (or has never yet completed) — a background refresh of data already on screen does not count (see hasData's own precedence below). */
  loading: boolean
  /** Whether there is currently displayable data for the CURRENT selection — false right after a filter/selection change clears the previous selection's data, not just on a cold first load. */
  hasData: boolean
  /** The store's current error message, '' when there is none. */
  error: string
}

/**
 * loadState decides which of the four LoadState values a view should
 * render, from the three signals above. Precedence, highest first:
 *
 * 1. 'ready' whenever `hasData` is true — even alongside a non-empty
 *    `error` (a background poll just failed but the PREVIOUS successful
 *    result is still on screen): states-plan.md item 1's own rule is
 *    that a background refresh must never replace rendered data with a
 *    skeleton, and a background failure must never blank it into an
 *    error state either — that case surfaces via a toast instead
 *    (stores/toasts.ts), not by this helper switching states.
 * 2. 'error' when there is no data and `error` is set — the first fetch
 *    for this selection failed outright, nothing to show underneath it.
 * 3. 'skeleton' when there is no data, no error, and `loading` is true —
 *    first load, or a filter/selection change that cleared the previous
 *    selection's own data and kicked off a fresh fetch.
 * 4. 'empty' otherwise — not loading, no error, genuinely no data.
 */
export function loadState({ loading, hasData, error }: LoadStateInput): LoadState {
  if (hasData) return 'ready'
  if (error) return 'error'
  if (loading) return 'skeleton'
  return 'empty'
}
