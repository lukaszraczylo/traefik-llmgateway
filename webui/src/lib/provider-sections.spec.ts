import { describe, expect, it } from 'vitest'

import { providerSections } from '@/lib/provider-sections'

describe('providerSections', () => {
  it('returns one untitled section when no kinds or endpoints are set', () => {
    expect(providerSections({ models: ['a', 'b'] })).toEqual([{ title: '', models: ['a', 'b'], endpoints: [] }])
  })

  it('groups models and endpoints by kind, plain models first', () => {
    const got = providerSections({
      models: ['chat', 'rerank', 'embed'],
      kinds: { rerank: 'reranking', embed: 'embedding', '/v1/decide': 'decision' },
      endpoints: ['/v1/decide'],
    })
    expect(got).toEqual([
      { title: '', models: ['chat'], endpoints: [] },
      { title: 'decision', models: [], endpoints: ['/v1/decide'] },
      { title: 'embedding', models: ['embed'], endpoints: [] },
      { title: 'reranking', models: ['rerank'], endpoints: [] },
    ])
  })

  it('files an endpoint without a kind under "endpoints"', () => {
    const got = providerSections({ models: ['chat'], endpoints: ['/v1/other'] })
    expect(got).toEqual([
      { title: '', models: ['chat'], endpoints: [] },
      { title: 'endpoints', models: [], endpoints: ['/v1/other'] },
    ])
  })
})
