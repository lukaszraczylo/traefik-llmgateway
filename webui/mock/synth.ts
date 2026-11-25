import { METRIC_PER_REQ, TOTALS_METRICS, hoursOf, normaliseQuery, type Fixture, type NormQuery } from './match.ts'
import { bucketsFor, WINDOW_HOURS, type MockWindow } from './time-shift.ts'

type Obj = Record<string, any> // eslint-disable-line @typescript-eslint/no-explicit-any

const COUNT_KEYS = ['count', 'attempts', 'failures', 'timeouts', 'failovers']
const MODEL_FIELDS: Record<string, string> = { req: 'requests', tokin: 'tokensIn', tokout: 'tokensOut', cost: 'costMicroUsd' }

export function hash(s: string): number {
  let h = 2166136261
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619)
  return h >>> 0
}

/**
 * Rescales one captured series to another bucket size, count and offset. Buckets are read as a rate
 * over a periodic extension of the capture, so a longer span repeats the captured shape and a coarser
 * bucket sums it. Output is oldest first.
 */
export function resample(points: number[], srcHours: number, dstHours: number, count: number, offset: number): number[] {
  const n = points.length
  const out = new Array<number>(count).fill(0)
  if (n === 0) return out
  const newestFirst = [...points].reverse()
  const cum = [0]
  for (const v of newestFirst) cum.push(cum[cum.length - 1]! + v)
  const total = cum[n]!
  const period = n * srcHours
  const integral = (x: number): number => {
    const whole = Math.floor(x / period)
    const y = x - whole * period
    const i = Math.min(Math.floor(y / srcHours), n - 1)
    return whole * total + cum[i]! + (newestFirst[i]! * (y - i * srcHours)) / srcHours
  }
  for (let k = 0; k < count; k++) {
    out[count - 1 - k] = Math.round(integral((k + offset + 1) * dstHours) - integral((k + offset) * dstHours))
  }
  return out
}

const windowHours = (w: unknown) => WINDOW_HOURS[w as MockWindow] ?? 24
const num = (q: NormQuery, k: string, d: number) => (q[k]?.[0] !== undefined ? Number(q[k]![0]) : d)
const win = (q: NormQuery) => (q.window?.[0] ?? 'day') as MockWindow

const scopePrefix = (s: string) => s.slice(0, s.includes(':') ? s.indexOf(':') : s.length)

/** The captured series for a scope, or a same-prefix one chosen by hash and scaled so ids differ visibly. */
function pickSeries(pool: Obj[], scope: string): { src: Obj; scale: number } {
  const hit = pool.find((s) => s.scope === scope)
  if (hit) return { src: hit, scale: 1 }
  const same = pool.filter((s) => scopePrefix(s.scope) === scopePrefix(scope))
  const from = same.length ? same : pool
  return { src: from[hash(scope) % from.length]!, scale: 0.4 + (hash(`${scope}#`) % 100) / 80 }
}

function synthSeries(body: Obj, fx: Fixture, q: NormQuery, requested: URLSearchParams, now: number): Obj {
  const have = normaliseQuery(fx.path, fx.query)
  const srcHours = windowHours(body.window)
  const dstHours = windowHours(win(q))
  const span = num(q, 'span', 14)
  const offset = num(q, 'offset', 0)
  const metric = q.metric?.[0] ?? 'req'
  const ratio = have.metric?.[0] === metric ? 1 : (METRIC_PER_REQ[metric] ?? 1) / (METRIC_PER_REQ[have.metric?.[0] ?? 'req'] ?? 1)
  const pool: Obj[] = body.series
  const series = requested.getAll('scope').map((scope) => {
    const { src, scale } = pickSeries(pool, scope)
    const points = resample(src.points, srcHours, dstHours, span, offset).map((v) => Math.round(v * ratio * scale))
    return { scope, points }
  })
  return { ...body, metric, window: win(q), span, offset, buckets: bucketsFor(win(q), span, offset, now), series, unknown: [] }
}

function synthTotals(body: Obj, q: NormQuery, limit: number | null): Obj {
  const ratio = hoursOf(q) / (windowHours(body.window) * Number(body.span))
  const kind = q.kind?.[0]
  const metrics = q.metrics ?? (kind === 'targetcaller' ? ['req'] : TOTALS_METRICS)
  const order = ['req', 'tokin', 'tokout', 'cost'].filter((m) => metrics.includes(m))
  const rows = (body.rows as Obj[]).map((r) => ({
    ...r,
    values: Object.fromEntries(order.map((m) => [m, Math.round((r.values[m] ?? 0) * ratio)])),
  }))
  return { ...body, window: win(q), span: num(q, 'span', 14), offset: num(q, 'offset', 0), metrics: order, rows: limit ? rows.slice(0, limit) : rows }
}

