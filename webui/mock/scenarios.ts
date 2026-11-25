type Obj = Record<string, any> // eslint-disable-line @typescript-eslint/no-explicit-any

export const SCENARIOS = [
  'live',
  'breaker-open',
  'redis-down',
  'config-warnings',
  'router-escalations',
  'targets-healthy',
  'stats-disabled',
  'auth-401',
  'auth-403',
  'empty',
  'limits-hot',
  'slow',
  'huge',
] as const

export type ScenarioName = (typeof SCENARIOS)[number]

export const SLOW_DELAY_MS = 1500

const API = '/admin/api/'
const HISTORY_PATHS = ['/admin/api/usage/series', '/admin/api/usage/totals', '/admin/api/usage/models']
const LATENCY_KEYS = ['p50Ms', 'p95Ms', 'p99Ms', 'ttfbP50Ms', 'ttfbP95Ms']
const HOT_USER_IDS = ['user-01', 'user-02', 'user-03']
const HOT_GROUP = 'friends'
const HOT_MINUTE_FRACTION = 0.93
const HOT_DAY_FRACTION = 0.9
const BREAKER_OPEN_MS = 90_000
const HUGE_FACTOR = 3

export interface ScenarioContext {
  now: number
  rng: () => number
}

export interface ScenarioResult {
  status: number
  body: unknown
}

const ZERO_TIME = '0001-01-01T00:00:00Z'
const BREAKER_PROVIDER = 'alibaba'
const WARNINGS = [
  'provider "alibaba": baseUrl has no scheme; assuming https',
  'modelAliases: "deepseek" targets a model no provider lists',
  'groups.friends.memberLimits.tokensPerMinute is below requestsPerMinute*1000; pacing may never start',
  'users file lists "user-13" twice; the last entry wins',
  'cache.ttl 5m0s is shorter than the router session ttl; session affinity can miss',
]
const WARNINGS_DROPPED = 3

/** Scenario names from `?mock=a,b` in a page URL (Referer) or the x-mock-scenario header; unknown names drop out. */
export function parseScenarios(referer: string | undefined, header: string | undefined): string[] {
  let raw = header ?? ''
  if (!raw && referer) {
    try {
      raw = new URL(referer).searchParams.get('mock') ?? ''
    } catch {
      raw = ''
    }
  }
  return raw
    .split(',')
    .map((s) => s.trim())
    .filter((s): s is ScenarioName => (SCENARIOS as readonly string[]).includes(s))
}

export function scenarioDelayMs(names: string[]): number {
  return names.includes('slow') ? SLOW_DELAY_MS : 0
}

export const errorEnvelope = (status: number, type: string, message: string) => ({
  error: { message, type, code: String(status) },
})

const fail = (status: number, type: string, message: string): ScenarioResult => ({ status, body: errorEnvelope(status, type, message) })

const iso = (ms: number) => new Date(ms).toISOString()

function eachRouter(body: Obj, fn: (r: Obj) => void) {
  if (Array.isArray(body.routers)) body.routers.forEach(fn)
}

function pickBreakerProvider(providers: Obj[]): Obj | undefined {
  return providers.find((p) => p.name === BREAKER_PROVIDER) ?? providers[0]
}

function breakerOpen(path: string, body: Obj, ctx: ScenarioContext) {
  if (path !== '/admin/api/overview' && path !== '/admin/api/catalog') return
  const p = pickBreakerProvider(body.providers ?? [])
  if (!p) return
  for (const other of body.providers) {
    other.healthState = 'closed'
    other.openUntil = ZERO_TIME
  }
  p.healthState = 'open'
  p.openUntil = iso(ctx.now + BREAKER_OPEN_MS)
  p.lastErr = 'model discovery failed: Get "/v1/models": context deadline exceeded'
  if (path === '/admin/api/overview') {
    p.attemptsMinute = Math.max(p.attemptsMinute ?? 0, 12)
    p.failuresMinute = p.attemptsMinute
  }
}

function redisDown(path: string, body: Obj, ctx: ScenarioContext): ScenarioResult | void {
  if (HISTORY_PATHS.includes(path)) return fail(503, 'server_error', 'usage history store unavailable')
  if (path === '/admin/api/overview') {
    body.redis = { configured: true, lastErrAt: iso(ctx.now - 4000), lastErr: 'dial tcp: connect: connection refused' }
  } else if (path === '/admin/api/events') {
    body.source = 'replica'
    body.degraded = true
  }
}

function configWarnings(path: string, body: Obj) {
  if (path === '/admin/api/overview') {
    body.warnings = [...WARNINGS]
    body.warningsDropped = WARNINGS_DROPPED
  } else if (path === '/admin/api/config') {
    body.warnings = [...WARNINGS]
  }
}

