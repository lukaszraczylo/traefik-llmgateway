import { describe, expect, it } from 'vitest'

import { joinPricingHealth, pricingHealthOrder } from './pricing-health'
import type { AdminCatalogModel, AdminUsageModelEntry } from '@/types/api'

describe('joinPricingHealth', () => {
  const catalog = new Map<string, AdminCatalogModel>([
    [
      'a/priced',
      {
        id: 'a/priced',
        model: 'priced',
        priceSource: 'builtin',
        contextTokens: 128000,
        inputPerMTokUsd: 1,
        outputPerMTokUsd: 2,
        aliases: ['a/alias'],
      },
    ],
    ['a/free-suffix', { id: 'a/free-suffix', model: 'free-suffix:free', priceSource: 'unpriced', displayFree: true }],
  ])

  it('joins a served model with its catalog entry by canonical id', () => {
    const models: AdminUsageModelEntry[] = [
      { id: 'a/priced', value: 5, requests: 5, tokensIn: 100, tokensOut: 50, costMicroUsd: 1234, free: false },
    ]
    expect(joinPricingHealth(models, catalog)).toEqual([
      {
        id: 'a/priced',
        priceSource: 'builtin',
        displayFree: false,
        requests: 5,
        tokensIn: 100,
        tokensOut: 50,
        costMicroUsd: 1234,
        contextTokens: 128000,
        inputPerMTokUsd: 1,
        outputPerMTokUsd: 2,
        aliases: ['a/alias'],
      },
    ])
  })

  it('reads displayFree from the catalog join, not the ranking free flag', () => {
    const models: AdminUsageModelEntry[] = [{ id: 'a/free-suffix', value: 1, requests: 1, free: false }]
    expect(joinPricingHealth(models, catalog)[0]?.displayFree).toBe(true)
    expect(joinPricingHealth(models, catalog)[0]?.priceSource).toBe('unpriced')
  })

  it('skips a served id missing from the catalog entirely', () => {
    const models: AdminUsageModelEntry[] = [{ id: 'a/removed', value: 1, requests: 1 }]
    expect(joinPricingHealth(models, catalog)).toEqual([])
  })

  it('defaults every detail field to 0 when the ranking entry omits it', () => {
    const models: AdminUsageModelEntry[] = [{ id: 'a/priced', value: 1 }]
    const row = joinPricingHealth(models, catalog)[0]!
    expect(row.requests).toBe(0)
    expect(row.tokensIn).toBe(0)
    expect(row.tokensOut).toBe(0)
    expect(row.costMicroUsd).toBe(0)
  })

  it('returns an empty array for an empty ranking', () => {
    expect(joinPricingHealth([], catalog)).toEqual([])
  })

  // N1 (verify-redesign-final.md): an override-priced model must join
  // with its billed inputPerMTokUsd/outputPerMTokUsd present, not
  // undefined — PricingHealthTable.vue's own priceCell renders "—"
  // whenever either is undefined, which used to fire for EVERY
  // 'override' row regardless of whether a `pricing:` override was
  // actually configured (admin_catalog.go filled these fields from a
  // separate display resolution that an override-only model never
  // populated).
  it('joins an override-priced model with its billed price present, not "—"', () => {
    const overrideCatalog = new Map<string, AdminCatalogModel>([
      ['a/override', { id: 'a/override', model: 'override', priceSource: 'override', inputPerMTokUsd: 5, outputPerMTokUsd: 10 }],
    ])
    const models: AdminUsageModelEntry[] = [{ id: 'a/override', value: 1, requests: 3, tokensIn: 10, tokensOut: 5, costMicroUsd: 100 }]
    const row = joinPricingHealth(models, overrideCatalog)[0]!
    expect(row.priceSource).toBe('override')
    expect(row.inputPerMTokUsd).toBe(5)
    expect(row.outputPerMTokUsd).toBe(10)
  })
})

describe('pricingHealthOrder', () => {
  it('puts unpriced rows first regardless of their requests', () => {
    const rows = [
      { id: 'a', priceSource: 'builtin' as const, displayFree: false, requests: 1000, tokensIn: 0, tokensOut: 0, costMicroUsd: 0 },
      { id: 'b', priceSource: 'unpriced' as const, displayFree: false, requests: 1, tokensIn: 0, tokensOut: 0, costMicroUsd: 0 },
    ]
    expect(pricingHealthOrder(rows).map((r) => r.id)).toEqual(['b', 'a'])
  })

  it('orders the full priceSource priority: unpriced, override, builtin, litellm, free', () => {
    const rows = (['free', 'litellm', 'builtin', 'override', 'unpriced'] as const).map((priceSource, i) => ({
      id: `m${i}`,
      priceSource,
      displayFree: false,
      requests: 1,
      tokensIn: 0,
      tokensOut: 0,
      costMicroUsd: 0,
    }))
    expect(pricingHealthOrder(rows).map((r) => r.priceSource)).toEqual(['unpriced', 'override', 'builtin', 'litellm', 'free'])
  })

  it('breaks ties within the same priceSource by descending requests', () => {
    const rows = [
      { id: 'low', priceSource: 'unpriced' as const, displayFree: false, requests: 5, tokensIn: 0, tokensOut: 0, costMicroUsd: 0 },
      { id: 'high', priceSource: 'unpriced' as const, displayFree: false, requests: 50, tokensIn: 0, tokensOut: 0, costMicroUsd: 0 },
    ]
    expect(pricingHealthOrder(rows).map((r) => r.id)).toEqual(['high', 'low'])
  })

  it('does not mutate the input array', () => {
    const rows = [
      { id: 'a', priceSource: 'free' as const, displayFree: false, requests: 1, tokensIn: 0, tokensOut: 0, costMicroUsd: 0 },
      { id: 'b', priceSource: 'unpriced' as const, displayFree: false, requests: 1, tokensIn: 0, tokensOut: 0, costMicroUsd: 0 },
    ]
    const copy = [...rows]
    pricingHealthOrder(rows)
    expect(rows).toEqual(copy)
  })
})
