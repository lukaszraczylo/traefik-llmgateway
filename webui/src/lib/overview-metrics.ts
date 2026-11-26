import { scopeId } from '@/lib/scope'
import { other } from '@/lib/series'
import type { Series } from '@/lib/series'
import type { HistoryWindow } from '@/types/api'

export type Trend = 'up' | 'down' | 'flat'

export interface Delta {
  text: string
  trend: Trend
}

export interface ChartLine {
  name: string
  data: number[]
  tone?: 'muted' | 'primary' | 'danger'
  stack?: false
}

const MS_PER_HOUR = 3_600_000
const MS_PER_DAY = 24 * MS_PER_HOUR
const DAYS_PER_MONTH = 30
const FLAT_BELOW_PCT = 0.5
const DELTA_CAP_PCT = 999

export function sumPoints(points: readonly number[]): number {
  let total = 0
  for (const p of points) if (Number.isFinite(p)) total += p
  return total
}

/** Signed whole-percent change against the previous period; null when there is nothing to compare. */
export function deltaOf(current: number, previous: number | null): Delta | null {
  if (previous === null || !(previous > 0) || !Number.isFinite(current)) return null
  const pct = ((current - previous) / previous) * 100
  if (Math.abs(pct) < FLAT_BELOW_PCT) return { text: '0%', trend: 'flat' }
  const capped = Math.min(DELTA_CAP_PCT, Math.max(-DELTA_CAP_PCT, Math.round(pct)))
  return { text: `${capped > 0 ? '+' : ''}${capped}%`, trend: capped > 0 ? 'up' : 'down' }
}

export function rangeMs(window: HistoryWindow, span: number): number {
  if (window === 'hour') return span * MS_PER_HOUR
  if (window === 'day') return span * MS_PER_DAY
  return span * DAYS_PER_MONTH * MS_PER_DAY
}

/** Counts rate_limit and paced events whose time falls inside the last `windowMs`. */
export function countThrottled(
  events: ReadonlyArray<{ kind: string; time: string }>,
  now: number,
  windowMs: number,
): { rateLimited: number; paced: number } {
  let rateLimited = 0
  let paced = 0
  for (const e of events) {
    const at = Date.parse(e.time)
    if (!Number.isFinite(at) || at < now - windowMs) continue
    if (e.kind === 'rate_limit') rateLimited++
    else if (e.kind === 'paced') paced++
  }
  return { rateLimited, paced }
}

/** Stacked traffic lines: the top models, the remainder as Other, and optionally the previous period as a plain line. */
export function trafficSeries(top: Series[], totalPoints: number[], previous: number[] | null): ChartLine[] {
  const lines: ChartLine[] = top.map((s) => ({ name: scopeId(s.scope), data: s.points }))
  const rest = other(totalPoints, top)
  if (rest.some((v) => v > 0)) lines.push({ name: 'Other', data: rest, tone: 'muted' })
  if (lines.length === 0) return []
  if (previous) lines.push({ name: 'Previous period', data: previous, tone: 'primary', stack: false })
  return lines
}
