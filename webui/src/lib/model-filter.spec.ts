import { describe, expect, it } from 'vitest'

import { filterModelRows } from './model-filter'
import type { ModelCatalogRow } from './model-table-columns'

function row(overrides: Partial<ModelCatalogRow> = {}): ModelCatalogRow {
  return {
    id: 'openai/gpt-5',
    model: 'gpt-5',
    providerName: 'openai',
    priceSource: 'builtin',
    aliases: [],
    requests: 0,
    tokensIn: 0,
    tokensOut: 0,
    costMicroUsd: 0,
    attempts: 0,
    failures: 0,
    ...overrides,
  }
}

const rows: ModelCatalogRow[] = [
  row({ id: 'openai/gpt-5' }),
  row({ id: 'openai/gpt-5-mini' }),
  row({ id: 'anthropic/claude-opus', aliases: ['smart', 'flagship'] }),
]

describe('filterModelRows', () => {
  it('returns every row unchanged for an empty query', () => {
    expect(filterModelRows(rows, '')).toBe(rows)
  })

  it('returns every row unchanged for a whitespace-only query', () => {
    expect(filterModelRows(rows, '   ')).toBe(rows)
  })

  it('matches a substring of the canonical id, case-insensitively', () => {
    expect(filterModelRows(rows, 'GPT-5').map((r) => r.id)).toEqual(['openai/gpt-5', 'openai/gpt-5-mini'])
  })

  it('matches a provider prefix', () => {
    expect(filterModelRows(rows, 'anthropic/').map((r) => r.id)).toEqual(['anthropic/claude-opus'])
  })

  it('matches an alias, case-insensitively', () => {
    expect(filterModelRows(rows, 'FLAGSHIP').map((r) => r.id)).toEqual(['anthropic/claude-opus'])
  })

  it('returns an empty array when nothing matches', () => {
    expect(filterModelRows(rows, 'mistral')).toEqual([])
  })
})
