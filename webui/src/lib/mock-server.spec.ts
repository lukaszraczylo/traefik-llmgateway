import { describe, expect, it } from 'vitest'

import manifestJson from '../../mock/manifest.json'
import { matchFixture, type Fixture } from '../../mock/match'
import { isDetail, respond, stripDetailOnly } from '../../mock/respond'
import { applyScenarios, hotUserIds, SCENARIOS, scenarioDelayMs, parseScenarios } from '../../mock/scenarios'
import { bucketsFor, CAPTURE_AT, shiftTimestamps } from '../../mock/time-shift'

const manifest = manifestJson as Fixture[]

const raw = import.meta.glob('../../mock/fixtures/*.json', { query: '?raw', import: 'default', eager: true }) as Record<string, string>
const readFixture = (file: string): unknown => JSON.parse(raw[`../../mock/fixtures/${file}`] as string)

const NOW = CAPTURE_AT + 3 * 86_400_000 + 5 * 3_600_000
const q = (s: string) => new URLSearchParams(s)
const get = (path: string, query = '', scenarios: string[] = [], now = NOW) =>
  respond({ manifest, readFixture, path, query: q(query), scenarios, now, rng: () => 0.5 })

// biome-ignore lint/suspicious/noExplicitAny: fixtures are untyped JSON
type Any = any

describe('matchFixture', () => {
  it('ignores key order', () => {
    const a = matchFixture(manifest, '/admin/api/usage/totals', q('kind=group&window=day&span=14&metrics=cost'))
    const b = matchFixture(manifest, '/admin/api/usage/totals', q('metrics=cost&span=14&window=day&kind=group'))
    expect(a).not.toBeNull()
    expect(b?.file).toBe(a?.file)
  })

  it('ignores repeated-key order', () => {
    const f = manifest.find((m) => m.path.endsWith('/series') && Array.isArray(m.query.scope) && m.query.scope.length === 2) as Fixture
    const scopes = f.query.scope as string[]
    const mk = (list: string[]) => {
      const p = new URLSearchParams()
      for (const s of list) p.append('scope', s)
      for (const [k, v] of Object.entries(f.query)) if (k !== 'scope') p.set(k, v as string)
      return p
    }
    expect(matchFixture(manifest, f.path, mk([...scopes].reverse()))?.file).toBe(f.file)
  })

  it('returns null for unknown path', () => {
    expect(matchFixture(manifest, '/admin/api/nope', q(''))).toBeNull()
  })

  it('matches on value class for an id not in the fixtures', () => {
    const hit = matchFixture(manifest, '/admin/api/usage/totals', q('kind=usermodel&window=day&span=14&user=user-99'))
    expect(hit?.query.kind).toBe('usermodel')
  })

  it('every manifest entry resolves to a fixture with its own status', () => {
    for (const f of manifest) {
      const p = new URLSearchParams()
      for (const [k, v] of Object.entries(f.query)) for (const x of Array.isArray(v) ? v : [v]) p.append(k, x)
      const hit = matchFixture(manifest, f.path, p)
      expect(hit, f.file).not.toBeNull()
      expect(hit?.status, f.file).toBe(f.status)
    }
  })
})

describe('shiftTimestamps', () => {
  it('moves newest bucket to now', () => {
    const entry = manifest.find((m) => m.path.endsWith('/series') && m.query.window === 'day' && m.query.span === '14' && !m.query.offset)!
    const body = readFixture(entry.file) as Any
    const shifted = shiftTimestamps(body, CAPTURE_AT, NOW) as Any
    expect(shifted.buckets.at(-1)).toBe(bucketsFor('day', 1, 0, NOW)[0])
    expect(shifted.buckets).toHaveLength(body.buckets.length)
    expect(shifted.series[0].points).toEqual(body.series[0].points)
  })

  it('shifts ISO timestamps by the delta and leaves the zero time alone', () => {
    const out = shiftTimestamps({ time: '2026-11-25T00:47:17.590946814Z', openUntil: '0001-01-01T00:00:00Z' }, CAPTURE_AT, CAPTURE_AT + 60_000) as Any
    expect(Date.parse(out.time)).toBe(Date.parse('2026-11-25T00:48:17.590Z'))
    expect(out.openUntil).toBe('0001-01-01T00:00:00Z')
  })
})

