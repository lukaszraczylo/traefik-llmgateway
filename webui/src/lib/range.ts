import type { HistoryWindow, TotalsKind } from '@/types/api'

/**
 * RANGE_KEYS is the global filter bar's fixed set of time-range presets
 * (redesign-plan.md section 3.2). Each maps to a bucket window and a
 * bucket count (`span`) — the same (window, span) pair GET /admin/api/
 * usage/series and /usage/models already accept.
 */
export const RANGE_KEYS = ['24h', '48h', '7d', '14d', '30d', '3mo', '6mo', '12mo'] as const

export type RangeKey = (typeof RANGE_KEYS)[number]

export const DEFAULT_RANGE: RangeKey = '7d'

/** One range preset's resolved (window, span) pair. */
export interface RangePreset {
  window: HistoryWindow
  span: number
}

export const RANGE_PRESETS: Record<RangeKey, RangePreset> = {
  '24h': { window: 'hour', span: 24 },
  '48h': { window: 'hour', span: 48 },
  '7d': { window: 'day', span: 7 },
  '14d': { window: 'day', span: 14 },
  '30d': { window: 'day', span: 30 },
  '3mo': { window: 'month', span: 3 },
  '6mo': { window: 'month', span: 6 },
  '12mo': { window: 'month', span: 12 },
}

/**
 * HISTORY_MAX_SPAN mirrors the server's own retention ceiling per window
 * (limits.go: historyMaxSpan — hour 48, day 35, month 13). The frontend
 * keeps its own copy rather than reading it from an endpoint: it never
 * changes without a server redeploy, and comparison() below needs it
 * synchronously to decide whether a comparison request would even be
 * valid before firing it.
 */
export const HISTORY_MAX_SPAN: Record<HistoryWindow, number> = {
  hour: 48,
  day: 35,
  month: 13,
}

/** The comparison toggle's two states (redesign-plan.md section 3.1's `cmp` hash param). */
export const CMP_MODES = ['none', 'prev'] as const

export type CmpMode = (typeof CMP_MODES)[number]

export const DEFAULT_CMP: CmpMode = 'none'

export interface ComparisonResult {
  /** Whether "vs previous period" can be requested for this range at all. */
  available: boolean
  /** How many buckets back the comparison window starts (span+offset stays within HISTORY_MAX_SPAN when available). */
  offset: number
  /** Set only when `available` is false — the reason a GlobalFilterBar cmp toggle disables itself, e.g. as a disabled control's title. */
  reason?: string
}

/**
 * comparison reports whether `range` can be paired with a "vs previous
 * period" comparison request, and what `offset` that comparison would
 * use (Q5, redesign-plan.md DECISIONS). A comparison covers the SAME
 * span twice — the current period and the one immediately before it — so
 * it needs `2 * span` buckets total; it is disabled whenever that would
 * exceed the window's own retention ceiling (HISTORY_MAX_SPAN), which is
 * exactly 48h, 30d, and 12mo among the eight presets above.
 */
export function comparison(range: RangeKey): ComparisonResult {
  const { window, span } = RANGE_PRESETS[range]
  const max = HISTORY_MAX_SPAN[window]
  if (2 * span > max) return { available: false, offset: span, reason: 'retention limit' }
  return { available: true, offset: span }
}

/**
 * dayOrMonthWindow clamps an arbitrary HistoryWindow/span pair (usually the
 * global filters store's current range) down to the day/month-only window
 * GET /admin/api/usage/totals accepts for its usermodel/modeluser/
 * targetcaller kinds (redesign-plan.md section 1.3.v: "usermodel/modeluser/
 * targetcaller: day/month only"). 'month' passes through unchanged; 'hour'
 * (the 24h/48h global ranges, which have no day/month equivalent span at
 * all) falls back to a fixed one-week day window rather than an arbitrary
 * guess at what span the reader meant; 'day' passes its own span through
 * unchanged. The single shared clamp for every one of those three kinds —
 * stores/consumers.ts (usermodel), stores/spend.ts (usermodel drilldown),
 * lib/target-columns.ts/TargetCallers.vue (targetcaller) all import this
 * rather than each re-deriving their own.
 */
export function dayOrMonthWindow(window: HistoryWindow, span: number): { window: 'day' | 'month'; span: number } {
  if (window === 'month') return { window: 'month', span }
  if (window === 'day') return { window: 'day', span }
  return { window: 'day', span: 7 }
}

