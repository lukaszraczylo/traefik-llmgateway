// Mirror of the backend's GET /admin/api/usage/series validation (stats_read.go, admin.go). Keep case-for-case in sync.

export const SERIES_MAX_SCOPES = 100
const HISTORY_MAX_SPAN: Record<string, number> = { hour: 48, day: 35, month: 13 }
const HISTORY_METRICS = ['req', 'tokin', 'tokout', 'cost']
const KNOWN_STATS_METRICS = ['rej', 'attempt', 'fail', 'timeout', 'fover', 'r402', 'chit', 'cmiss', 'csave']

export type SeriesScopeKind = 'user' | 'group' | 'total' | 'model' | 'prov' | 'provmodel'

/** parseSeriesScope: the scope kind a `scope` param names, or null when the backend would list it as unknown. */
export function parseSeriesScope(raw: string): SeriesScopeKind | null {
  if (raw === 'total') return 'total'
  const at = raw.indexOf(':')
  if (at < 0 || at === raw.length - 1) return null
  switch (raw.slice(0, at)) {
    case 'user':
    case 'group':
    case 'model':
    case 'provmodel':
      return raw.slice(0, at) as SeriesScopeKind
    case 'provider':
      return 'prov'
    default:
      return null
  }
}

/** seriesValidMetric: stats_read.go's metric x scope-kind x window matrix. */
export function seriesValidMetric(scopeKind: SeriesScopeKind, metric: string, window: string): boolean {
  const hourOrDay = window === 'hour' || window === 'day'
  switch (scopeKind) {
    case 'user':
    case 'group':
    case 'total':
      if (['req', 'tokin', 'tokout', 'cost'].includes(metric)) return true
      if (metric === 'rej') return hourOrDay
      if (scopeKind === 'total' && ['chit', 'cmiss', 'csave', 'r402'].includes(metric)) return hourOrDay
      return false
    case 'model':
      if (['req', 'tokin', 'tokout', 'cost'].includes(metric)) return true
      if (['chit', 'csave', 'r402'].includes(metric)) return window === 'day'
      return false
    case 'prov':
      return ['attempt', 'fail', 'timeout', 'fover'].includes(metric) && hourOrDay
    case 'provmodel':
      return ['attempt', 'fail'].includes(metric) && hourOrDay
  }
}

const isInt = (raw: string): boolean => /^[+-]?\d+$/.test(raw)

/** seriesQueryError: the 400 message serveAdminUsageSeries returns for this query, or null when the backend accepts it. Same check order as the Go handler. */
export function seriesQueryError(query: URLSearchParams): string | null {
  const scopes = query.getAll('scope')
  if (scopes.length === 0) return 'at least one scope is required'
  if (scopes.length > SERIES_MAX_SCOPES) return 'too many scopes'
  const window = query.get('window') ?? ''
  const max = HISTORY_MAX_SPAN[window]
  if (max === undefined) return 'unknown window'
  const metric = query.get('metric') ?? ''
  if (!HISTORY_METRICS.includes(metric) && !KNOWN_STATS_METRICS.includes(metric)) return 'unknown metric'
  const rawSpan = query.get('span') ?? ''
  let span = max
  if (rawSpan !== '') {
    span = Number(rawSpan)
    if (!isInt(rawSpan) || span < 1 || span > max) return 'invalid span'
  }
  const rawOffset = query.get('offset') ?? ''
  if (rawOffset !== '') {
    const offset = Number(rawOffset)
    if (!isInt(rawOffset) || offset < 0 || offset > max - span) return 'invalid offset'
  }
  for (const raw of scopes) {
    const kind = parseSeriesScope(raw)
    if (kind !== null && !seriesValidMetric(kind, metric, window)) return 'unknown metric for scope kind'
  }
  return null
}
