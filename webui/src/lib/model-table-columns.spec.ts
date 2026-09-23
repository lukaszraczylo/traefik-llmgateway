import type { VNode } from 'vue'
import { isVNode } from 'vue'
import { describe, expect, it } from 'vitest'

import EntityLink from '@/components/EntityLink.vue'
import ModelChip from '@/components/ModelChip.vue'
import { Badge } from '@/components/ui/badge'
import { buildModelCatalogRows, modelCatalogColumns } from './model-table-columns'
import type { ModelCatalogRow } from './model-table-columns'
import type { AdminCatalogResponse, AdminPerfResponse, AdminUsageModelsResponse } from '@/types/api'

function testRow(overrides: Partial<ModelCatalogRow> = {}): ModelCatalogRow {
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

function accessorFnOf(col: unknown): (row: ModelCatalogRow, index: number) => unknown {
  return (col as { accessorFn: (row: ModelCatalogRow, index: number) => unknown }).accessorFn
}

function cellOf(col: unknown): (ctx: { row: { original: ModelCatalogRow } }) => unknown {
  return (col as { cell: (ctx: { row: { original: ModelCatalogRow } }) => unknown }).cell
}

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

describe('modelCatalogColumns: id column', () => {
  const col = modelCatalogColumns().find((c) => c.id === 'id')

  it('cell renders a ModelChip and an EntityLink pointed at kind "model"', () => {
    const rendered = cellOf(col)({ row: { original: testRow({ id: 'anthropic/claude' }) } }) as VNode
    const children = rendered.children as VNode[]
    const chip = children.find((c) => c.type === ModelChip)
    const link = children.find((c) => c.type === EntityLink)
    expect(chip?.props?.id).toBe('anthropic/claude')
    expect(link?.props?.kind).toBe('model')
    expect(link?.props?.id).toBe('anthropic/claude')
    expect(link?.props?.label).toBe('anthropic/claude')
  })
})

describe('modelCatalogColumns: price column', () => {
  const col = modelCatalogColumns().find((c) => c.id === 'price')

  it('renders a destructive "unpriced" Badge for priceSource "unpriced"', () => {
    const rendered = cellOf(col)({ row: { original: testRow({ priceSource: 'unpriced' }) } }) as VNode
    expect(rendered.type).toBe(Badge)
    expect(rendered.props?.variant).toBe('destructive')
  })

  it('renders a "free" Badge for priceSource "free"', () => {
    const rendered = cellOf(col)({ row: { original: testRow({ priceSource: 'free' }) } }) as VNode
    expect(rendered.type).toBe(Badge)
    const slot = (rendered.children as { default: () => string }).default
    expect(slot()).toBe('free')
  })

  it('renders the formatted price plus a source Badge for override/builtin/litellm', () => {
    const rendered = cellOf(col)({
      row: { original: testRow({ priceSource: 'litellm', inputPerMTokUsd: 1, outputPerMTokUsd: 2 }) },
    }) as VNode
    const children = rendered.children as VNode[]
    expect((children[0] as VNode).children).toBe('in $1.00 / out $2.00 per MTok')
    const badge = children[1] as VNode
    expect(badge.type).toBe(Badge)
    expect((badge.children as { default: () => string }).default()).toBe('litellm')
  })

  it('renders an em dash placeholder for a priced source with unknown per-token cost (version skew)', () => {
    const rendered = cellOf(col)({ row: { original: testRow({ priceSource: 'override' }) } }) as VNode
    const children = rendered.children as VNode[]
    expect((children[0] as VNode).children).toBe('—')
  })

  it('sorts unpriced first, then ascending by input price', () => {
    const accessor = accessorFnOf(col)
    const rows = [
      testRow({ id: 'a', priceSource: 'builtin', inputPerMTokUsd: 5 }),
      testRow({ id: 'b', priceSource: 'unpriced' }),
      testRow({ id: 'c', priceSource: 'builtin', inputPerMTokUsd: 1 }),
    ]
    const sorted = [...rows].sort((x, y) => (accessor(x, 0) as number) - (accessor(y, 0) as number))
    expect(sorted.map((r) => r.id)).toEqual(['b', 'c', 'a'])
  })
})

describe('modelCatalogColumns: free column (":free" suffix, distinct from priceSource)', () => {
  const col = modelCatalogColumns().find((c) => c.id === 'free')

  it('renders a Badge only when displayFree is true', () => {
    const rendered = cellOf(col)({ row: { original: testRow({ displayFree: true }) } }) as VNode
    expect(rendered.type).toBe(Badge)
  })

  it('renders an em dash when displayFree is false/undefined, even for priceSource "free"', () => {
    const rendered = cellOf(col)({ row: { original: testRow({ priceSource: 'free' }) } })
    expect(isVNode(rendered) && rendered.type).toBe('span')
    expect(isVNode(rendered) && rendered.children).toBe('—')
  })
})

describe('modelCatalogColumns: aliases column', () => {
  const col = modelCatalogColumns().find((c) => c.id === 'aliases')

  it('renders an em dash for an empty alias list', () => {
    const rendered = cellOf(col)({ row: { original: testRow({ aliases: [] }) } })
    expect(isVNode(rendered) && rendered.children).toBe('—')
  })

  it('renders one Badge per alias', () => {
    const rendered = cellOf(col)({ row: { original: testRow({ aliases: ['a', 'b'] }) } }) as VNode
    const children = rendered.children as VNode[]
    expect(children).toHaveLength(2)
    expect(children.every((c) => c.type === Badge)).toBe(true)
  })
})

describe('modelCatalogColumns: cost column', () => {
  const col = modelCatalogColumns().find((c) => c.id === 'cost')

  it('renders a "free" Badge for a zero-cost free/displayFree row instead of $0.0000', () => {
    const rendered = cellOf(col)({ row: { original: testRow({ priceSource: 'free', costMicroUsd: 0 }) } }) as VNode
    expect(rendered.type).toBe(Badge)
    const rendered2 = cellOf(col)({ row: { original: testRow({ displayFree: true, costMicroUsd: 0 }) } }) as VNode
    expect(rendered2.type).toBe(Badge)
  })

  it('renders the real formatted cost when non-zero, even if free', () => {
    const rendered = cellOf(col)({ row: { original: testRow({ priceSource: 'free', costMicroUsd: 1_234_500 }) } })
    expect(rendered).toBe('$1.2345')
  })

  it('renders plain formatCost for a priced, non-free row', () => {
    const rendered = cellOf(col)({ row: { original: testRow({ costMicroUsd: 2_000_000 }) } })
    expect(rendered).toBe('$2.0000')
  })
})

describe('modelCatalogColumns: p50/p95/error rate columns', () => {
  it('p50/p95 render an em dash when undefined, formatted latency otherwise', () => {
    const p50 = modelCatalogColumns().find((c) => c.id === 'p50')
    const p95 = modelCatalogColumns().find((c) => c.id === 'p95')
    expect(cellOf(p50)({ row: { original: testRow() } })).toMatchObject({ children: '—' })
    expect(cellOf(p95)({ row: { original: testRow({ p95Ms: 1500 }) } })).toMatchObject({ children: '1.5s' })
  })

  it('error rate renders "no data" for zero attempts, a ceiled percent otherwise', () => {
    const col = modelCatalogColumns().find((c) => c.id === 'errorRate')
    expect(cellOf(col)({ row: { original: testRow({ attempts: 0, failures: 0 }) } })).toMatchObject({ children: 'no data' })
    expect(cellOf(col)({ row: { original: testRow({ attempts: 100, failures: 1 }) } })).toMatchObject({ children: '1%' })
  })

  it('error rate sorts a no-traffic row (-1) below every real rate', () => {
    const col = modelCatalogColumns().find((c) => c.id === 'errorRate')
    const accessor = accessorFnOf(col)
    expect(accessor(testRow({ attempts: 0, failures: 0 }), 0)).toBe(-1)
    expect(accessor(testRow({ attempts: 10, failures: 0 }), 0)).toBe(0)
  })
})
