import { describe, expect, it } from 'vitest'

import { groupByProvider, priceStatus, selectModels, sortThenSlice } from '@/lib/models-view'
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

describe('sortThenSlice', () => {
  const rows = [
    { id: 'a', requests: 5, name: 'b' },
    { id: 'b', requests: null, name: 'a' },
    { id: 'c', requests: 9, name: 'c' },
    { id: 'd', requests: 1, name: 'd' },
  ]
  it('orders the whole list before taking the page', () => {
    const p0 = sortThenSlice(rows, { key: 'requests', direction: 'asc' }, 2, 0)
    const p1 = sortThenSlice(rows, { key: 'requests', direction: 'asc' }, 2, 1)
    expect(p0.map((r) => r.id)).toEqual(['d', 'a'])
    expect(p1.map((r) => r.id)).toEqual(['c', 'b'])
  })
  it('keeps missing values last in both directions', () => {
    expect(sortThenSlice(rows, { key: 'requests', direction: 'desc' }, 4, 0).map((r) => r.id)).toEqual(['c', 'a', 'd', 'b'])
  })
  it('sorts text and keeps input order without a sort', () => {
    expect(sortThenSlice(rows, { key: 'name', direction: 'asc' }, 4, 0).map((r) => r.id)).toEqual(['b', 'a', 'c', 'd'])
    expect(sortThenSlice(rows, null, 2, 1).map((r) => r.id)).toEqual(['c', 'd'])
  })
  it('clamps an out-of-range page and does not mutate the input', () => {
    const copy = [...rows]
    expect(sortThenSlice(rows, null, 3, 9).map((r) => r.id)).toEqual(['d'])
    expect(rows).toEqual(copy)
  })
})
