import { describe, expect, it } from 'vitest'

import { findTarget, hostOf, targetRows } from './target-rows'
import type { AdminTargetsResponse, AdminTargetView } from '@/types/api'

function target(name: string, overrides: Partial<AdminTargetView> = {}): AdminTargetView {
  return {
    name,
    url: `http://${name}.svc.internal:8080/mcp`,
    access: null,
    counters: { requestsPerMinute: 0, requestsPerDay: 1, requestsPerMonth: 2 },
    health: { state: 'healthy', consecutiveFailures: 0 },
    ...overrides,
  }
}

const targets: AdminTargetsResponse = {
  mcpServers: [target('fetch'), target('maps', { health: { state: 'unhealthy', consecutiveFailures: 3 } })],
  agents: [target('fetch', { url: 'https://agent.example.com' }), target('planner', { health: { state: 'unknown', consecutiveFailures: 0 } })],
}

describe('hostOf', () => {
  it('returns host and port of a URL', () => {
    expect(hostOf('http://mcp-fetch.svc.internal:8088/mcp')).toBe('mcp-fetch.svc.internal:8088')
  })
  it('falls back to the raw text for an unparseable URL', () => {
    expect(hostOf('not a url')).toBe('not a url')
  })
  it('never returns an empty string', () => {
    expect(hostOf('')).toBe('—')
  })
})

describe('targetRows', () => {
  it('lists MCP servers then agents with composed ids, so equal names stay distinct', () => {
    expect(targetRows(targets).map((r) => r.id)).toEqual(['mcp/fetch', 'mcp/maps', 'agent/fetch', 'agent/planner'])
  })
  it('labels the kind and keeps the host', () => {
    const [mcp, , agent] = targetRows(targets)
    expect([mcp.kind, agent.kind]).toEqual(['MCP', 'Agent'])
    expect(agent.host).toBe('agent.example.com')
  })
  it('ranks unhealthy above unknown above healthy', () => {
    const rank = Object.fromEntries(targetRows(targets).map((r) => [r.id, r.healthRank]))
    expect(rank['mcp/maps']).toBeGreaterThan(rank['agent/planner'])
    expect(rank['agent/planner']).toBeGreaterThan(rank['mcp/fetch'])
  })
  it('treats a health state this build does not know as unknown', () => {
    const odd = { mcpServers: [target('x', { health: { state: 'weird' as never, consecutiveFailures: 0 } })], agents: [] }
    expect(targetRows(odd)[0].health).toBe('unknown')
  })
  it('is empty for no targets', () => {
    expect(targetRows(null)).toEqual([])
  })
})

describe('findTarget', () => {
  it('finds a target by composed id', () => {
    expect(findTarget(targets, 'agent/fetch')?.url).toBe('https://agent.example.com')
  })
  it('resolves a bare name only when it is unambiguous', () => {
    expect(findTarget(targets, 'planner')?.name).toBe('planner')
    expect(findTarget(targets, 'fetch')).toBeNull()
  })
  it('is null for an unknown id', () => {
    expect(findTarget(targets, 'mcp/nope')).toBeNull()
  })
})
