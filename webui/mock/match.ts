import { WINDOW_HOURS, type MockWindow } from './time-shift.ts'

export interface Fixture {
  file: string
  path: string
  query: Record<string, string | string[]>
  status: number
}

/** Query params that decide which fixture answers; anything else (limit, cache busters) never affects matching. */
const SIGNIFICANT: Record<string, string[]> = {
  '/admin/api/usage/series': ['scope', 'metric', 'window', 'span', 'offset'],
  '/admin/api/usage/totals': ['kind', 'window', 'span', 'offset', 'metrics', 'group', 'user', 'model', 'target'],
  '/admin/api/usage/models': ['metric', 'window', 'span', 'detail'],
  '/admin/api/performance': ['kind', 'window', 'span', 'offset', 'series', 'id'],
}

/** Params naming an entity; a fixture for another entity of the same class stands in when the exact one is missing. */
const ID_PARAMS: Record<string, string[]> = {
  '/admin/api/usage/series': ['scope'],
  '/admin/api/usage/totals': ['group', 'user', 'model', 'target'],
  '/admin/api/performance': ['id'],
}

/** Metric ratios relative to requests, used only when a series fixture exists for another metric. */
export const METRIC_PER_REQ: Record<string, number> = {
  req: 1,
  tokin: 45_000,
  tokout: 600,
  cost: 150,
  attempt: 1.25,
  fail: 0.15,
  fover: 0.05,
  timeout: 0.03,
  r402: 0.002,
  rej: 0.004,
}

export const TOTALS_METRICS = ['req', 'tokin', 'tokout', 'cost']

export type NormQuery = Record<string, string[]>

export function normaliseQuery(path: string, query: URLSearchParams | Record<string, string | string[]>): NormQuery {
  const keys = SIGNIFICANT[path] ?? []
  const entries: [string, string][] =
    query instanceof URLSearchParams
      ? [...query.entries()]
      : Object.entries(query).flatMap(([k, v]) => (Array.isArray(v) ? v.map((x): [string, string] => [k, x]) : [[k, v]]))
  const out: NormQuery = {}
  for (const [k, v] of entries) {
    if (!keys.includes(k)) continue
    const parts = k === 'metrics' ? v.split(',').filter(Boolean) : [v]
    out[k] = [...(out[k] ?? []), ...parts]
  }
  for (const k of Object.keys(out)) {
    out[k] = [...new Set(out[k])].sort()
    if (k === 'offset' && out[k].every((x) => Number(x) === 0)) delete out[k]
  }
  return out
}

const keyOf = (q: NormQuery) =>
  Object.keys(q)
    .sort()
    .map((k) => `${k}=${q[k]!.join(',')}`)
    .join('&')

const scopeClass = (s: string) => (s.includes(':') ? s.slice(0, s.indexOf(':')) : s)

function classOf(path: string, param: string, values: string[]): string {
  if (path === '/admin/api/usage/series' && param === 'scope') return [...new Set(values.map(scopeClass))].sort().join('|')
  return '*'
}

function overlap(a: string[], b: string[]): number {
  const set = new Set(b)
  return a.filter((x) => set.has(x)).length
}

/** Exact match on normalised query (key order, repeated-key order and non-significant params ignored), else the fixture for a same-class id. */
export function matchFixture(manifest: Fixture[], path: string, query: URLSearchParams): Fixture | null {
  const cands = manifest.filter((f) => f.path === path)
  if (cands.length === 0) return null
  const want = normaliseQuery(path, query)
  const wantKey = keyOf(want)
  const exact = cands.find((f) => keyOf(normaliseQuery(path, f.query)) === wantKey)
  if (exact) return exact
  const ids = ID_PARAMS[path] ?? []
  let best: Fixture | null = null
  let bestScore = -Infinity
  for (const f of cands) {
    const have = normaliseQuery(path, f.query)
    const others = Object.keys({ ...want, ...have }).filter((k) => !ids.includes(k))
    if (others.some((k) => (want[k] ?? []).join(',') !== (have[k] ?? []).join(','))) continue
    let score = 0
    let ok = true
    for (const k of ids) {
      const w = want[k]
      const h = have[k]
      if (!w !== !h) ok = false
      else if (w && h) {
        if (classOf(path, k, w) !== classOf(path, k, h)) ok = false
        score += overlap(w, h) * 10 - Math.abs(w.length - h.length)
      }
    }
    if (ok && score > bestScore) {
      best = f
      bestScore = score
    }
  }
  return best
}