describe('respond', () => {
  it('serves an exact fixture with buckets ending now', () => {
    const r = get('/admin/api/usage/series', 'scope=group:friends&metric=cost&window=day&span=14')
    expect(r.status).toBe(200)
    expect((r.body as Any).buckets.at(-1)).toBe(bucketsFor('day', 1, 0, NOW)[0])
  })

  it('404s an unknown query with the real error envelope', () => {
    const r = get('/admin/api/performance', 'kind=bogus&window=day&span=7')
    expect(r.status).toBe(404)
    expect(r.body).toEqual({ error: { message: expect.any(String), type: 'invalid_request_error', code: '404' } })
  })

  it('ranks usage/models by the requested metric, not by a top-N cut of another one', () => {
    const r = get('/admin/api/usage/models', 'metric=req&window=day&span=7&limit=5')
    const ids = ((r.body as Any).models as Any[]).map((m) => m.id)
    expect(ids[0]).toBe('uni/text-embedding-multilingual-e5-base')
    expect(ids).toHaveLength(5)
    expect(ids).toContain('uni/deepseek-v4-flash-0731')
  })

  it('404s an unknown path', () => {
    expect(get('/admin/api/nope').status).toBe(404)
  })

  it('400s detail=1 at day/30 for any metric', () => {
    const r = get('/admin/api/usage/models', 'metric=tokout&window=day&span=30&limit=100&detail=1')
    expect(r.status).toBe(400)
    expect((r.body as Any).error.message).toContain('range too large')
  })

  it('synthesises performance month from day data with aligned buckets', () => {
    const r = get('/admin/api/performance', 'kind=provider&window=month&span=3&series=1&id=alibaba')
    expect(r.status).toBe(200)
    const b = r.body as Any
    expect(b.window).toBe('month')
    expect(b.span).toBe(3)
    expect(b.buckets).toEqual(bucketsFor('month', 3, 0, NOW))
    expect(b.points).toHaveLength(3)
    expect(b.points.map((p: Any) => p.id)).toEqual(b.buckets)
  })

  it('synthesises performance month rows scaled up from day rows', () => {
    const day = (get('/admin/api/performance', 'kind=provider&window=day&span=14').body as Any).rows[0]
    const month = (get('/admin/api/performance', 'kind=provider&window=month&span=3').body as Any).rows[0]
    expect(month.count).toBeGreaterThan(day.count * 2)
    expect(month.p50Ms).toBe(day.p50Ms)
  })

  it('synthesises hour-window usermodel totals from day data', () => {
    const r = get('/admin/api/usage/totals', 'kind=usermodel&window=hour&span=24&user=admin')
    expect(r.status).toBe(200)
    const b = r.body as Any
    expect(b.window).toBe('hour')
    expect(b.span).toBe(24)
    const dayReq = (get('/admin/api/usage/totals', 'kind=usermodel&window=day&span=14&user=admin').body as Any).rows.reduce((s: number, x: Any) => s + x.values.req, 0)
    const hourReq = b.rows.reduce((s: number, x: Any) => s + x.values.req, 0)
    expect(hourReq).toBeLessThan(dayReq)
  })

  it('synthesises a missing series combination to the requested span and bucket count', () => {
    const r = get('/admin/api/usage/series', 'scope=group:friends&scope=group:home&metric=req&window=hour&span=36')
    expect(r.status).toBe(200)
    const b = r.body as Any
    expect(b.buckets).toEqual(bucketsFor('hour', 36, 0, NOW))
    expect(b.series.map((s: Any) => s.scope).sort()).toEqual(['group:friends', 'group:home'])
    for (const s of b.series) expect(s.points).toHaveLength(36)
  })

  it('never serves a different range verbatim', () => {
    const a = (get('/admin/api/usage/series', 'scope=group:friends&metric=cost&window=day&span=14').body as Any).series[0].points
    const b = (get('/admin/api/usage/series', 'scope=group:friends&metric=cost&window=day&span=20').body as Any).series[0].points
    expect(b).toHaveLength(20)
    expect(b.slice(-14)).toEqual(a)
  })

  it('applies offset to synthesised buckets', () => {
    const b = get('/admin/api/usage/series', 'scope=group:friends&metric=cost&window=day&span=10&offset=10').body as Any
    expect(b.buckets).toEqual(bucketsFor('day', 10, 10, NOW))
    expect(b.offset).toBe(10)
  })

  it('slices events to the requested limit', () => {
    expect((get('/admin/api/events', 'limit=20').body as Any).events).toHaveLength(20)
  })

  it('jitters live minute meters between calls', () => {
    let n = 0
    const rng = () => (n++ * 0.37) % 1
    const call = () => respond({ manifest, readFixture, path: '/admin/api/usage', query: q(''), scenarios: ['live'], now: NOW, rng }).body as Any
    expect(call().total.requestsPerMinute).not.toBe(call().total.requestsPerMinute)
  })

  it('adds 1500ms with slow and sets auth status through scenarios', () => {
    expect(get('/admin/api/overview', '', ['slow']).delayMs).toBe(1500)
    expect(get('/admin/api/overview', '', ['auth-401']).status).toBe(401)
  })
})