function routerEscalations(path: string, body: Obj) {
  if (path !== '/admin/api/overview' && path !== '/admin/api/catalog') return
  eachRouter(body, (r) => {
    const tiers: string[] = (r.tiers ?? []).map((t: Obj) => t.name)
    if (tiers.length < 2) return
    const stats = (r.stats ??= { scope: 'shared', tiers: [], reasons: [], outcomes: [], requests: 0 })
    stats.escalations = tiers.slice(0, -1).map((from, i) => ({ from, to: tiers[i + 1], count: 14 - i * 5 }))
    if (tiers.length > 2) stats.escalations.push({ from: tiers[0], to: tiers[tiers.length - 1], count: 3 })
    stats.shadow = tiers.flatMap((live, i) => tiers.map((shadow, j) => ({ live, shadow, count: i === j ? 40 - i * 9 : 4 + ((i + j) % 3) * 3 })))
    stats.requests = Math.max(stats.requests ?? 0, 120)
    r.shadowEnabled = true
    r.shadowClassifier ??= 'v2'
    r.fellBack = 7
  })
}

function targetsHealthy(path: string, body: Obj, ctx: ScenarioContext) {
  if (path !== '/admin/api/targets') return
  for (const t of [...(body.mcpServers ?? []), ...(body.agents ?? [])]) {
    t.health = { state: 'healthy', consecutiveFailures: 0, lastCheck: iso(ctx.now - 3000), latencyMs: 18 + (t.name.length % 40), source: 'traffic' }
  }
}

function statsDisabled(path: string, body: Obj, status: number): ScenarioResult | void {
  if (path === '/admin/api/usage/totals' && body.kind === 'usermodel') {
    return fail(404, 'invalid_request_error', 'user/model stats are not enabled')
  }
  if (status !== 200) return
  if (path === '/admin/api/overview') {
    Object.assign((body.features ??= {}), { userModelStats: false, latencyStats: false, cacheStats: false })
  } else if (path === '/admin/api/config') {
    const stats = (((body.config ??= {}).admin ??= {}).stats ??= {})
    stats.latency = false
    stats.userModel = false
    if (body.features) Object.assign(body.features, { userModelStats: false, latencyStats: false, cacheStats: false })
  } else if (path === '/admin/api/performance') {
    body.latencyEnabled = false
    for (const r of [...(body.rows ?? []), ...(body.points ?? [])]) for (const k of LATENCY_KEYS) delete r[k]
  }
}

const zeroCounters = (o: Obj) => {
  for (const k of Object.keys(o)) {
    if (/(PerMinute|PerDay|PerMonth|MicroUsd)$/.test(k) && typeof o[k] === 'number') o[k] = 0
  }
}

function empty(path: string, body: Obj) {
  switch (path) {
    case '/admin/api/usage/series':
      for (const s of body.series ?? []) s.points = s.points.map(() => 0)
      break
    case '/admin/api/usage/totals':
      body.rows = []
      break
    case '/admin/api/usage/models':
      body.models = []
      break
    case '/admin/api/events':
      body.events = []
      break
    case '/admin/api/performance':
      body.rows = []
      for (const p of body.points ?? []) for (const k of ['count', 'attempts', 'failures', ...LATENCY_KEYS]) if (k in p) p[k] = 0
      break
    case '/admin/api/usage':
      for (const o of [...(body.users ?? []), ...(body.groups ?? []), body.total ?? {}]) zeroCounters(o)
      break
    case '/admin/api/overview':
      for (const p of body.providers ?? []) zeroCounters(p)
      for (const p of body.providers ?? []) for (const k of ['attemptsDay', 'failuresDay', 'attemptsMinute', 'failuresMinute']) p[k] = 0
      break
    case '/admin/api/targets':
      for (const t of [...(body.mcpServers ?? []), ...(body.agents ?? [])]) zeroCounters(t.counters ?? {})
      break
  }
}

function limitsHot(path: string, body: Obj, ctx: ScenarioContext) {
  if (path === '/admin/api/usage') {
    const hot = (body.users as Obj[]).filter((u) => HOT_USER_IDS.includes(u.id) && (u.limits?.requestsPerMinute ?? 0) > 0)
    for (const u of hot) {
      u.requestsPerMinute = Math.floor(u.limits.requestsPerMinute * HOT_MINUTE_FRACTION)
      if (u.limits.requestsPerDay > 0) u.requestsPerDay = Math.floor(u.limits.requestsPerDay * HOT_DAY_FRACTION)
      u.rejectionsPerDay = Math.max(u.rejectionsPerDay ?? 0, 9)
    }
    const added = hot.reduce((s: number, u: Obj) => s + u.requestsPerMinute, 0)
    for (const g of body.groups ?? []) if (g.id === HOT_GROUP) g.requestsPerMinute += added
    if (body.total) body.total.requestsPerMinute += added
  } else if (path === '/admin/api/events') {
    const fresh = HOT_USER_IDS.flatMap((user, i) => [
      limitEvent(user, 'rate_limit', ctx.now - (i * 2 + 1) * 4000, 'requests per minute limit reached; request rejected', 429),
      limitEvent(user, 'paced', ctx.now - (i * 2 + 2) * 4000, `request delayed ${(1.5 + i * 0.7).toFixed(1)}s by pacing`),
    ])
    body.events = [...fresh, ...(body.events ?? [])]
  }
}