export const hoursOf = (q: NormQuery): number => {
  const w = (q.window?.[0] ?? 'day') as MockWindow
  return Number(q.span?.[0] ?? 14) * (WINDOW_HOURS[w] ?? 24)
}

/** Closeness of a fixture's range to the requested one: same window and similar total span score highest. */
function rangeScore(want: NormQuery, have: NormQuery): number {
  const sameWindow = (want.window?.[0] ?? 'day') === (have.window?.[0] ?? 'day') ? 20 : 0
  return sameWindow - Math.abs(Math.log(hoursOf(have) / hoursOf(want))) * 5
}

const fixtureMetrics = (q: NormQuery, kind: string | undefined): string[] =>
  q.metrics ?? (kind === 'targetcaller' ? ['req'] : TOTALS_METRICS)

/** Closest fixture of any other range, to be rescaled by the caller. Null when nothing is usable. */
export function nearestFixture(manifest: Fixture[], path: string, query: URLSearchParams): Fixture | null {
  const want = normaliseQuery(path, query)
  let best: Fixture | null = null
  let bestScore = -Infinity
  for (const f of manifest) {
    if (f.path !== path || f.status !== 200) continue
    const have = normaliseQuery(path, f.query)
    let score = rangeScore(want, have)
    if (path === '/admin/api/usage/series') {
      const metric = want.metric?.[0] ?? ''
      const same = have.metric?.[0] === metric
      if (!same && !(have.metric?.[0] && have.metric[0] in METRIC_PER_REQ && metric in METRIC_PER_REQ)) continue
      score += same ? 100 : 0
      const w = want.scope ?? []
      const h = have.scope ?? []
      score += (overlap(w, h) / Math.max(w.length, 1)) * 60
      score += classOf(path, 'scope', w) === classOf(path, 'scope', h) ? 20 : 0
      score -= Math.abs(w.length - h.length) * 0.1
      score += (want.offset?.[0] ?? '0') === (have.offset?.[0] ?? '0') ? 5 : 0
    } else if (path === '/admin/api/usage/totals') {
      if (want.kind?.[0] !== have.kind?.[0]) continue
      for (const k of ID_PARAMS[path]!) if (!want[k] !== !have[k]) score = -Infinity
      if (score === -Infinity) continue
      const need = want.metrics ?? fixtureMetrics(want, want.kind?.[0])
      const got = fixtureMetrics(have, have.kind?.[0])
      if (need.some((m) => !got.includes(m))) continue
      for (const k of ID_PARAMS[path]!) if (want[k]?.join() === have[k]?.join()) score += 30
      score += need.length === got.length ? 10 : 0
    } else if (path === '/admin/api/usage/models') {
      // A detail fixture holds the full ranking, so it can answer a plain request for any metric; a
      // plain fixture is a top-N cut of one metric and only answers that metric.
      if (want.detail && !have.detail) continue
      if (!have.detail && have.metric?.[0] !== want.metric?.[0]) continue
      score += have.metric?.[0] === want.metric?.[0] ? 100 : 0
    } else if (path === '/admin/api/performance') {
      if (want.kind?.[0] !== have.kind?.[0] || !want.series !== !have.series) continue
      if (!want.id !== !have.id) continue
    }
    if (score > bestScore) {
      best = f
      bestScore = score
    }
  }
  return best
}
