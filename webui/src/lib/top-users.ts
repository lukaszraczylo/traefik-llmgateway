import type { AdminTotalsResponse, HistoryWindow } from '@/types/api'

/**
 * TOP_USERS_MODES is HomePage.vue's "top users" card Tabs switch (Cost |
 * Requests | Tokens) — the values match the `top` hash param verbatim
 * ('req'/'tokens'; 'cost' is the default and is never written to the
 * hash at all, mirroring lib/access-matrix.ts's MATRIX_KINDS/`matrix`
 * param convention), so no separate internal-vs-URL name mapping exists.
 */
export const TOP_USERS_MODES = ['cost', 'req', 'tokens'] as const

export type TopUsersMode = (typeof TOP_USERS_MODES)[number]

export const DEFAULT_TOP_USERS_MODE: TopUsersMode = 'cost'

/** TOP_USERS_MODE_LABEL is each mode's Tabs label and the source for the card's "Top 5 users by …" title. */
export const TOP_USERS_MODE_LABEL: Record<TopUsersMode, string> = {
  cost: 'Cost',
  req: 'Requests',
  tokens: 'Tokens',
}

/**
 * ADMIN_MAX_KEYS_PER_REQUEST mirrors the server's own adminMaxKeysPerRequest
 * (stats_read.go:28, `const adminMaxKeysPerRequest = 64000`) — GET
 * /admin/api/usage/totals?kind=user rejects with 400 the moment
 * `len(ids) * len(metrics) * span` exceeds this, where `ids` is EVERY
 * configured user (stats_read.go:566-576: built from `g.auth.snapshot()`'s
 * userSummaries, not filtered to users with recent traffic). `dashboard.
 * usage.users` (admin.go's own userSummaries, already polled every 5s) IS
 * an exact, free source for this count — HomePage.vue's own
 * configuredUserCount reads it directly (verify-ui-states-2.md #4; an
 * earlier revision of this comment wrongly claimed no such source
 * existed and used a per-group memberCount sum instead, which over-counts
 * a user who belongs to more than one group). topUsersCapExceeded below
 * re-implements that same inequality for topUsersCapCheckResult's
 * best-effort predictive check.
 */
export const ADMIN_MAX_KEYS_PER_REQUEST = 64000

/**
 * TOP_USERS_METRICS_BY_MODE (P1 fix, verify-ui-states.md #1) — each mode
 * requests ONLY the metric(s) it ranks by, not all four unconditionally: a
 * fetch this card used to always shape as `metrics=req,tokin,tokout,cost`
 * (4 metrics) now costs 1 metric (cost/req) or 2 (tokens, summed
 * client-side by rankTopUsers below) against the server's own
 * len(ids)*len(metrics)*span cap — up to 4x more configured users before a
 * fleet trips adminMaxKeysPerRequest on the coarsest (hour, span 48) global
 * range. This is the ONLY lever this card has over that cap: `ids` is
 * fixed by server-side config, and the `limit` query param (below) is
 * applied AFTER the cap check, over already-fetched rows (buildTotalsRows,
 * stats_read.go:453-480) — it does not, and cannot, reduce the read cost
 * the cap guards.
 */
export const TOP_USERS_METRICS_BY_MODE: Record<TopUsersMode, readonly string[]> = {
  cost: ['cost'],
  req: ['req'],
  tokens: ['tokin', 'tokout'],
}

/**
 * parseTopUsersMode reads the `top` hash param into a TopUsersMode —
 * unknown or absent values fall back to 'cost' rather than throwing,
 * mirroring lib/access-matrix.ts's parseMatrixKind.
 */
export function parseTopUsersMode(raw: string | undefined): TopUsersMode {
  return (TOP_USERS_MODES as readonly string[]).includes(raw ?? '') ? (raw as TopUsersMode) : DEFAULT_TOP_USERS_MODE
}

/**
 * TOP_USERS_FETCH_LIMIT (1000, the server's own adminTotalsMaxLimit,
 * stats_read.go) is headroom for CLIENT-SIDE re-ranking, not a cap-safety
 * lever (see TOP_USERS_METRICS_BY_MODE's own doc comment — `limit` is
 * applied after the cap check, over already-fetched rows). It matters only
 * for 'tokens' mode: the server sorts by metrics[0] ('tokin') descending
 * and truncates to `limit` BEFORE this card re-ranks by tokin+tokout
 * summed (rankTopUsers below) — a `limit` too small could truncate away a
 * user with a large tokout but a middling tokin before the client ever
 * gets a chance to re-rank them in. 1000 keeps that reordering effect
 * practically unreachable for any real fleet.
 */
export const TOP_USERS_FETCH_LIMIT = 1000

/** TOP_USERS_RANK_LIMIT is how many rows rankTopUsers keeps after ranking — the card's own "top 5". */
export const TOP_USERS_RANK_LIMIT = 5

