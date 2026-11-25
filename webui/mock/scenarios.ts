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
const HOT_USERS = 3
const HOT_GROUP = 'friends'
const LIVE_TPM_MIN_FRACTION = 0.01
const LIVE_TPM_SPREAD_FRACTION = 0.03
const HOT_MINUTE_FRACTION = 0.93
const HOT_DAY_FRACTION = 0.9
const BREAKER_OPEN_MS = 90_000
const HUGE_FACTOR = 3
/** Each captured user becomes this many in the huge scenario. */
const HUGE_USER_FACTOR = 20
const HOT_TOKENS_IN_PER_REQUEST = 1500
const HOT_TOKENS_OUT_PER_REQUEST = 400

export interface ScenarioContext {
  now: number
  rng: () => number
  /** Users limits-hot targets on the events path; usage derives them itself, see hotUserIds. */
  hotUsers?: string[]
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

/** The first HOT_USERS members of the friends group that have a per-minute request limit. */
export function hotUserIds(usage: Obj): string[] {
  return (usage.users as Obj[])
    .filter((u) => (u.groups ?? [u.groupName]).includes(HOT_GROUP) && (u.limits?.requestsPerMinute ?? 0) > 0)
    .map((u) => u.id as string)
    .sort()
    .slice(0, HOT_USERS)
}

function addToGroupAndTotal(body: Obj, groupName: string | undefined, field: string, delta: number) {
  for (const g of body.groups ?? []) if (g.id === groupName) g[field] = (g[field] ?? 0) + delta
  if (body.total) body.total[field] = (body.total[field] ?? 0) + delta
}

function limitsHot(path: string, body: Obj, ctx: ScenarioContext) {
  if (path === '/admin/api/usage') {
    const ids = hotUserIds(body)
    for (const u of (body.users as Obj[]).filter((x) => ids.includes(x.id))) {
      const rpm = Math.floor(u.limits.requestsPerMinute * HOT_MINUTE_FRACTION)
      addToGroupAndTotal(body, HOT_GROUP, 'requestsPerMinute', rpm - u.requestsPerMinute)
      u.requestsPerMinute = rpm
      if (u.limits.tokensPerMinute > 0) {
        const tpm = Math.floor(u.limits.tokensPerMinute * HOT_MINUTE_FRACTION)
        addToGroupAndTotal(body, HOT_GROUP, 'tokensPerMinute', tpm - (u.tokensPerMinute ?? 0))
        u.tokensPerMinute = tpm
      }
      if (u.limits.requestsPerDay > 0) {
        const day = Math.floor(u.limits.requestsPerDay * HOT_DAY_FRACTION)
        addToGroupAndTotal(body, HOT_GROUP, 'requestsPerDay', day - u.requestsPerDay)
        u.requestsPerDay = day
      }
      const tokensIn = u.requestsPerDay * HOT_TOKENS_IN_PER_REQUEST
      const tokensOut = u.requestsPerDay * HOT_TOKENS_OUT_PER_REQUEST
      addToGroupAndTotal(body, HOT_GROUP, 'tokensInPerDay', tokensIn - u.tokensInPerDay)
      addToGroupAndTotal(body, HOT_GROUP, 'tokensOutPerDay', tokensOut - u.tokensOutPerDay)
      u.tokensInPerDay = tokensIn
      u.tokensOutPerDay = tokensOut
      u.tokensInPerMonth = Math.max(u.tokensInPerMonth ?? 0, tokensIn)
      u.tokensOutPerMonth = Math.max(u.tokensOutPerMonth ?? 0, tokensOut)
      u.lastSeen = Math.floor(ctx.now / 1000)
      u.rejectionsPerDay = Math.max(u.rejectionsPerDay ?? 0, 9)
    }
  } else if (path === '/admin/api/events') {
    const fresh = (ctx.hotUsers ?? []).flatMap((user, i) => [
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
const hugeProvider = (name: string, n: number) => (n === 0 ? name : `${name}-${n + 1}`)

/** Every "provider/model" id the huge overview and catalog list for one captured id. */
function hugeModelIds(id: string): string[] {
  const slash = id.indexOf('/')
  const provider = slash < 0 ? '' : id.slice(0, slash)
  const model = id.slice(slash + 1)
  return copies(HUGE_FACTOR, (n) => copies(HUGE_FACTOR, (r) => `${provider ? `${hugeProvider(provider, n)}/` : ''}${suffix(model, r)}`)).flat()
}

const expandRows = (rows: Obj[], ids: (id: string) => string[]): Obj[] => rows.flatMap((r) => ids(r.id).map((id) => ({ ...r, id })))

/** A clone of one user with a distinct id and a spread of traffic, so rankings and shares differ. */
function hugeUser(u: Obj, n: number): Obj {
  const scale = 1 / (n + 1)
  const scaled = (v: unknown) => (typeof v === 'number' ? Math.floor(v * scale) : v)
  return { ...u, id: `${u.id}-${n + 1}`, requestsPerDay: scaled(u.requestsPerDay), tokensInPerDay: scaled(u.tokensInPerDay), tokensOutPerDay: scaled(u.tokensOutPerDay) }
}

function huge(path: string, body: Obj) {
  if (path === '/admin/api/overview') {
    body.providers = (body.providers as Obj[]).flatMap((p) =>
      copies(HUGE_FACTOR, (n) => {
        const models = (p.models as string[]).flatMap((m) => copies(HUGE_FACTOR, (r) => suffix(m, r)))
        const grow = (src: Obj) => Object.fromEntries(Object.entries(src ?? {}).flatMap(([m, v]) => copies(HUGE_FACTOR, (r) => [suffix(m, r), v])))
        return { ...p, name: hugeProvider(p.name, n), models, modelCount: models.length, modelMeta: grow(p.modelMeta), modelRates: grow(p.modelRates) }
      }),
    )
  } else if (path === '/admin/api/catalog') {
    body.providers = (body.providers as Obj[]).flatMap((p) =>
      copies(HUGE_FACTOR, (n) => {
        const name = hugeProvider(p.name, n)
        const models = (p.models as Obj[]).flatMap((m) => copies(HUGE_FACTOR, (r) => ({ ...m, model: suffix(m.model, r), id: `${name}/${suffix(m.model, r)}` })))
        return { ...p, name, models }
      }),
    )
  } else if (path === '/admin/api/usage' && Array.isArray(body.users)) {
    body.users = (body.users as Obj[]).flatMap((u) => copies(HUGE_USER_FACTOR, (n) => (n === 0 ? u : hugeUser(u, n))))
  } else if (path === '/admin/api/consumers' && Array.isArray(body.users)) {
    body.users = (body.users as Obj[]).flatMap((u) => copies(HUGE_USER_FACTOR, (n) => (n === 0 ? u : { ...u, name: `${u.name}-${n + 1}` })))
  } else if (path === '/admin/api/usage/models' && Array.isArray(body.models)) {
    body.models = expandRows(body.models, hugeModelIds)
  } else if (path === '/admin/api/performance' && Array.isArray(body.rows)) {
    body.rows = expandRows(body.rows, body.kind === 'provider' ? (id) => copies(HUGE_FACTOR, (n) => hugeProvider(id, n)) : hugeModelIds)
  } else if (path === '/admin/api/usage/totals' && body.kind === 'usermodel') {
    body.rows = expandRows(body.rows, hugeModelIds)
  }
}

function live(path: string, body: Obj, ctx: ScenarioContext) {
  if (path === '/admin/api/events') {
    const kinds = ['upstream', 'capacity', 'unpriced']
    const kind = kinds[Math.floor(ctx.rng() * kinds.length)]!
    body.events = [{ time: iso(ctx.now), replica: body.replica ?? 'mock-replica-1', instance: 'mock', model: 'gx10/Qwen3.8-27B', route: 'chat/completions', kind, message: `live ${kind} event at ${iso(ctx.now)}` }, ...(body.events ?? [])]
  } else if (path === '/admin/api/usage') {
    for (const u of body.users ?? []) if (u.requestsPerDay > 0) u.requestsPerMinute = Math.max(u.requestsPerMinute, 3)
    body.total.requestsPerMinute = Math.max(body.total.requestsPerMinute, 24)
    for (const u of body.users ?? []) {
      const limit = u.limits?.tokensPerMinute ?? 0
      if (limit <= 0 || !(u.requestsPerDay > 0)) continue
      u.tokensPerMinute = Math.round(limit * (LIVE_TPM_MIN_FRACTION + ctx.rng() * LIVE_TPM_SPREAD_FRACTION))
      addToGroupAndTotal(body, u.groupName, 'tokensPerMinute', u.tokensPerMinute)
    }
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
