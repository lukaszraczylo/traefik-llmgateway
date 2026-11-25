import { describe, expect, it } from 'vitest'

import { costAvoidedMicros, costComparisons, COST_REFERENCES, freeRows, latestSonnet } from './cost-avoided'
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

const model = (id: string, priceSource: AdminCatalogModel['priceSource'], inP?: number, outP?: number): [string, AdminCatalogModel] => [
  id,
  { id, model: id.split('/')[1] ?? id, priceSource, inputPerMTokUsd: inP, outputPerMTokUsd: outP },
]
const SONNETS = [
  model('anthropic/claude-sonnet-4-5-20250929', 'litellm', 3, 15),
  model('anthropic/claude-sonnet-4-6', 'litellm', 3, 15),
  model('anthropic/claude-sonnet-5', 'litellm', 2, 10),
  model('anthropic/claude-sonnet-5-5', 'litellm', 2, 10),
]
const MINIMAX_ID = 'minimax/MiniMax-M3.1-Flash'

describe('latestSonnet', () => {
  it('picks 5-5 over 5, 4-6 and the dated 4-5', () => {
    expect(latestSonnet(new Map(SONNETS))?.id).toBe('anthropic/claude-sonnet-5-5')
  })

  it('treats a date suffix as a tie-breaker, never a version', () => {
    const catalog = new Map([model('anthropic/claude-sonnet-4-5-20250929', 'litellm', 3, 15), model('anthropic/claude-sonnet-4-6', 'litellm', 3, 15)])
    expect(latestSonnet(catalog)?.id).toBe('anthropic/claude-sonnet-4-6')
    const sameVersion = new Map([model('anthropic/claude-sonnet-4-5-20250101', 'litellm', 3, 15), model('anthropic/claude-sonnet-4-5-20250929', 'litellm', 3, 15)])
    expect(latestSonnet(sameVersion)?.id).toBe('anthropic/claude-sonnet-4-5-20250929')
  })

  it('skips unpriced and free Sonnets', () => {
    const catalog = new Map([...SONNETS, model('anthropic/claude-sonnet-6', 'unpriced'), model('anthropic/claude-sonnet-7', 'free', 0, 0)])
    expect(latestSonnet(catalog)?.id).toBe('anthropic/claude-sonnet-5-5')
  })

  it('returns null when no Sonnet is priced', () => {
    expect(latestSonnet(new Map([model('anthropic/claude-sonnet-5', 'unpriced')]))).toBeNull()
    expect(latestSonnet(new Map())).toBeNull()
  })
})

describe('costComparisons', () => {
  const rows = [{ tokensIn: 1_000_000, tokensOut: 500_000 }]

  it('returns MiniMax then the latest Sonnet', () => {
    const out = costComparisons(rows, new Map(SONNETS))
    expect(out.map((r) => r.id)).toEqual([COST_REFERENCES.minimax.id, 'anthropic/claude-sonnet-5-5'])
  })

  it('uses the reference rate when the catalog has MiniMax unpriced', () => {
    const [mm] = costComparisons(rows, new Map([model(MINIMAX_ID, 'unpriced')]))
    expect(mm).toMatchObject({ inPerMTok: 0.15, outPerMTok: 0.6, priceLabel: 'MiniMax list price, Nov 2026' })
    expect(mm.avoidedMicros).toBe(Math.round(1_000_000 * 0.15 + 500_000 * 0.6))
  })

  it('uses the catalog rate when the catalog has MiniMax priced', () => {
    const [mm] = costComparisons(rows, new Map([model(MINIMAX_ID, 'litellm', 0.3, 1.2)]))
    expect(mm).toMatchObject({ inPerMTok: 0.3, outPerMTok: 1.2, priceLabel: 'gateway price' })
    expect(mm.avoidedMicros).toBe(1_000_000 * 0.3 + 500_000 * 1.2)
  })

  it('prices Sonnet from the catalog', () => {
    const [, son] = costComparisons(rows, new Map(SONNETS))
    expect(son).toMatchObject({ inPerMTok: 2, outPerMTok: 10, priceLabel: 'gateway price', avoidedMicros: 1_000_000 * 2 + 500_000 * 10 })
  })

  it('gives null avoided cost when no Sonnet is priced', () => {
    const [, son] = costComparisons(rows, new Map())
    expect(son.avoidedMicros).toBeNull()
    expect(son.inPerMTok).toBeNull()
  })

  it('gives 0, never NaN, for zero free traffic', () => {
    const out = costComparisons([], new Map(SONNETS))
    expect(out.map((r) => r.avoidedMicros)).toEqual([0, 0])
  })
})
