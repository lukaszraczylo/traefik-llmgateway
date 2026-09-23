import { describe, expect, it } from 'vitest'

import { buildAccessMatrix, providerAccessCell, targetAccessCell } from './access-matrix'
import type { AdminConsumerGroup, AdminTargetView } from '@/types/api'

function group(overrides: Partial<AdminConsumerGroup> = {}): AdminConsumerGroup {
  return {
    name: 'friends',
    allowedProviders: [],
    modelAccess: {},
    memberCount: 3,
    ...overrides,
  }
}

function target(overrides: Partial<AdminTargetView> = {}): AdminTargetView {
  return {
    name: 'fetch',
    url: 'http://mcp-fetch/mcp',
    counters: { requestsPerMinute: 0, requestsPerDay: 0, requestsPerMonth: 0 },
    health: { state: 'unknown', consecutiveFailures: 0 },
    ...overrides,
  }
}

describe('providerAccessCell', () => {
  it('not allowed when the provider is absent from allowedProviders', () => {
    const g = group({ allowedProviders: ['minimax'] })
    expect(providerAccessCell(g, 'uni')).toEqual({ allowed: false })
  })

  it('allowed with a "n/m models" detail when modelAccess has an entry', () => {
    const g = group({ allowedProviders: ['minimax'], modelAccess: { minimax: { allowed: 2, total: 5 } } })
    expect(providerAccessCell(g, 'minimax')).toEqual({ allowed: true, detail: '2/5 models' })
  })

  it('allowed with no detail when modelAccess has no entry for this provider', () => {
    const g = group({ allowedProviders: ['minimax'], modelAccess: {} })
    expect(providerAccessCell(g, 'minimax')).toEqual({ allowed: true, detail: undefined })
  })

  // P3 item 18: allowed but 0 of the provider's models are reachable
  // (a `models` glob matching none of it) must NOT render identically to
  // an ordinary, genuinely-unrestricted allowed cell.
  it('sets zero: true when the provider is allowed but 0 of its models are reachable', () => {
    const g = group({ allowedProviders: ['minimax'], modelAccess: { minimax: { allowed: 0, total: 5 } } })
    expect(providerAccessCell(g, 'minimax')).toEqual({ allowed: true, detail: '0/5 models', zero: true })
  })

  it('does NOT set zero when the provider itself has 0 total models (unrelated to the grant)', () => {
    const g = group({ allowedProviders: ['minimax'], modelAccess: { minimax: { allowed: 0, total: 0 } } })
    expect(providerAccessCell(g, 'minimax')).toEqual({ allowed: true, detail: '0/0 models' })
  })
})

describe('targetAccessCell', () => {
  it('allowed for every group when access is undefined (unrestricted)', () => {
    expect(targetAccessCell(group({ name: 'friends' }), target({ access: undefined }))).toEqual({ allowed: true })
  })

  it('allowed for every group when access is null (unrestricted)', () => {
    expect(targetAccessCell(group({ name: 'friends' }), target({ access: null }))).toEqual({ allowed: true })
  })

  it('not allowed for any group when access is an empty array', () => {
    expect(targetAccessCell(group({ name: 'friends' }), target({ access: [] }))).toEqual({ allowed: false })
  })

  it('allowed only for a named group', () => {
    expect(targetAccessCell(group({ name: 'friends' }), target({ access: ['friends'] }))).toEqual({ allowed: true })
    expect(targetAccessCell(group({ name: 'home' }), target({ access: ['friends'] }))).toEqual({ allowed: false })
  })
})

describe('buildAccessMatrix', () => {
  it('builds one row per group, keyed by column name', () => {
    const groups = [
      group({ name: 'home', allowedProviders: ['anthropic', 'openai'], modelAccess: { anthropic: { allowed: 5, total: 5 } } }),
      group({ name: 'friends', allowedProviders: ['minimax'] }),
    ]
    const mcp = [target({ name: 'fetch', access: undefined }), target({ name: 'weather', access: ['home'] })]
    const agents = [target({ name: 'agentkit', access: [] })]
    const rows = buildAccessMatrix(groups, ['anthropic', 'openai', 'minimax'], mcp, agents)

    expect(rows).toHaveLength(2)
    expect(rows[0].group).toBe('home')
    expect(rows[0].providers.anthropic).toEqual({ allowed: true, detail: '5/5 models' })
    expect(rows[0].providers.openai).toEqual({ allowed: true, detail: undefined })
    expect(rows[0].providers.minimax).toEqual({ allowed: false })
    expect(rows[0].mcpServers.fetch).toEqual({ allowed: true })
    expect(rows[0].mcpServers.weather).toEqual({ allowed: true })
    expect(rows[0].agents.agentkit).toEqual({ allowed: false })

    expect(rows[1].group).toBe('friends')
    expect(rows[1].mcpServers.weather).toEqual({ allowed: false })
  })

  it('returns an empty array for no groups', () => {
    expect(buildAccessMatrix([], ['anthropic'], [], [])).toEqual([])
  })
})