function limitEvent(user: string, kind: string, at: number, message: string, status?: number): Obj {
  return { time: iso(at), replica: 'mock-replica-1', instance: 'mock', user, group: HOT_GROUP, route: 'chat/completions', kind, message, ...(status ? { status } : {}) }
}

const copies = <T>(factor: number, make: (n: number) => T): T[] => Array.from({ length: factor }, (_, n) => make(n))
const suffix = (id: string, n: number) => (n === 0 ? id : `${id}-v${n + 1}`)

function huge(path: string, body: Obj) {
  if (path === '/admin/api/overview') {
    body.providers = (body.providers as Obj[]).flatMap((p) =>
      copies(HUGE_FACTOR, (n) => {
        const models = (p.models as string[]).flatMap((m) => copies(HUGE_FACTOR, (r) => suffix(m, r)))
        const grow = (src: Obj) => Object.fromEntries(Object.entries(src ?? {}).flatMap(([m, v]) => copies(HUGE_FACTOR, (r) => [suffix(m, r), v])))
        return { ...p, name: n === 0 ? p.name : `${p.name}-${n + 1}`, models, modelCount: models.length, modelMeta: grow(p.modelMeta), modelRates: grow(p.modelRates) }
      }),
    )
  } else if (path === '/admin/api/catalog') {
    body.providers = (body.providers as Obj[]).flatMap((p) =>
      copies(HUGE_FACTOR, (n) => {
        const name = n === 0 ? p.name : `${p.name}-${n + 1}`
        const models = (p.models as Obj[]).flatMap((m) => copies(HUGE_FACTOR, (r) => ({ ...m, model: suffix(m.model, r), id: `${name}/${suffix(m.model, r)}` })))
        return { ...p, name, models }
      }),
    )
  } else if (path === '/admin/api/usage/models' && Array.isArray(body.models)) {
    body.models = tripleRows(body.models)
  } else if (path === '/admin/api/performance' && Array.isArray(body.rows) && body.kind === 'model') {
    body.rows = tripleRows(body.rows)
  }
}

function tripleRows(rows: Obj[]): Obj[] {
  return rows.flatMap((r) => copies(HUGE_FACTOR, (n) => ({ ...r, id: suffix(r.id, n) })))
}

function live(path: string, body: Obj, ctx: ScenarioContext) {
  if (path === '/admin/api/events') {
    const kinds = ['upstream', 'capacity', 'unpriced']
    const kind = kinds[Math.floor(ctx.rng() * kinds.length)]!
    body.events = [{ time: iso(ctx.now), replica: body.replica ?? 'mock-replica-1', instance: 'mock', model: 'gx10/Qwen3.8-27B', route: 'chat/completions', kind, message: `live ${kind} event at ${iso(ctx.now)}` }, ...(body.events ?? [])]
  } else if (path === '/admin/api/usage') {
    for (const u of body.users ?? []) if (u.requestsPerDay > 0) u.requestsPerMinute = Math.max(u.requestsPerMinute, 3)
    body.total.requestsPerMinute = Math.max(body.total.requestsPerMinute, 24)
  } else if (path === '/admin/api/overview') {
    for (const p of body.providers ?? []) if (p.attemptsDay > 0) p.attemptsMinute = Math.max(p.attemptsMinute, 4)
  }
}

/** Applies the selected scenarios to one response. The input body is never mutated. */
export function applyScenarios(names: string[], path: string, body: unknown, status: number, ctx: ScenarioContext = { now: Date.now(), rng: Math.random }): ScenarioResult {
  if (!path.startsWith(API)) return { status, body }
  if (names.includes('auth-401')) return fail(401, 'authentication_error', 'invalid or missing API key')
  if (names.includes('auth-403')) return fail(403, 'invalid_request_error', 'admin access required')
  if (names.length === 0 || status !== 200) return { status, body }

  const b = structuredClone(body) as Obj
  const steps: ((p: string, b: Obj) => ScenarioResult | void)[] = []
  const on = (name: ScenarioName, step: (p: string, b: Obj) => ScenarioResult | void) => names.includes(name) && steps.push(step)
  on('stats-disabled', (p, x) => statsDisabled(p, x, status))
  on('redis-down', (p, x) => redisDown(p, x, ctx))
  on('empty', empty)
  on('huge', huge)
  on('breaker-open', (p, x) => breakerOpen(p, x, ctx))
  on('config-warnings', configWarnings)
  on('router-escalations', routerEscalations)
  on('targets-healthy', (p, x) => targetsHealthy(p, x, ctx))
  on('live', (p, x) => live(p, x, ctx))
  on('limits-hot', (p, x) => limitsHot(p, x, ctx))
  for (const step of steps) {
    const out = step(path, b)
    if (out) return out
  }
  return { status, body: b }
}
