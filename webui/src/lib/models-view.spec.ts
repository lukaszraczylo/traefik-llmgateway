import { describe, expect, it } from 'vitest'

import { groupByProvider, priceStatus, selectModels } from '@/lib/models-view'
import type { ModelCatalogRow } from '@/lib/model-table-columns'

function row(id: string, over: Partial<ModelCatalogRow> = {}): ModelCatalogRow {
  const slash = id.indexOf('/')
  return {
    id,
    model: id.slice(slash + 1),
    providerName: id.slice(0, slash),
    kind: '',
    endpoint: false,
    priceSource: 'litellm',
    aliases: [],
    requests: 0,
    tokensIn: 0,
    tokensOut: 0,
    costMicroUsd: 0,
    attempts: 0,
    failures: 0,
    ...over,
  }
}

describe('priceStatus', () => {
  it('maps price sources to the three filter values', () => {
    expect(priceStatus(row('a/x', { priceSource: 'override' }))).toBe('priced')
    expect(priceStatus(row('a/x', { priceSource: 'unpriced' }))).toBe('unpriced')
    expect(priceStatus(row('a/x', { priceSource: 'free' }))).toBe('free')
  })
  it('gives endpoint pseudo-models no price status', () => {
    expect(priceStatus(row('a/x', { endpoint: true, priceSource: 'unpriced' }))).toBe('none')
  })
})

describe('selectModels', () => {
  const rows = [
    row('a/one', { kind: 'embedding', priceSource: 'free' }),
    row('a/two', { priceSource: 'unpriced', aliases: ['fast'] }),
    row('b/three'),
  ]
  it('returns every row for an empty selection', () => {
    expect(selectModels(rows, {})).toHaveLength(3)
  })
  it('combines provider, kind, price and text with AND', () => {
    expect(selectModels(rows, { provider: 'a' }).map((r) => r.id)).toEqual(['a/one', 'a/two'])
    expect(selectModels(rows, { kind: 'embedding' }).map((r) => r.id)).toEqual(['a/one'])
    expect(selectModels(rows, { price: 'unpriced' }).map((r) => r.id)).toEqual(['a/two'])
    expect(selectModels(rows, { q: 'FAST' }).map((r) => r.id)).toEqual(['a/two'])
    expect(selectModels(rows, { provider: 'b', price: 'unpriced' })).toEqual([])
  })
  it('treats kind "none" as the unlabelled models', () => {
    expect(selectModels(rows, { kind: 'none' }).map((r) => r.id)).toEqual(['a/two', 'b/three'])
  })
})

describe('groupByProvider', () => {
  it('groups in first-seen order with counts and request totals', () => {
    const g = groupByProvider([row('b/x', { requests: 2 }), row('a/y', { requests: 5 }), row('b/z', { requests: 3 })])
    expect(g.map((x) => [x.provider, x.rows.length, x.requests])).toEqual([
      ['b', 2, 5],
      ['a', 1, 5],
    ])
  })
  it('sorts rows inside a group by requests then id', () => {
    const [g] = groupByProvider([row('a/b'), row('a/a'), row('a/c', { requests: 9 })])
    expect(g!.rows.map((r) => r.id)).toEqual(['a/c', 'a/a', 'a/b'])
  })
})
