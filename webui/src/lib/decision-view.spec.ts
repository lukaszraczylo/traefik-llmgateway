import { describe, expect, it } from 'vitest'

import { decisionView, hasDecision } from '@/lib/decision-view'
import type { AdminRouterView } from '@/types/api'

function view(over: Partial<AdminRouterView> = {}): AdminRouterView {
  return {
    name: 'smartrouter/uni',
    defaultTier: 'low',
    shadowEnabled: false,
    classifier: 'v2',
    riskZ: 128,
    selection: 'ordered',
    calibration: { mode: 'off', delta: [], maxLower: 0, maxRaise: 0, freeze: false },
    saturationDowngrade: false,
    downgradeBand: 100,
    downgraded: 0,
    timeoutFallback: false,
    fellBack: 0,
    tiers: [
      { name: 'low', models: ['a'], minScore: 0 },
      { name: 'mid', models: ['b'], minScore: 150 },
      { name: 'top', models: ['c'], minScore: 350 },
    ],
    keywordRules: [],
    maxAttempts: 2,
    contextEscalation: false,
    decision: { provider: 'gx10', path: '/v1/decide', mode: 'live', timeout: '500ms', minProb: 700, band: 100 },
    stats: {
      scope: 'shared',
      requests: 10,
      tiers: [],
      reasons: [],
      escalations: [],
      shadow: [],
      outcomes: [],
      decision: [{ heuristic: 'mid', model: 'top', count: 40 }],
      decisionOutcomes: [
        { name: 'agree', count: 5 },
        { name: 'override', count: 40 },
      ],
    },
    ...over,
  }
}

describe('decisionView', () => {
  it('puts mid to top count in the grid by tier order', () => {
    const grid = decisionView(view())!.grid!
    expect(grid.rows).toEqual(['low', 'mid', 'top'])
    expect(grid.values[grid.rows.indexOf('mid')][grid.cols.indexOf('top')]).toBe(40)
  })

  it('lists outcomes by name', () => {
    const d = decisionView(view())!
    expect(d.outcomes).toContainEqual({ name: 'override', count: 40 })
  })

  it('summarises the config', () => {
    expect(decisionView(view())!.summary).toBe('gx10 · live · 500ms · ≥70%')
  })

  it('hides the section without a decision block', () => {
    const v = view({ decision: undefined })
    expect(hasDecision(v)).toBe(false)
    expect(decisionView(v)).toBeNull()
  })
})
