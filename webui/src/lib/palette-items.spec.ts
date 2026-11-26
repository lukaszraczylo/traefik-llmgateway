import { describe, expect, it } from 'vitest'

import { paletteEntities } from './palette-items'
import { PANEL_PAGE } from './panel-selection'

describe('paletteEntities', () => {
  it('is empty before data loads', () => {
    expect(paletteEntities(null, null, null)).toEqual([])
  })

  it('lists each entity kind with a provider-prefixed model id', () => {
    const items = paletteEntities(
      { providers: [{ name: 'uni', models: ['glm-5.3-flash'] }], groups: [{ name: 'eng' }], routers: [{ name: 'auto' }] },
      { users: [{ id: 'alice' }] },
      { mcpServers: [{ name: 'fetch' }], agents: [{ name: 'coder' }] },
    )
    expect(items.map((i) => `${i.kind}:${i.id}`)).toEqual([
      'user:alice',
      'group:eng',
      'provider:uni',
      'model:uni/glm-5.3-flash',
      'router:auto',
      'target:fetch',
      'target:coder',
    ])
  })
})

describe('PANEL_PAGE', () => {
  it('routes every kind to its directory page', () => {
    expect(PANEL_PAGE).toEqual({
      user: 'users',
      group: 'users',
      model: 'models',
      provider: 'models',
      router: 'routers',
      target: 'targets',
    })
  })
})
