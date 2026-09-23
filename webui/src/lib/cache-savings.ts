import type { HistoryWindow } from '@/types/api'

/**
 * CacheSavingsStatus is cacheSavingsStatus's own result: whether the
 * requested bucket window can report a cache-savings figure at all, and
 * any caveat text worth showing alongside a figure that IS available but
 * incomplete.
 */
export interface CacheSavingsStatus {
  available: boolean
  note: string | null
}

/**
 * cacheSavingsStatus (N2 fix, verify-redesign-final.md) reports whether
 * GET /admin/api/usage/models' `detail=1` `cacheSavedMicroUsd` figure is
 * available for `window`, and any caveat note CostAvoidedCard.vue should
 * show alongside it — grounded in admin.go's own cache-savings read
 * (serveAdminUsageModelsDetail, admin.go:~1826-1851):
 *   - `'hour'`: chit/csave are OMITTED entirely (both fields left
 *     undefined, never a fabricated `$0.00`) — recordCacheHit only ever
 *     writes them at DAY granularity (plan §1.2: "model day only"), so
 *     there is no per-hour figure to recover at all, exact or otherwise.
 *   - `'day'`: read directly at the exact requested day buckets — exact,
 *     no caveat needed.
 *   - `'month'`: read via monthRangeToDayBuckets' exact covering day
 *     buckets, but those are capped at the day counter's own 35-day
 *     retention (historyMaxSpan(windowDay), admin.go) — a 3mo/6mo/12mo
 *     range's figure only ever covers the most recent 35 days of it,
 *     never silently more, and never the whole selected range once that
 *     range exceeds 35 days.
 *
 * This replaces an earlier, stale note ("reflects the last N-bucket
 * window, not the selected range") that described a DIFFERENT, already-
 * fixed backend bug (P3 item 15: cache savings used to always sum DAY
 * buckets regardless of the requested window) — the current backend
 * behaviour above is what ships now, not that one.
 */
export function cacheSavingsStatus(window: HistoryWindow): CacheSavingsStatus {
  if (window === 'hour') {
    return { available: false, note: 'Cache savings are not tracked per hour — pick a day or month range instead.' }
  }
  if (window === 'month') {
    return { available: true, note: 'Covers only the most recent 35 days (day-bucket retention), not the full selected range.' }
  }
  return { available: true, note: null }
}
