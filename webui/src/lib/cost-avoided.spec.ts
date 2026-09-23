import { describe, expect, it } from 'vitest'

import { costAvoidedMicros, freeRows, pickReferenceModel } from './cost-avoided'
import type { AdminCatalogModel, AdminUsageModelEntry } from '@/types/api'

describe('freeRows', () => {
  it('keeps only entries with free === true, reading out their token counts', () => {
    const models: AdminUsageModelEntry[] = [
      { id: 'a/free-model:free', value: 0, free: true, tokensIn: 100, tokensOut: 50 },
      { id: 'b/paid-model', value: 10, free: false, tokensIn: 20, tokensOut: 10 },
    ]
    expect(freeRows(models)).toEqual([{ tokensIn: 100, tokensOut: 50 }])
  })

  it('defaults missing token counts to 0', () => {
    const models: AdminUsageModelEntry[] = [{ id: 'a/free', value: 0, free: true }]
    expect(freeRows(models)).toEqual([{ tokensIn: 0, tokensOut: 0 }])
  })

  it('returns an empty array when nothing is free', () => {
    const models: AdminUsageModelEntry[] = [{ id: 'a/paid', value: 5, free: false }]
    expect(freeRows(models)).toEqual([])
  })
})

describe('costAvoidedMicros', () => {
  it('sums tokensIn*refIn + tokensOut*refOut across rows, already in micro-USD', () => {
    // 1_000_000 tokensIn at $1/MTok = 1,000,000 micros ($1); 500_000
    // tokensOut at $2/MTok = 1,000,000 micros ($1). Total 2,000,000.
    const rows = [{ tokensIn: 1_000_000, tokensOut: 500_000 }]
    expect(costAvoidedMicros(rows, 1, 2)).toBe(2_000_000)
  })

  it('sums across multiple rows', () => {
    const rows = [
      { tokensIn: 1_000_000, tokensOut: 0 },
      { tokensIn: 0, tokensOut: 1_000_000 },
    ]
    expect(costAvoidedMicros(rows, 0.5, 1.5)).toBe(500_000 + 1_500_000)
  })

  it('returns 0 for an empty rows array', () => {
    expect(costAvoidedMicros([], 1, 1)).toBe(0)
  })

  it('rounds the summed total to the nearest whole micro-USD', () => {
    const rows = [{ tokensIn: 1, tokensOut: 1 }]
    expect(costAvoidedMicros(rows, 0.33, 0.33)).toBe(Math.round(0.33 + 0.33))
  })
})

describe('pickReferenceModel', () => {
  const catalog = new Map<string, AdminCatalogModel>([
    ['a/paid-high', { id: 'a/paid-high', model: 'paid-high', priceSource: 'builtin' }],
    ['a/paid-low', { id: 'a/paid-low', model: 'paid-low', priceSource: 'override' }],
    ['a/free', { id: 'a/free', model: 'free', priceSource: 'free' }],
    ['a/unpriced', { id: 'a/unpriced', model: 'unpriced', priceSource: 'unpriced' }],
  ])

  it('picks the most-requested model among genuinely priced catalog entries', () => {
    const models: AdminUsageModelEntry[] = [
      { id: 'a/paid-low', value: 1, requests: 5 },
      { id: 'a/paid-high', value: 1, requests: 50 },
    ]
    expect(pickReferenceModel(models, catalog)).toBe('a/paid-high')
  })

  it('skips free and unpriced catalog entries even if most-requested', () => {
    const models: AdminUsageModelEntry[] = [
      { id: 'a/free', value: 1, requests: 1000 },
      { id: 'a/paid-low', value: 1, requests: 1 },
    ]
    expect(pickReferenceModel(models, catalog)).toBe('a/paid-low')
  })

  it('skips a served id missing from the catalog entirely', () => {
    const models: AdminUsageModelEntry[] = [{ id: 'a/removed', value: 1, requests: 999 }]
    expect(pickReferenceModel(models, catalog)).toBeNull()
  })

  it('skips a detail-less entry with no requests field', () => {
    const models: AdminUsageModelEntry[] = [{ id: 'a/paid-high', value: 1 }]
    expect(pickReferenceModel(models, catalog)).toBeNull()
  })

  it('returns null when nothing qualifies', () => {
    const models: AdminUsageModelEntry[] = [{ id: 'a/free', value: 1, requests: 10 }]
    expect(pickReferenceModel(models, catalog)).toBeNull()
  })

  it('returns null for an empty ranking', () => {
    expect(pickReferenceModel([], catalog)).toBeNull()
  })
})

/**
 * N1 (verify-redesign-final.md): CostAvoidedCard.vue's own `avoidedMicros`
 * computed is exactly `pickReferenceModel` feeding `costAvoidedMicros`
 * with the picked model's `cat.inputPerMTokUsd`/`outputPerMTokUsd` — this
 * end-to-end shape is what broke when the catalog reported `priceSource:
 * 'override'` alongside undefined prices (admin_catalog.go's now-fixed
 * bug). This exercises the same two-step flow directly against a fixed-
 * shape catalog (billing price present for every priced source,
 * including 'override' — the contract GET /admin/api/catalog now
 * guarantees) so a regression back to undefined-but-priced catalog
 * entries fails here, not just in a Go-only test.
 */
describe('pickReferenceModel + costAvoidedMicros end-to-end (N1 regression)', () => {
  it('computes a non-null avoided-cost figure when the picked reference model is override-priced', () => {
    const catalog = new Map<string, AdminCatalogModel>([
      ['a/override-priced', { id: 'a/override-priced', model: 'override-priced', priceSource: 'override', inputPerMTokUsd: 5, outputPerMTokUsd: 10 }],
    ])
    const models: AdminUsageModelEntry[] = [
      { id: 'a/override-priced', value: 1, requests: 42, free: false },
      { id: 'a/free-model', value: 0, free: true, tokensIn: 1_000_000, tokensOut: 500_000 },
    ]

    const picked = pickReferenceModel(models, catalog)
    expect(picked).toBe('a/override-priced')

    const cat = catalog.get(picked as string)
    expect(cat?.inputPerMTokUsd).toBe(5)
    expect(cat?.outputPerMTokUsd).toBe(10)

    const avoided = costAvoidedMicros(freeRows(models), cat?.inputPerMTokUsd as number, cat?.outputPerMTokUsd as number)
    expect(avoided).toBe(1_000_000 * 5 + 500_000 * 10)
  })
})