describe('scenarios', () => {
  const overview = () => readFixture('overview.json') as Any
  const NOWS = { now: NOW, rng: () => 0.5 }
  const apply = (names: string[], path: string, body: unknown, status = 200) => applyScenarios(names, path, body, status, NOWS)

  it('lists every documented scenario', () => {
    expect([...SCENARIOS]).toEqual([
      'live', 'breaker-open', 'redis-down', 'config-warnings', 'router-escalations', 'targets-healthy',
      'stats-disabled', 'auth-401', 'auth-403', 'empty', 'limits-hot', 'slow', 'huge',
    ])
  })

  it('parses scenarios from referer and header', () => {
    expect(parseScenarios('http://localhost:5173/admin/?mock=empty,slow', undefined)).toEqual(['empty', 'slow'])
    expect(parseScenarios(undefined, 'huge, bogus')).toEqual(['huge'])
    expect(parseScenarios(undefined, undefined)).toEqual([])
  })

  it('breaker-open sets healthState open with future openUntil on one provider', () => {
    const out = apply(['breaker-open'], '/admin/api/overview', overview()).body as Any
    const open = out.providers.filter((p: Any) => p.healthState === 'open')
    expect(open).toHaveLength(1)
    expect(Date.parse(open[0].openUntil)).toBeGreaterThan(NOW)
    expect(open[0].lastErr).toBeTruthy()
  })

  it('breaker-open also marks the catalog provider', () => {
    const out = apply(['breaker-open'], '/admin/api/catalog', readFixture('catalog.json')).body as Any
    expect(out.providers.filter((p: Any) => p.healthState === 'open')).toHaveLength(1)
  })

  it('redis-down returns 503 on usage/series', () => {
    const out = apply(['redis-down'], '/admin/api/usage/series', { buckets: [] })
    expect(out.status).toBe(503)
    expect((out.body as Any).error.message).toBe('usage history store unavailable')
  })

  it('redis-down surfaces the redis error in overview and degrades events', () => {
    const o = apply(['redis-down'], '/admin/api/overview', overview()).body as Any
    expect(o.redis.lastErr).toBeTruthy()
    const e = apply(['redis-down'], '/admin/api/events', readFixture('events__limit-200.json')).body as Any
    expect(e.source).toBe('replica')
    expect(e.degraded).toBe(true)
  })

  it('config-warnings fills warnings on overview and config', () => {
    const o = apply(['config-warnings'], '/admin/api/overview', overview()).body as Any
    expect(o.warnings.length).toBeGreaterThan(2)
    expect(o.warningsDropped).toBeGreaterThan(0)
    expect((apply(['config-warnings'], '/admin/api/config', readFixture('config.json')).body as Any).warnings.length).toBeGreaterThan(2)
  })

  it('router-escalations fills stats.escalations, stats.shadow and fellBack', () => {
    const o = apply(['router-escalations'], '/admin/api/catalog', readFixture('catalog.json')).body as Any
    const r = o.routers[0]
    expect(r.stats.escalations.length).toBeGreaterThan(0)
    expect(r.stats.shadow.length).toBeGreaterThan(0)
    expect(r.fellBack).toBeGreaterThan(0)
    expect(r.shadowEnabled).toBe(true)
  })

  it('targets-healthy marks every target healthy', () => {
    const t = apply(['targets-healthy'], '/admin/api/targets', readFixture('targets.json')).body as Any
    for (const x of [...t.mcpServers, ...t.agents]) {
      expect(x.health.state).toBe('healthy')
      expect(x.health.consecutiveFailures).toBe(0)
    }
  })

  it('stats-disabled turns the features off and 404s usermodel totals', () => {
    const o = apply(['stats-disabled'], '/admin/api/overview', overview()).body as Any
    expect(o.features).toMatchObject({ userModelStats: false, latencyStats: false, cacheStats: false })
    expect(apply(['stats-disabled'], '/admin/api/usage/totals', { kind: 'usermodel', rows: [] }, 200)).toMatchObject({ status: 404 })
    const p = apply(['stats-disabled'], '/admin/api/performance', readFixture('performance__kind-model_window-day_span-14.json')).body as Any
    expect(p.latencyEnabled).toBe(false)
    expect(p.rows[0].p50Ms).toBeUndefined()
  })

  it('auth-401 returns 401 for every api path', () => {
    for (const p of new Set(manifest.map((m) => m.path))) {
      const out = apply(['auth-401'], p, {})
      expect(out.status, p).toBe(401)
      expect((out.body as Any).error.type).toBe('authentication_error')
    }
  })

  it('auth-403 returns 403 for every api path', () => {
    expect(apply(['auth-403'], '/admin/api/overview', {}).status).toBe(403)
  })

  it('empty zeroes series and empties lists', () => {
    const s = apply(['empty'], '/admin/api/usage/series', readFixture(manifest.find((m) => m.path.endsWith('/series'))!.file)).body as Any
    expect(s.series.length).toBeGreaterThan(0)
    for (const x of s.series) expect(x.points.every((n: number) => n === 0)).toBe(true)
    expect((apply(['empty'], '/admin/api/usage/totals', { rows: [{ id: 'a', values: { req: 1 } }] }).body as Any).rows).toEqual([])
    expect((apply(['empty'], '/admin/api/usage/models', { models: [{ id: 'a' }] }).body as Any).models).toEqual([])
    expect((apply(['empty'], '/admin/api/events', readFixture('events__limit-200.json')).body as Any).events).toEqual([])
    const u = apply(['empty'], '/admin/api/usage', readFixture('usage.json')).body as Any
    expect(u.total.requestsPerDay).toBe(0)
    expect(u.users.every((x: Any) => x.requestsPerDay === 0)).toBe(true)
  })

  it('limits-hot sets requestsPerMinute near limit for 3 friends users and adds rate_limit/paced events', () => {
    const usage = readFixture('usage.json') as Any
    const u = apply(['limits-hot'], '/admin/api/usage', usage).body as Any
    const hot = u.users.filter((x: Any) => x.limits?.requestsPerMinute > 0 && x.requestsPerMinute >= x.limits.requestsPerMinute * 0.8)
    expect(hot).toHaveLength(3)
    expect(hot.map((h: Any) => h.id).sort()).toEqual(hotUserIds(usage))
    for (const h of hot) {
      expect(h.groupName).toBe('friends')
      expect(h.requestsPerMinute).toBeLessThanOrEqual(h.limits.requestsPerMinute)
    }
    const e = applyScenarios(['limits-hot'], '/admin/api/events', readFixture('events__limit-200.json'), 200, { ...NOWS, hotUsers: hotUserIds(usage) }).body as Any
    const kinds = new Set(e.events.map((x: Any) => x.kind))
    expect(kinds.has('rate_limit')).toBe(true)
    expect(kinds.has('paced')).toBe(true)
    expect(e.events.filter((x: Any) => hot.some((h: Any) => h.id === x.user)).length).toBeGreaterThan(0)
  })

  it('derives the hot users from the friends group members', () => {
    const usage = { users: [
      { id: 'b', groupName: 'home', groups: ['home'], limits: { requestsPerMinute: 5 } },
      { id: 'z', groupName: 'friends', groups: ['friends'], limits: { requestsPerMinute: 5 } },
      { id: 'c', groupName: 'friends', groups: ['friends'], limits: { requestsPerMinute: 5 } },
      { id: 'n', groupName: 'friends', groups: ['friends'], limits: {} },
      { id: 'a', groupName: 'friends', groups: ['friends'], limits: { requestsPerMinute: 5 } },
      { id: 'd', groupName: 'friends', groups: ['friends'], limits: { requestsPerMinute: 5 } },
    ] }
    expect(hotUserIds(usage)).toEqual(['a', 'c', 'd'])
  })

  it('limits-hot sets live tokensPerMinute near the limit and sums it into group and total', () => {
    const usage = readFixture('usage.json') as Any
    const u = apply(['limits-hot'], '/admin/api/usage', usage).body as Any
    const hot = u.users.filter((x: Any) => hotUserIds(usage).includes(x.id))
    for (const h of hot) {
      expect(h.tokensPerMinute).toBe(Math.floor(h.limits.tokensPerMinute * 0.93))
      expect(h.tokensPerMinute).toBeLessThanOrEqual(h.limits.tokensPerMinute)
    }
    const sum = hot.reduce((n: number, h: Any) => n + h.tokensPerMinute, 0)
    expect(u.groups.find((g: Any) => g.id === 'friends').tokensPerMinute).toBe(sum)
    expect(u.total.tokensPerMinute).toBe(sum)
  })

  it('live gives users with a tokensPerMinute limit a small value and others none', () => {
    const u = apply(['live'], '/admin/api/usage', readFixture('usage.json')).body as Any
    const active = u.users.filter((x: Any) => x.limits?.tokensPerMinute > 0 && x.requestsPerDay > 0)
    expect(active.length).toBeGreaterThan(0)
    for (const x of active) {
      expect(x.tokensPerMinute).toBeGreaterThan(0)
      expect(x.tokensPerMinute).toBeLessThan(x.limits.tokensPerMinute * 0.05)
    }
    for (const x of u.users.filter((y: Any) => !(y.limits?.tokensPerMinute > 0))) expect(x.tokensPerMinute).toBeUndefined()
  })

  it('huge triples providers and models with unique ids', () => {
    const base = overview()
    const o = apply(['huge'], '/admin/api/overview', base).body as Any
    expect(o.providers).toHaveLength(base.providers.length * 3)
    expect(new Set(o.providers.map((p: Any) => p.name)).size).toBe(o.providers.length)
    for (const p of o.providers) expect(new Set(p.models).size).toBe(p.models.length)
    expect(o.providers[0].models).toHaveLength(base.providers[0].models.length * 3)
    const m = apply(['huge'], '/admin/api/usage/models', readFixture(manifest.find((x) => x.path.endsWith('/usage/models') && x.status === 200)!.file)).body as Any
    expect(new Set(m.models.map((x: Any) => x.id)).size).toBe(m.models.length)
  })

  it('slow reports a 1500ms delay', () => {
    expect(scenarioDelayMs(['slow'])).toBe(1500)
    expect(scenarioDelayMs([])).toBe(0)
  })

  it('leaves non-matching paths and scenarios untouched', () => {
    const body = { a: 1 }
    expect(apply([], '/admin/api/overview', body)).toEqual({ status: 200, body })
    expect(apply(['live'], '/admin/api/config', body).body).toEqual(body)
  })
})

