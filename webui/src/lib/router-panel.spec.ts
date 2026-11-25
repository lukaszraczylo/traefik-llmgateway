import { describe, expect, it } from 'vitest'

import { routerPanelModel } from '@/lib/router-panel'
import type { AdminRouterView } from '@/types/api'

function view(over: Partial<AdminRouterView> = {}): AdminRouterView {
  return {
    name: 'smartrouter/uni',
    defaultTier: 'low',
    shadowEnabled: false,
    classifier: 'v2',
    riskZ: 128,
    firstByteTimeout: '40s',
    selection: 'ordered',
    calibration: { mode: 'off', delta: [], maxLower: 0, maxRaise: 0, freeze: false },
    saturationDowngrade: false,
    downgradeBand: 100,
    downgraded: 0,
    timeoutFallback: true,
    fellBack: 2,
    tiers: [
      { name: 'low', models: ['uni/a'], minScore: 0 },
      { name: 'mid', models: ['uni/b'], minScore: 150 },
      { name: 'top', models: ['uni/c', 'uni/d'], minScore: 350 },
    ],
    keywordRules: [],
    maxAttempts: 4,
    contextEscalation: true,
    contextTokens: 524288,
    stats: {
      scope: 'shared',
      requests: 3,
      tiers: [
        { name: 'low', count: 1 },
        { name: 'mid', count: 1 },
        { name: 'top', count: 1 },
      ],
      reasons: [{ name: 'score', count: 3 }],
      escalations: [],
      shadow: [],
      outcomes: [{ tier: 'low', reason: 'score', outcome: 'ok', count: 3 }],
    },
    ...over,
  }
}

describe('routerPanelModel', () => {
  it('shadow null when stats.shadow empty', () => {
    expect(routerPanelModel(view()).shadow).toBeNull()
    expect(routerPanelModel(view({ stats: undefined })).shadow).toBeNull()
  })

  it('builds a live-by-shadow matrix in tier order when stats.shadow has cells', () => {
    const m = routerPanelModel(
      view({
        stats: {
          ...view().stats!,
          shadow: [
            { live: 'top', shadow: 'low', count: 4 },
            { live: 'low', shadow: 'low', count: 9 },
            { live: 'low', shadow: 'mid', count: 2 },
          ],
        },
      }),
    )
    expect(m.shadow).toEqual({ rows: ['low', 'mid', 'top'], cols: ['low', 'mid', 'top'], values: [[9, 2, 0], [0, 0, 0], [4, 0, 0]] })
  })

  it('split percentages sum to 100', () => {
    const counts = [1, 1, 1]
    const m = routerPanelModel(view({ stats: { ...view().stats!, tiers: counts.map((count, i) => ({ name: ['low', 'mid', 'top'][i], count })) } }))
    expect(m.split.map((s) => s.pct).reduce((a, b) => a + b, 0)).toBe(100)
    expect(m.split.map((s) => s.name)).toEqual(['low', 'mid', 'top'])
  })

  it('split is all zero without traffic and keeps unseen tiers', () => {
    const m = routerPanelModel(view({ stats: undefined }))
    expect(m.split).toEqual([
      { name: 'low', count: 0, pct: 0 },
      { name: 'mid', count: 0, pct: 0 },
      { name: 'top', count: 0, pct: 0 },
    ])
  })

  it('settings include firstByteTimeout and timeoutFallback', () => {
    const m = routerPanelModel(view())
    const byKey = Object.fromEntries(m.settings.map((s) => [s.key, s.value]))
    expect(byKey.firstByteTimeout).toBe('40s')
    expect(byKey.timeoutFallback).toBe('on')
    expect(byKey.contextTokens).toBe('524288')
  })

  it('shows unset optional settings as a dash, never undefined', () => {
    const m = routerPanelModel(view({ firstByteTimeout: undefined, contextTokens: undefined, timeoutFallback: false }))
    const byKey = Object.fromEntries(m.settings.map((s) => [s.key, s.value]))
    expect(byKey.firstByteTimeout).toBe('—')
    expect(byKey.timeoutFallback).toBe('off')
    expect(m.settings.every((s) => !/undefined|NaN/.test(s.value))).toBe(true)
  })

  it('totals outcomes in a fixed order and drops empty ones', () => {
    const m = routerPanelModel(
      view({
        stats: {
          ...view().stats!,
          outcomes: [
            { tier: 'low', reason: 'score', outcome: 'bad', count: 2 },
            { tier: 'low', reason: 'score', outcome: 'ok', count: 5 },
            { tier: 'mid', reason: 'bump', outcome: 'ok', count: 3 },
          ],
        },
      }),
    )
    expect(m.outcomes).toEqual([
      { outcome: 'ok', count: 8, pct: 80 },
      { outcome: 'bad', count: 2, pct: 20 },
    ])
  })

  it('lists tiers with models, default flag and request count', () => {
    const m = routerPanelModel(view())
    expect(m.tiers[2]).toEqual({ name: 'top', minScore: 350, models: ['uni/c', 'uni/d'], isDefault: false, count: 1 })
    expect(m.tiers[0].isDefault).toBe(true)
  })
})
