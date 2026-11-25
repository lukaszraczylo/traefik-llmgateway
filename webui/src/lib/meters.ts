import { formatCompactCount } from '@/lib/format'
import type { AdminUsageEntryView, LimitSources, LimitsConfig } from '@/types/api'

/** Pacing starts here when a scope sets no pacing block; matches the budget bars' amber tier. */
const DEFAULT_HOT_FRACTION = 0.8
const PERCENT = 100

export interface MeterMarker {
  value: number
  label: string
  tone: 'warning' | 'danger'
}

export interface Meter {
  label: string
  value: number
  max: number
  markers: MeterMarker[]
}

type LiveEntry = Pick<AdminUsageEntryView, 'requestsPerMinute' | 'requestsPerDay' | 'tokensPerMinute'>

const has = (n: number | undefined): n is number => typeof n === 'number' && Number.isFinite(n) && n > 0

function limitMarker(limit: number): MeterMarker {
  return { value: limit, label: 'limit', tone: 'danger' }
}

function pacingMarker(limit: number, limits: LimitsConfig): MeterMarker | null {
  const pct = limits.pacing?.startPct
  return has(pct) ? { value: (limit * pct) / PERCENT, label: `pacing ${pct}%`, tone: 'warning' } : null
}

function minuteMarkers(limit: number, limits: LimitsConfig, live: boolean): MeterMarker[] {
  const pacing = live ? pacingMarker(limit, limits) : null
  return pacing ? [pacing, limitMarker(limit)] : [limitMarker(limit)]
}

/** meterModel builds one meter per set limit; pacing applies to minute meters only, and needs a live reading. */
export function meterModel(entry: LiveEntry, limits: LimitsConfig | undefined): Meter[] {
  if (!limits) return []
  const out: Meter[] = []
  if (has(limits.requestsPerMinute)) {
    out.push({ label: 'Requests/min', value: entry.requestsPerMinute, max: limits.requestsPerMinute, markers: minuteMarkers(limits.requestsPerMinute, limits, true) })
  }
  if (has(limits.requestsPerDay)) {
    out.push({ label: 'Requests/day', value: entry.requestsPerDay, max: limits.requestsPerDay, markers: [limitMarker(limits.requestsPerDay)] })
  }
  if (has(limits.tokensPerMinute)) {
    const live = entry.tokensPerMinute !== undefined
    out.push({ label: 'Tokens/min', value: entry.tokensPerMinute ?? 0, max: limits.tokensPerMinute, markers: minuteMarkers(limits.tokensPerMinute, limits, live) })
  }
  return out
}

/** isHot reports a minute meter at or past the pacing start (80% when pacing is unset). */
export function isHot(entry: LiveEntry, limits: LimitsConfig | undefined): boolean {
  if (!limits) return false
  const threshold = has(limits.pacing?.startPct) ? (limits.pacing as { startPct: number }).startPct / PERCENT : DEFAULT_HOT_FRACTION
  return meterModel(entry, limits)
    .filter((m) => m.label.endsWith('/min'))
    .some((m) => m.value / m.max >= threshold)
}

/** primaryLimit picks the single limit a table cell shows. */
export function primaryLimit(limits: LimitsConfig | undefined): { limit: number | null; unit: string } {
  if (has(limits?.requestsPerMinute)) return { limit: limits.requestsPerMinute, unit: 'min' }
  if (has(limits?.tokensPerMinute)) return { limit: limits.tokensPerMinute, unit: 'tok/min' }
  if (has(limits?.requestsPerDay)) return { limit: limits.requestsPerDay, unit: 'day' }
  return { limit: null, unit: '' }
}

export interface LimitRow {
  key: string
  label: string
  value: string
  source: string
}

const LIMIT_FIELDS: Array<{ key: Exclude<keyof LimitsConfig, 'pacing'>; label: string; usd?: boolean }> = [
  { key: 'requestsPerMinute', label: 'Requests/min' },
  { key: 'requestsPerDay', label: 'Requests/day' },
  { key: 'tokensPerMinute', label: 'Tokens/min' },
  { key: 'tokensPerDay', label: 'Tokens/day' },
  { key: 'tokensPerMonth', label: 'Tokens/month' },
  { key: 'costPerDayUSD', label: 'Cost/day', usd: true },
  { key: 'costPerMonthUSD', label: 'Cost/month', usd: true },
]

const MEMBER_PREFIX = 'memberLimits:'

function sourceLabel(source: string | undefined): string {
  if (!source) return ''
  return source.startsWith(MEMBER_PREFIX) ? `group ${source.slice(MEMBER_PREFIX.length)} (per member)` : source
}

/** effectiveLimitRows lists each set limit with the config layer it came from. */
export function effectiveLimitRows(limits: LimitsConfig | undefined, sources: LimitSources | undefined): LimitRow[] {
  if (!limits) return []
  const rows: LimitRow[] = []
  for (const f of LIMIT_FIELDS) {
    const v = limits[f.key]
    if (has(v)) rows.push({ key: f.key, label: f.label, value: f.usd ? `$${v}` : formatCompactCount(v), source: sourceLabel(sources?.[f.key]) })
  }
  if (limits.pacing) {
    rows.push({ key: 'pacing', label: 'Pacing', value: `from ${limits.pacing.startPct}%, up to ${limits.pacing.maxDelay} delay`, source: sourceLabel(sources?.pacing) })
  }
  return rows
}