describe('huge scenario consistency', () => {
  const hugeGet = (path: string, query: string) => get(path, query, ['huge']).body as Any

  it('lists the same provider ids in overview and performance kind=provider', () => {
    const overview = hugeGet('/admin/api/overview', '')
    const perf = hugeGet('/admin/api/performance', 'kind=provider&window=day&span=14')
    const names = overview.providers.map((p: Any) => p.name).sort()
    expect(perf.rows.map((r: Any) => r.id).sort()).toEqual(names)
    expect(names).toHaveLength(57)
  })

  it('lists model ids that overview and catalog also list', () => {
    const overview = hugeGet('/admin/api/overview', '')
    const fromOverview = overview.providers.flatMap((p: Any) => p.models.map((m: string) => `${p.name}/${m}`))
    const catalog = hugeGet('/admin/api/catalog', '')
    const known = new Set<string>(catalog.providers.flatMap((p: Any) => p.models.map((m: Any) => m.id)))
    for (const id of fromOverview) expect(known.has(id), id).toBe(true)
    const perf = hugeGet('/admin/api/performance', 'kind=model&window=day&span=14')
    const models = hugeGet('/admin/api/usage/models', 'metric=req&window=day&span=7&limit=100&detail=1')
    const totals = hugeGet('/admin/api/usage/totals', 'kind=usermodel&window=day&span=14&user=admin')
    for (const rows of [perf.rows, models.models, totals.rows]) {
      expect(rows.length).toBeGreaterThan(0)
      expect(new Set(rows.map((r: Any) => r.id)).size).toBe(rows.length)
      for (const r of rows) expect(known.has(r.id), r.id).toBe(true)
    }
  })

  it('serves series for the tripled provider and model scopes', () => {
    const prov = hugeGet('/admin/api/usage/series', 'scope=provider:alibaba-2&metric=attempt&window=day&span=7')
    const model = hugeGet('/admin/api/usage/series', 'scope=model:alibaba-3/deepseek-v4-flash-0731-v2&metric=req&window=day&span=7')
    expect(prov.series.map((s: Any) => s.scope)).toEqual(['provider:alibaba-2'])
    expect(model.series.map((s: Any) => s.scope)).toEqual(['model:alibaba-3/deepseek-v4-flash-0731-v2'])
    for (const s of [...prov.series, ...model.series]) expect(s.points).toHaveLength(7)
  })
})

