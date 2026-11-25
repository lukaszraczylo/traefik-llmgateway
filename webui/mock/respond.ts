import { matchFixture, nearestFixture, normaliseQuery, type Fixture } from './match.ts'
import { applyScenarios, errorEnvelope, scenarioDelayMs } from './scenarios.ts'
import { fitSeriesScopes, synthesise } from './synth.ts'
import { CAPTURE_AT, shiftTimestamps } from './time-shift.ts'

type Obj = Record<string, any> // eslint-disable-line @typescript-eslint/no-explicit-any

export interface RespondInput {
  manifest: Fixture[]
  readFixture: (file: string) => unknown
  path: string
  query: URLSearchParams
  scenarios: string[]
  now: number
  rng: () => number
}

export interface MockResponse {
  status: number
  body: unknown
  delayMs: number
}

const SERIES = '/admin/api/usage/series'
const MODELS = '/admin/api/usage/models'
const SERIES_SCOPE = /^(model|provider|group|user):.+$|^total$/
const WINDOWS = ['hour', 'day', 'month']
const JITTER = 0.15
const RANGE_TOO_LARGE = 'range too large; narrow span or filter'
/** Spans at which the live API rejects usage/models?detail=1. */
const DETAIL_MAX_SPAN: Record<string, number> = { hour: 48, day: 30 }

const notFound = (message: string) => ({ status: 404, body: errorEnvelope(404, 'invalid_request_error', message) })

function validQuery(path: string, query: URLSearchParams): string | null {
  const window = query.get('window')
  if (window !== null && !WINDOWS.includes(window)) return 'invalid window'
  const span = query.get('span')
  if (span !== null && !(Number(span) >= 1)) return 'invalid span'
  if (path === SERIES) {
    const scopes = query.getAll('scope')
    if (scopes.length === 0 || scopes.some((s) => !SERIES_SCOPE.test(s))) return 'unknown scope'
  }
  return null
}

function limitRows(path: string, query: URLSearchParams, body: Obj): Obj {
  const limit = Number(query.get('limit') ?? 0)
  if (!(limit > 0)) return body
  if (path === '/admin/api/events') return { ...body, events: body.events.slice(0, limit) }
  if (path === '/admin/api/usage/totals') return { ...body, rows: body.rows.slice(0, limit) }
  if (path === MODELS) return { ...body, models: body.models.slice(0, limit) }
  return body
}

function resolveBase(i: RespondInput): { status: number; body: unknown } {
  const { manifest, path, query, now } = i
  if (!manifest.some((f) => f.path === path)) return notFound(`unknown admin route ${path}`)
  const bad = validQuery(path, query)
  if (bad) return notFound(bad)

  const exact = matchFixture(manifest, path, query)
  if (exact && exact.status !== 200) return { status: exact.status, body: i.readFixture(exact.file) }
  const detail = query.get('detail')
  const window = query.get('window') ?? ''
  if (path === MODELS && detail && Number(query.get('span')) >= (DETAIL_MAX_SPAN[window] ?? Infinity)) {
    return { status: 400, body: errorEnvelope(400, 'invalid_request_error', RANGE_TOO_LARGE) }
  }

  const want = normaliseQuery(path, query)
  if (exact) {
    const have = normaliseQuery(path, exact.query)
    const sameRange = ['window', 'span', 'offset', 'metric', 'metrics'].every((k) => (want[k] ?? []).join() === (have[k] ?? []).join())
    if (sameRange) {
      let body = shiftTimestamps(i.readFixture(exact.file), CAPTURE_AT, now) as Obj
      if (path === SERIES) body = fitSeriesScopes(body, query)
      return { status: 200, body: limitRows(path, query, body) }
    }
  }
  const near = exact ?? nearestFixture(manifest, path, query)
  if (!near) return notFound('no data for this query')
  return { status: 200, body: limitRows(path, query, synthesise(near, i.readFixture(near.file) as Obj, query, now)) }
}

const wobble = (v: number, rng: () => number, cap?: number) => {
  const next = Math.round(v * (1 - JITTER + rng() * 2 * JITTER))
  return cap !== undefined ? Math.min(next, cap) : next
}

/** Live minute meters move on every call; counters that are zero stay zero. */
export function jitterMeters(path: string, body: unknown, rng: () => number): unknown {
  if (path !== '/admin/api/overview' && path !== '/admin/api/usage') return body
  const b = structuredClone(body) as Obj
  if (path === '/admin/api/overview') {
    for (const p of b.providers ?? []) {
      p.attemptsMinute = wobble(p.attemptsMinute, rng)
      p.failuresMinute = Math.min(wobble(p.failuresMinute, rng), p.attemptsMinute)
    }
  } else {
    for (const u of b.users ?? []) u.requestsPerMinute = wobble(u.requestsPerMinute, rng, u.limits?.requestsPerMinute || undefined)
    for (const g of b.groups ?? []) g.requestsPerMinute = wobble(g.requestsPerMinute, rng)
    if (b.total) b.total.requestsPerMinute = wobble(b.total.requestsPerMinute, rng)
  }
  return b
}

/** One mock admin API response: fixture or synthesis, then scenarios, then meter jitter. */
export function respond(i: RespondInput): MockResponse {
  const base = resolveBase(i)
  const out = applyScenarios(i.scenarios, i.path, base.body, base.status, { now: i.now, rng: i.rng })
  const body = out.status === 200 ? jitterMeters(i.path, out.body, i.rng) : out.body
  return { status: out.status, body, delayMs: scenarioDelayMs(i.scenarios) }
}
