import { describe, expect, it } from 'vitest'

import { compactColumn, costColumn, EMPTY_CELL, optionalColumn, usageMetricColumns } from './columns'

interface Row {
  requests: number
  tokensIn: number
  tokensOut: number
  costMicroUsd: number
  latencyMs?: number
}

describe('EMPTY_CELL', () => {
  it('is the em dash', () => {
    expect(EMPTY_CELL).toBe('—')
  })
})

describe('compactColumn', () => {
  it('carries the given id/header and right-aligns', () => {
    const col = compactColumn<Row>('requests', 'Requests', (r) => r.requests)
    expect(col.id).toBe('requests')
    expect(col.header).toBe('Requests')
    expect(col.meta).toEqual({ align: 'right' })
  })

  it('accessorFn reads the given field via the read function', () => {
    const col = compactColumn<Row>('requests', 'Requests', (r) => r.requests) as unknown as { accessorFn: (r: Row) => number }
    expect(col.accessorFn({ requests: 42, tokensIn: 0, tokensOut: 0, costMicroUsd: 0 })).toBe(42)
  })
})

describe('costColumn', () => {
  it('accessorFn reads the given field, unformatted', () => {
    const col = costColumn<Row>('cost', 'Cost', (r) => r.costMicroUsd) as unknown as { accessorFn: (r: Row) => number }
    expect(col.accessorFn({ requests: 0, tokensIn: 0, tokensOut: 0, costMicroUsd: 1_500_000 })).toBe(1_500_000)
  })
})

describe('optionalColumn', () => {
  it('accessorFn falls back to the default sentinel (0) for an undefined value', () => {
    const col = optionalColumn<Row>('latency', 'Latency', (r) => r.latencyMs, String) as unknown as { accessorFn: (r: Row) => number }
    expect(col.accessorFn({ requests: 0, tokensIn: 0, tokensOut: 0, costMicroUsd: 0 })).toBe(0)
  })

  it('accessorFn falls back to a caller-supplied sortSentinel for an undefined value', () => {
    const col = optionalColumn<Row>('latency', 'Latency', (r) => r.latencyMs, String, { sortSentinel: -1 }) as unknown as {
      accessorFn: (r: Row) => number
    }
    expect(col.accessorFn({ requests: 0, tokensIn: 0, tokensOut: 0, costMicroUsd: 0 })).toBe(-1)
  })

  it('accessorFn reads the real value when defined, ignoring the sentinel', () => {
    const col = optionalColumn<Row>('latency', 'Latency', (r) => r.latencyMs, String, { sortSentinel: -1 }) as unknown as {
      accessorFn: (r: Row) => number
    }
    expect(col.accessorFn({ requests: 0, tokensIn: 0, tokensOut: 0, costMicroUsd: 0, latencyMs: 120 })).toBe(120)
  })
})

describe('usageMetricColumns', () => {
  it('builds exactly requests/tokensIn/tokensOut/cost, in that order', () => {
    const columns = usageMetricColumns<Row>()
    expect(columns.map((c) => c.id)).toEqual(['requests', 'tokensIn', 'tokensOut', 'cost'])
  })

  it('each column\'s accessorFn reads its own field', () => {
    const row: Row = { requests: 1, tokensIn: 2, tokensOut: 3, costMicroUsd: 4 }
    const columns = usageMetricColumns<Row>() as unknown as { id: string; accessorFn: (r: Row) => number }[]
    expect(columns.find((c) => c.id === 'requests')?.accessorFn(row)).toBe(1)
    expect(columns.find((c) => c.id === 'tokensIn')?.accessorFn(row)).toBe(2)
    expect(columns.find((c) => c.id === 'tokensOut')?.accessorFn(row)).toBe(3)
    expect(columns.find((c) => c.id === 'cost')?.accessorFn(row)).toBe(4)
  })
})