describe('usage/models detail parsing', () => {
  const MODELS = '/admin/api/usage/models'
  const base = 'metric=req&window=day&span=7&limit=100'
  const hasField = (query: string): boolean => (get(MODELS, query).body as Any).models.some((m: Any) => 'promptCacheReadTokens' in m)

  it('mirrors parseUsageModelsDetail', () => {
    for (const v of ['', '0', 'false']) expect(isDetail(q(v === '' ? 'detail=' : `detail=${v}`))).toBe(false)
    for (const v of ['1', 'true']) expect(isDetail(q(`detail=${v}`))).toBe(true)
    for (const v of ['TRUE', 'yes', '2']) expect(isDetail(q(`detail=${v}`))).toBeNull()
    expect(isDetail(q(''))).toBe(false)
  })

  it('returns the prompt-cache fields only for detail=1 and detail=true', () => {
    expect(hasField(`${base}&detail=1`)).toBe(true)
    expect(hasField(`${base}&detail=true`)).toBe(true)
    expect(hasField(`${base}&detail=0`)).toBe(false)
    expect(hasField(`${base}&detail=false`)).toBe(false)
    expect(hasField(base)).toBe(false)
  })

  it('rejects an unknown detail value with 400', () => {
    expect(get(MODELS, `${base}&detail=yes`).status).toBe(400)
  })

  it('does not apply the range limit to detail=0', () => {
    expect(get(MODELS, 'metric=req&window=day&span=30&limit=100&detail=0').status).toBe(200)
    expect(get(MODELS, 'metric=req&window=day&span=30&limit=100&detail=1').status).toBe(400)
  })
})

describe('stripDetailOnly', () => {
  const body = { models: [{ id: 'a', tokensIn: 5, promptCacheReadTokens: 1, promptCacheWriteTokens: 2, promptCacheSavedMicroUsd: 3 }] }

  it('removes only the prompt-cache fields when detail is off', () => {
    expect((stripDetailOnly('/admin/api/usage/models', q('detail=0'), body) as Any).models).toEqual([{ id: 'a', tokensIn: 5 }])
  })

  it('keeps the body when detail is on or the path is another route', () => {
    expect(stripDetailOnly('/admin/api/usage/models', q('detail=1'), body)).toBe(body)
    expect(stripDetailOnly('/admin/api/usage/totals', q(''), body)).toBe(body)
  })
})
