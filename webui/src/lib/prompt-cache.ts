import type { AdminUsageModelEntry } from '@/types/api'

type HitRateInput = Pick<AdminUsageModelEntry, 'tokensIn' | 'promptCacheReadTokens'>

/** Share of input tokens served from the provider prompt cache; null when unknown (no input, or an older backend / non-detail row). */
export function promptCacheHitRate(row: HitRateInput): number | null {
  if (row.promptCacheReadTokens === undefined || !row.tokensIn) return null
  return Math.min(1, row.promptCacheReadTokens / row.tokensIn)
}

/** Total real paid-provider saving from prompt caching, in micro-USD. */
export function promptCacheSavedMicros(rows: Pick<AdminUsageModelEntry, 'promptCacheSavedMicroUsd'>[]): number {
  let total = 0
  for (const r of rows) total += r.promptCacheSavedMicroUsd ?? 0
  return total
}

/** Hit rate as a percentage string, or an em dash when unknown. */
export function formatHitRate(rate: number | null): string {
  return rate === null ? '—' : `${Math.round(rate * 1000) / 10}%`
}