function synthModels(body: Obj, q: NormQuery, requested: URLSearchParams): Obj {
  const ratio = hoursOf(q) / (windowHours(body.window) * Number(body.span))
  const metric = q.metric?.[0] ?? 'cost'
  const field = MODEL_FIELDS[metric] ?? 'requests'
  const scaled = (body.models as Obj[]).map((m) => {
    const o: Obj = { ...m }
    for (const k of Object.keys(m)) if (typeof m[k] === 'number') o[k] = Math.round(m[k] * ratio)
    o.value = o[field] ?? o.value
    return o
  })
  scaled.sort((a, b) => b.value - a.value)
  const limit = Number(requested.get('limit') ?? 0)
  return { ...body, metric, window: win(q), span: num(q, 'span', 14), offset: num(q, 'offset', 0), models: limit > 0 ? scaled.slice(0, limit) : scaled }
}

function synthPerformance(body: Obj, q: NormQuery, now: number): Obj {
  const srcHours = windowHours(body.window)
  const dstHours = windowHours(win(q))
  const span = num(q, 'span', 14)
  const offset = num(q, 'offset', 0)
  const ratio = hoursOf(q) / (srcHours * Number(body.span ?? span))
  const rows = (body.rows as Obj[]).map((r) => {
    const o: Obj = { ...r }
    for (const k of COUNT_KEYS) if (typeof r[k] === 'number') o[k] = Math.round(r[k] * ratio)
    return o
  })
  const out: Obj = { ...body, window: win(q), span, offset, rows }
  if (Array.isArray(body.points)) {
    const src: Obj[] = body.points
    const buckets = bucketsFor(win(q), span, offset, now)
    const series = Object.fromEntries(COUNT_KEYS.filter((k) => typeof src[0]?.[k] === 'number').map((k) => [k, resample(src.map((p) => p[k]), srcHours, dstHours, span, offset)]))
    const live = src.map((p, i) => (p.count > 0 ? i : -1)).filter((i) => i >= 0)
    out.buckets = buckets
    out.points = buckets.map((id, i) => {
      const k = span - 1 - i
      const pos = Math.floor(((k + offset + 0.5) * dstHours) / srcHours) % src.length
      const from = src.length - 1 - pos
      const pick = live.length ? live.reduce((a, b) => (Math.abs(b - from) < Math.abs(a - from) ? b : a)) : -1
      const p: Obj = { id }
      for (const key of COUNT_KEYS) if (series[key]) p[key] = series[key]![i]
      if (pick >= 0 && p.count > 0) for (const [key, v] of Object.entries(src[pick]!)) if (!(key in p) && key !== 'id') p[key] = v
      return p
    })
  }
  return out
}

/** Builds a response for a query no fixture covers from the nearest fixture, rescaled to the requested range. */
export function synthesise(fx: Fixture, body: Obj, query: URLSearchParams, now: number): Obj {
  const q = normaliseQuery(fx.path, query)
  switch (fx.path) {
    case '/admin/api/usage/series':
      return synthSeries(body, fx, q, query, now)
    case '/admin/api/usage/totals': {
      const limit = Number(query.get('limit') ?? 0)
      return synthTotals(body, q, limit > 0 ? limit : null)
    }
    case '/admin/api/usage/models':
      return synthModels(body, q, query)
    case '/admin/api/performance':
      return synthPerformance(body, q, now)
    default:
      return body
  }
}

/** Remaps series scopes onto the ones requested when a same-class fixture answered, so each requested id gets a series. */
export function fitSeriesScopes(body: Obj, requested: URLSearchParams): Obj {
  const want = requested.getAll('scope')
  const pool: Obj[] = body.series
  if (want.length === pool.length && want.every((s, i) => s === pool[i]!.scope)) return body
  const series = want.map((scope) => {
    const { src, scale } = pickSeries(pool, scope)
    return scale === 1 && src.scope === scope ? src : { scope, points: (src.points as number[]).map((v) => Math.round(v * scale)) }
  })
  return { ...body, series }
}
