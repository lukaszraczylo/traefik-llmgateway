import { readFileSync } from 'node:fs'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

import type { Connect, Plugin } from 'vite'

import type { Fixture } from './match.ts'
import { respond } from './respond.ts'
import { errorEnvelope, parseScenarios, scenarioDelayMs } from './scenarios.ts'

const HERE = dirname(fileURLToPath(import.meta.url))
const API_PREFIX = '/admin/api'

function send(res: ServerResponse, status: number, body: unknown) {
  res.statusCode = status
  res.setHeader('content-type', 'application/json')
  res.setHeader('cache-control', 'no-store')
  res.end(JSON.stringify(body))
}

const header = (req: IncomingMessage, name: string) => {
  const v = req.headers[name]
  return Array.isArray(v) ? v[0] : v
}

/** Vite plugin serving /admin/api/* from the captured fixtures; dev and preview servers only, enabled by MOCK=1 in vite.config.ts. */
export function mockAdminApi(): Plugin {
  const manifest = JSON.parse(readFileSync(join(HERE, 'manifest.json'), 'utf8')) as Fixture[]
  const readFixture = (file: string) => JSON.parse(readFileSync(join(HERE, 'fixtures', file), 'utf8')) as unknown
  const handle = async (req: Connect.IncomingMessage, res: ServerResponse) => {
    const url = new URL(req.originalUrl ?? req.url ?? '/', 'http://mock.local')
    const scenarios = parseScenarios(header(req, 'referer'), header(req, 'x-mock-scenario'))
    if (req.method !== 'GET') return send(res, 405, errorEnvelope(405, 'invalid_request_error', 'method not allowed'))
    const hasKey = (header(req, 'x-api-key') ?? '').trim() !== ''
    const out = respond({
      manifest,
      readFixture,
      path: url.pathname,
      query: url.searchParams,
      scenarios: hasKey ? scenarios : [...scenarios, 'auth-401'],
      now: Date.now(),
      rng: Math.random,
    })
    const delay = scenarioDelayMs(scenarios)
    if (delay > 0) await new Promise((r) => setTimeout(r, delay))
    send(res, out.status, out.body)
  }
  return {
    name: 'mock-admin-api',
    apply: 'serve',
    configureServer(server) {
      server.middlewares.use(API_PREFIX, handle)
      server.config.logger.info(`  mock admin API active (scenarios via ?mock=a,b)`)
    },
    // `vite preview` of a built dist (CSP checks) serves the same fixtures.
    configurePreviewServer(server) {
      server.middlewares.use(API_PREFIX, handle)
    },
  }
}
