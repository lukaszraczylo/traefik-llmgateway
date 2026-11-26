import { describe, expect, it } from 'vitest'

import { NAV_GROUPS, navCounts } from './nav-groups'
import { DEFAULT_PAGE, PAGE_IDS } from './pages'

describe('NAV_GROUPS', () => {
  it('every PAGE_ID appears exactly once in NAV_GROUPS', () => {
    const ids = NAV_GROUPS.flatMap((g) => g.items.map((i) => i.id))
    expect([...ids].sort()).toEqual([...PAGE_IDS].sort())
    expect(new Set(ids).size).toBe(ids.length)
  })

  it('orders groups as Overview/Usage/Reliability | Directory | Events/Config', () => {
    expect(NAV_GROUPS.map((g) => [g.label, g.items.map((i) => i.id)])).toEqual([
      [null, ['overview', 'usage', 'reliability']],
      ['Directory', ['users', 'models', 'routers', 'targets']],
      [null, ['events', 'config']],
    ])
  })

  it('labels the directory pages as specified', () => {
    const labels = NAV_GROUPS[1].items.map((i) => i.label)
    expect(labels).toEqual(['Users & groups', 'Models & providers', 'Smart routers', 'MCP & agents'])
  })

  it('defaults to overview', () => {
    expect(DEFAULT_PAGE).toBe('overview')
  })
})

describe('navCounts', () => {
  it('is empty before any data has loaded', () => {
    expect(navCounts(null, null, null)).toEqual({})
  })

  it('derives the four directory counts', () => {
    const counts = navCounts(
      { providers: [{ models: ['a', 'b'] }, { models: ['c'] }], routers: [{}, {}] },
      { users: [{}, {}, {}] },
      { mcpServers: [{}], agents: [{}, {}] },
    )
    expect(counts).toEqual({ users: 3, models: 3, routers: 2, targets: 3 })
  })
})
