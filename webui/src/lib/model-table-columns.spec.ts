import { describe, expect, it } from 'vitest'

import { buildModelCatalogRows } from './model-table-columns'
import type { AdminCatalogResponse, AdminPerfResponse, AdminUsageModelsResponse } from '@/types/api'

describe('buildModelCatalogRows', () => {
  function catalog(): AdminCatalogResponse {
    return {
      providers: [
        {
          lastRefresh: '2026-01-01T00:00:00Z',
          openUntil: '0001-01-01T00:00:00Z',
          name: 'openai',
          type: 'openai',
          healthState: 'closed',
          discoveryEnabled: true,
          models: [
            { id: 'openai/gpt-5', model: 'gpt-5', priceSource: 'builtin', inputPerMTokUsd: 1.5, outputPerMTokUsd: 6 },
            { id: 'openai/gpt-5-mini', model: 'gpt-5-mini', priceSource: 'unpriced' },
          ],
        },
      ],
      aliases: [],
    }
  }

  function ranking(): AdminUsageModelsResponse {
    return {
      metric: 'cost',
      window: 'day',
      span: 7,
      offset: 0,
      detail: true,
      models: [{ id: 'openai/gpt-5', value: 5, requests: 10, tokensIn: 100, tokensOut: 200, costMicroUsd: 5_000_000 }],
    }
  }

  function perf(): AdminPerfResponse {
    return {
      kind: 'model',
      window: 'day',
      span: 7,
      offset: 0,
      latencyEnabled: true,
      rows: [{ id: 'openai/gpt-5', p50Ms: 120, p95Ms: 400, count: 10, attempts: 10, failures: 1 }],
    }
  }

  it('produces one row per CATALOG model, not per ranking/performance row', () => {
    const rows = buildModelCatalogRows(catalog(), ranking(), perf())
    expect(rows).toHaveLength(2)
    expect(rows.map((r) => r.id)).toEqual(['openai/gpt-5', 'openai/gpt-5-mini'])
  })

  it('joins ranking and performance fields onto a matching catalog row', () => {
    const rows = buildModelCatalogRows(catalog(), ranking(), perf())
    const row = rows.find((r) => r.id === 'openai/gpt-5')
    expect(row).toMatchObject({
      requests: 10,
      tokensIn: 100,
      tokensOut: 200,
      costMicroUsd: 5_000_000,
      p50Ms: 120,
      p95Ms: 400,
      attempts: 10,
      failures: 1,
    })
  })

  it('defaults usage/performance fields to zero/undefined for a catalog model with no traffic', () => {
    const rows = buildModelCatalogRows(catalog(), ranking(), perf())
    const row = rows.find((r) => r.id === 'openai/gpt-5-mini')
    expect(row).toMatchObject({ requests: 0, tokensIn: 0, tokensOut: 0, costMicroUsd: 0, attempts: 0, failures: 0 })
    expect(row?.p50Ms).toBeUndefined()
    expect(row?.p95Ms).toBeUndefined()
  })

  it('carries priceSource/displayFree/aliases straight from the catalog model', () => {
    const c = catalog()
    c.providers[0].models[0].displayFree = true
    c.providers[0].models[0].aliases = ['smart']
    const rows = buildModelCatalogRows(c, ranking(), perf())
    const row = rows.find((r) => r.id === 'openai/gpt-5')
    expect(row?.priceSource).toBe('builtin')
    expect(row?.displayFree).toBe(true)
    expect(row?.aliases).toEqual(['smart'])
  })

  it('defaults aliases to an empty array when the catalog model omits it', () => {
    const rows = buildModelCatalogRows(catalog(), ranking(), perf())
    expect(rows.find((r) => r.id === 'openai/gpt-5-mini')?.aliases).toEqual([])
  })
})

describe('buildModelCatalogRows: router targets', () => {
  it('carries a model routers list and defaults to empty', () => {
    const catalog = {
      providers: [
        {
          name: 'openai',
          type: 'openai',
          lastRefresh: '',
          openUntil: '',
          healthState: 'closed',
          discoveryEnabled: false,
          models: [
            { id: 'openai/a', model: 'a', priceSource: 'unpriced', routers: ['auto'] },
            { id: 'openai/b', model: 'b', priceSource: 'unpriced' },
          ],
        },
      ],
      aliases: [],
      routers: [],
    } as unknown as AdminCatalogResponse
    const rows = buildModelCatalogRows(catalog, { models: [] } as unknown as AdminUsageModelsResponse, { rows: [] } as unknown as AdminPerfResponse)
    expect(rows.map((r) => r.routers)).toEqual([['auto'], []])
  })
})