/**
 * topUsersTotalsUrl builds HomePage.vue's "top users" query string for one
 * mode: GET /admin/api/usage/totals?kind=user&window=<window>&span=<span>&
 * offset=0&metrics=<mode's own metrics>&limit=<limit>. Built directly (not
 * via lib/range.ts's totalsUrl) because totalsUrl's own `offset` convention
 * deliberately OMITS the param when 0 (server default, seriesUrl/totalsUrl's
 * shared doc comment, lib/range.spec.ts's own "omits offset when 0" test) —
 * this card's own fixed shape always writes `offset=0` explicitly, so it
 * cannot reuse that builder without changing totalsUrl's behavior for every
 * other caller.
 */
export function topUsersTotalsUrl(window: HistoryWindow, span: number, mode: TopUsersMode, limit: number = TOP_USERS_FETCH_LIMIT): string {
  const q = new URLSearchParams()
  q.set('kind', 'user')
  q.set('window', window)
  q.set('span', String(span))
  q.set('offset', '0')
  q.set('metrics', TOP_USERS_METRICS_BY_MODE[mode].join(','))
  q.set('limit', String(limit))
  return `/admin/api/usage/totals?${q.toString()}`
}

/**
 * topUsersCapExceeded mirrors the server's own kind=user cap check
 * (stats_read.go:573: `len(ids)*len(metrics)*span > adminMaxKeysPerRequest`)
 * so HomePage.vue can decide, BEFORE firing a request, whether it would
 * come back 400 — `userCount` is the caller's own count of every
 * CONFIGURED user (HomePage.vue's configuredUserCount, an EXACT count as
 * of verify-ui-states-2.md #4, not an estimate). Deliberately a plain,
 * easily-tested predicate over three numbers rather than something that
 * reaches into a store itself.
 */
export function topUsersCapExceeded(userCount: number, metricsCount: number, span: number): boolean {
  return userCount * metricsCount * span > ADMIN_MAX_KEYS_PER_REQUEST
}

/**
 * topUsersCapCheckResult (verify-ui-states-2.md #4) wraps topUsersCapExceeded
 * for HomePage.vue's actual call site: `configuredUserCount` is null before
 * dashboard.usage has ever loaded (a cold mount, or a load failure) — the
 * predictive check is skipped entirely in that case (returns false, so
 * fetchTopUsers fires the request) rather than guessing at a count, and the
 * server's own real 400 (fetchTopUsers's own catch, matched on "range too
 * large") still covers whatever this happens to miss once usage loads late.
 */
export function topUsersCapCheckResult(configuredUserCount: number | null, metricsCount: number, span: number): boolean {
  if (configuredUserCount === null) return false
  return topUsersCapExceeded(configuredUserCount, metricsCount, span)
}

/** One ranked "top users" row — the id plus the single numeric value the current mode ranked it by (already summed for 'tokens'). HomePage.vue formats `value` itself (formatCost for 'cost', formatCompactCount for 'req'/'tokens') — this stays a plain number so the sort/tiebreak logic below never depends on a formatted string. */
export interface RankedTopUser {
  id: string
  value: number
}

/** topUserValue reads the single metric (or metric sum) `mode` ranks by out of one totals row — 'tokens' sums tokin+tokout, matching TOP_USERS_METRICS_BY_MODE's own request shape; a metric absent from `values` (should not happen given TOP_USERS_METRICS_BY_MODE, but Record<string, number> guarantees no key) reads as 0. */
function topUserValue(row: { values: Record<string, number> }, mode: TopUsersMode): number {
  switch (mode) {
    case 'cost':
      return row.values.cost ?? 0
    case 'req':
      return row.values.req ?? 0
    case 'tokens':
      return (row.values.tokin ?? 0) + (row.values.tokout ?? 0)
  }
}

/**
 * rankTopUsers ranks ONE fetched totals response client-side for `mode` —
 * Cost/Requests/Tokens each sort by their own value descending, ties broken
 * by id ascending; a row whose CURRENT MODE's value is 0 is excluded (a
 * user with requests but $0 cost, e.g. free-model-only traffic, drops out
 * of the Cost ranking but stays in the Requests one) — distinct from the
 * server's own all-metrics-zero drop (stats_read.go's buildTotalsRows),
 * which this re-implements for the one metric set actually requested
 * rather than relying on it.
 */
export function rankTopUsers(
  rows: AdminTotalsResponse['rows'],
  mode: TopUsersMode,
  limit: number = TOP_USERS_RANK_LIMIT,
): RankedTopUser[] {
  return rows
    .map((row): RankedTopUser => ({ id: row.id, value: topUserValue(row, mode) }))
    .filter((row) => row.value > 0)
    .sort((a, b) => (b.value !== a.value ? b.value - a.value : a.id.localeCompare(b.id)))
    .slice(0, limit)
}
