import { describe, expect, it } from 'vitest'

import { composeTargetId, parseTargetCallerId, resolveTargetRef } from './target-columns'
import type { AdminTargetsResponse, AdminTargetView } from '@/types/api'
describe('composeTargetId', () => {
  it('joins kind and name with a slash', () => {
    expect(composeTargetId('mcp', 'fetch')).toBe('mcp/fetch')
    expect(composeTargetId('agent', 'researcher')).toBe('agent/researcher')
  })
})

describe('parseTargetCallerId', () => {
  it('splits a well-formed "kind/target/caller" id into its three parts', () => {
    expect(parseTargetCallerId('mcp/fetch/alice')).toEqual({ kind: 'mcp', target: 'fetch', caller: 'alice' })
  })

  it('handles a caller name that itself contains a slash by keeping it in `caller` verbatim', () => {
    expect(parseTargetCallerId('agent/researcher/team/bob')).toEqual({ kind: 'agent', target: 'researcher', caller: 'team/bob' })
  })

  it('falls back to an empty target/caller for an id with no slash at all, without throwing', () => {
    expect(parseTargetCallerId('malformed')).toEqual({ kind: '', target: '', caller: 'malformed' })
  })

  it('falls back to an empty caller for an id with only one slash', () => {
    expect(parseTargetCallerId('mcp/fetch')).toEqual({ kind: 'mcp', target: 'fetch', caller: '' })
  })
})
// --- resolveTargetRef (P2 item: a hand-typed #targets?target=demo-mcp
// 400ed — parseTargetRef, stats_read.go, only accepts "mcp/{name}"/
// "agent/{name}") ---

function namedTarget(name: string): AdminTargetView {
  return {
    name,
    url: 'https://example.com/mcp',
    counters: { requestsPerMinute: 0, requestsPerDay: 0, requestsPerMonth: 0 },
    health: { state: 'unknown', consecutiveFailures: 0 },
  }
}

function targets(mcpNames: string[], agentNames: string[]): Pick<AdminTargetsResponse, 'mcpServers' | 'agents'> {
  return { mcpServers: mcpNames.map(namedTarget), agents: agentNames.map(namedTarget) }
}

describe('resolveTargetRef', () => {
  it('passes an already-prefixed "mcp/{name}" ref through unchanged, without needing targets loaded', () => {
    expect(resolveTargetRef('mcp/fetch', null)).toEqual({ kind: 'ref', ref: 'mcp/fetch', name: 'fetch' })
  })

  it('passes an already-prefixed "agent/{name}" ref through unchanged', () => {
    expect(resolveTargetRef('agent/agentkit', targets([], ['agentkit']))).toEqual({
      kind: 'ref',
      ref: 'agent/agentkit',
      name: 'agentkit',
    })
  })

  it('round-trips a name that itself contains a slash, matching stats_read.go\'s Cut-on-first-slash', () => {
    expect(resolveTargetRef('mcp/team/fetch', null)).toEqual({ kind: 'ref', ref: 'mcp/team/fetch', name: 'team/fetch' })
  })

  it('is "pending" for a bare name while dashboard.targets has not loaded yet (null)', () => {
    expect(resolveTargetRef('demo-mcp', null)).toEqual({ kind: 'pending' })
  })

  it('resolves a bare name found only in mcpServers', () => {
    expect(resolveTargetRef('demo-mcp', targets(['demo-mcp'], []))).toEqual({
      kind: 'ref',
      ref: 'mcp/demo-mcp',
      name: 'demo-mcp',
    })
  })

  it('resolves a bare name found only in agents', () => {
    expect(resolveTargetRef('agentkit', targets([], ['agentkit']))).toEqual({
      kind: 'ref',
      ref: 'agent/agentkit',
      name: 'agentkit',
    })
  })

  it('shows a notice for a bare name found in neither list (unknown)', () => {
    const result = resolveTargetRef('nope', targets(['demo-mcp'], ['agentkit']))
    expect(result.kind).toBe('notice')
    expect(result.kind === 'notice' && result.message).toContain('No MCP server or agent named "nope"')
  })

  it('shows a notice for a bare name found in BOTH lists (ambiguous) rather than guessing', () => {
    const result = resolveTargetRef('shared', targets(['shared'], ['shared']))
    expect(result.kind).toBe('notice')
    expect(result.kind === 'notice' && result.message).toContain('matches both an MCP server and an agent')
  })

  it('rejects an unknown prefix as a bare name, not a ref (e.g. "http/foo" is looked up, not passed through)', () => {
    expect(resolveTargetRef('http/foo', targets([], []))).toEqual({
      kind: 'notice',
      message: 'No MCP server or agent named "http/foo" is configured.',
    })
  })
})