/**
 * dayOrMonthClampNotice (N3 fix, verify-redesign-final.md) is
 * dayOrMonthWindow's own "worth telling the reader" half: dayOrMonthWindow
 * silently substitutes a fixed day-window whenever `window` is 'hour'
 * (24h/48h has no day/month equivalent span at all) — this reports the
 * exact substituted range as a one-line note when that happened, and
 * null when it did not ('day'/'month' pass through dayOrMonthWindow
 * unchanged, so there is nothing to disclose).
 *
 * Every reader of dayOrMonthWindow that surfaces its result to a screen
 * — UserDetail's "Models used", the Spend drilldown's usermodel level,
 * TargetCallers — shares this one function via ClampedRangeNotice.vue,
 * so the wording never drifts between them. ReliabilityPage.vue's own
 * month-clamp note is a SEPARATE, unrelated clamp (hour/day -> a fixed
 * 30-day window, stores/reliability.ts's hourOrDayWindow) and stays its
 * own inline Alert, not this one.
 */
export function dayOrMonthClampNotice(window: HistoryWindow, span: number): string | null {
  if (window !== 'hour') return null
  const clamped = dayOrMonthWindow(window, span)
  return `Showing last ${clamped.span} days — per-hour data is not kept for this view.`
}

/** isRangeKey is a runtime type guard against RANGE_KEYS — the only place that string union is checked against untrusted input (a hash, a hand-typed URL). */
export function isRangeKey(value: string): value is RangeKey {
  return (RANGE_KEYS as readonly string[]).includes(value)
}

/** isCmpMode is CMP_MODES' own runtime type guard, mirroring isRangeKey above. */
export function isCmpMode(value: string): value is CmpMode {
  return (CMP_MODES as readonly string[]).includes(value)
}

export interface SeriesUrlParams {
  /** One or more scope ids — repeated as separate `scope` query params, matching admin.go's `scope` (1..100, repeated). */
  scope: string[]
  metric: string
  window: HistoryWindow
  span: number
  /** Buckets back from "now" the window starts — omitted (server default 0) when 0. */
  offset?: number
}

/** seriesUrl builds the query string for GET /admin/api/usage/series (redesign-plan.md section 1.3.iv) — the one place every store that reads it (spend/consumers/models/reliability, phase 2) builds that URL, rather than each hand-rolling its own URLSearchParams. */
export function seriesUrl(params: SeriesUrlParams): string {
  const q = new URLSearchParams()
  for (const scope of params.scope) q.append('scope', scope)
  q.set('metric', params.metric)
  q.set('window', params.window)
  q.set('span', String(params.span))
  if (params.offset) q.set('offset', String(params.offset))
  return `/admin/api/usage/series?${q.toString()}`
}

export interface TotalsUrlParams {
  kind: TotalsKind
  window?: HistoryWindow
  span?: number
  offset?: number
  /** Comma-joined server-side (admin.go's `metrics` query param) — omitted entirely (server default: every metric that kind allows) when empty/absent. */
  metrics?: string[]
  limit?: number
  /** kind 'user' only — narrows to one group's members. */
  group?: string
  /** kind 'usermodel' only — required by the server. */
  user?: string
  /** kind 'modeluser' only — required by the server. */
  model?: string
  /** kind 'targetcaller' only — "mcp/{name}" or "agent/{name}". */
  target?: string
}

/** totalsUrl builds the query string for GET /admin/api/usage/totals (redesign-plan.md section 1.3.v), mirroring seriesUrl above. */
export function totalsUrl(params: TotalsUrlParams): string {
  const q = new URLSearchParams()
  q.set('kind', params.kind)
  if (params.window) q.set('window', params.window)
  if (params.span !== undefined) q.set('span', String(params.span))
  if (params.offset) q.set('offset', String(params.offset))
  if (params.metrics && params.metrics.length > 0) q.set('metrics', params.metrics.join(','))
  if (params.limit !== undefined) q.set('limit', String(params.limit))
  if (params.group) q.set('group', params.group)
  if (params.user) q.set('user', params.user)
  if (params.model) q.set('model', params.model)
  if (params.target) q.set('target', params.target)
  return `/admin/api/usage/totals?${q.toString()}`
}
