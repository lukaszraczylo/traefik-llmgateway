import { createSSRApp } from 'vue'
import { renderToString } from 'vue/server-renderer'
import { describe, expect, it } from 'vitest'

import SmartRoutersCard from '@/components/SmartRoutersCard.vue'
import type { AdminRouterView } from '@/types/api'

const router: AdminRouterView = {
  name: 'auto',
  defaultTier: 'small',
  selection: 'hash',
  maxAttempts: 3,
  contextEscalation: false,
  shadowEnabled: false,
  classifier: 'v1',
  riskZ: 128,
  calibration: { mode: 'off', delta: [], maxLower: 150, maxRaise: 0, freeze: false },
  saturationDowngrade: false,
  downgradeBand: 100,
  downgraded: 0,
  timeoutFallback: false,
  fellBack: 0,
  tiers: [
    { name: 'small', minScore: 0, models: ['openai/gpt-5-mini'] },
    { name: 'big', minScore: 400, models: ['anthropic/claude-opus', 'openai/gpt-5'] },
  ],
  keywordRules: [{ tier: 'big', keywords: ['prove', 'audit'] }],
  contextTokens: 262144,
  stats: {
    scope: 'shared',
    requests: 4,
    tiers: [
      { name: 'small', count: 1 },
      { name: 'big', count: 3 },
    ],
    reasons: [
      { name: 'keyword', count: 1 },
      { name: 'score', count: 3 },
    ],
    escalations: [{ from: 'small', to: 'big', count: 2 }],
    shadow: [],
    outcomes: [],
  },
}

function render(routers: AdminRouterView[]): Promise<string> {
  return renderToString(createSSRApp(SmartRoutersCard, { routers }))
}

describe('SmartRoutersCard', () => {
  it('shows an empty message with no routers', async () => {
    expect(await render([])).toContain('None configured.')
  })

  it('shows calibration, adaptive selection and downgrade settings', async () => {
    const live: AdminRouterView = {
      ...router,
      selection: 'adaptive',
      adaptive: { explorePct: 5, windowHours: 6, latencyWeightPct: 30 },
      calibration: { mode: 'shadow', delta: [{ tier: 'big', delta: 25 }], maxLower: 150, maxRaise: 0, freeze: false },
      saturationDowngrade: true,
      downgradeBand: 100,
      downgraded: 7,
    }
    const html = await render([live])
    expect(html).toContain('selection: adaptive')
    expect(html).toContain('explore 5%, window 6h')
    expect(html).toContain('calibration: shadow')
    expect(html).toContain('offsets (not applied): big -25')
    expect(html).toContain('saturation downgrade: on, band 100')
    expect(html).toContain('downgraded: 7')
    const off = await render([router])
    expect(off).toContain('calibration: off')
    expect(off).toContain('saturation downgrade: off')
    expect(off).not.toContain('offsets')
    expect(off).not.toContain('explore')
    expect(off).not.toContain('downgraded:')
  })

  it('shows the timeout fallback flag and its count only when on', async () => {
    const on = await render([{ ...router, timeoutFallback: true, fellBack: 3 }])
    expect(on).toContain('timeout fallback: on')
    expect(on).toContain('fell back: 3')
    const off = await render([router])
    expect(off).toContain('timeout fallback: off')
    expect(off).not.toContain('fell back:')
  })

  it('renders router settings, tiers and keyword rules', async () => {
    const html = await render([router])
    expect(html).toContain('Smart routers')
    expect(html).toContain('auto')
    expect(html).toContain('default tier: small')
    expect(html).toContain('selection: hash')
    expect(html).toContain('max attempts: 3')
    expect(html).toContain('context escalation: off')
    expect(html).toContain('from score 400')
    expect(html).toContain('anthropic/claude-opus, openai/gpt-5')
    expect(html).toContain('prove, audit')
  })

  it('shows the first byte timeout only when set', async () => {
    expect(await render([{ ...router, firstByteTimeout: '40s' }])).toContain('first byte timeout: 40s')
    expect(await render([router])).not.toContain('first byte timeout')
  })

  it('shows the tools min tier only when set', async () => {
    expect(await render([{ ...router, toolsMinTier: 'big' }])).toContain('tools min tier: big')
    expect(await render([router])).not.toContain('tools min tier')
  })

  it('shows the classifier and the risk margin only when v2 is involved', async () => {
    const plain = await render([router])
    expect(plain).toContain('classifier: v1')
    expect(plain).not.toContain('risk z')
    expect(plain).not.toContain('shadow classifier')
    const shadowed = await render([{ ...router, shadowEnabled: true, shadowClassifier: 'v2', riskZ: 90 }])
    expect(shadowed).toContain('shadow classifier: v2')
    expect(shadowed).toContain('risk z: 90')
    expect(await render([{ ...router, classifier: 'v2' }])).toContain('risk z: 128')
  })

  it('renders the shadow agreement matrix only when shadow weights are set', async () => {
    const shadowed: AdminRouterView = {
      ...router,
      shadowEnabled: true,
      stats: {
        ...router.stats!,
        shadow: [
          { live: 'small', shadow: 'small', count: 6 },
          { live: 'big', shadow: 'small', count: 2 },
          { live: 'big', shadow: 'big', count: 2 },
        ],
      },
    }
    const html = await render([shadowed])
    expect(html).toContain('Shadow weights')
    expect(html).toContain('aria-label="Shadow agreement matrix for auto"')
    expect(html).toContain('agreement: 80%')
    expect(html).toMatch(/<th[^>]*>small<\/th>/)
    expect(html).toMatch(/data-cell="big:small"[^>]*>2</)
    expect(html).toMatch(/data-cell="small:big"[^>]*>0</)
    expect(await render([router])).not.toContain('Shadow weights')
  })

  it('shows an empty shadow matrix without NaN', async () => {
    const html = await render([{ ...router, shadowEnabled: true }])
    expect(html).toContain('Shadow weights')
    expect(html).toContain('agreement: 0%')
    expect(html).not.toContain('NaN')
  })

  it('renders outcome counts per tier summed over reasons', async () => {
    const withOutcomes: AdminRouterView = {
      ...router,
      stats: {
        ...router.stats!,
        outcomes: [
          { tier: 'small', reason: 'score', outcome: 'ok', count: 3 },
          { tier: 'small', reason: 'keyword', outcome: 'ok', count: 1 },
          { tier: 'small', reason: 'score', outcome: 'escalated', count: 2 },
          { tier: 'big', reason: 'score', outcome: 'error', count: 1 },
          { tier: 'big', reason: 'score', outcome: 'bad', count: 2 },
          { tier: 'big', reason: 'score', outcome: 'slow', count: 5 },
          { tier: 'big', reason: 'keyword', outcome: 'slow', count: 1 },
        ],
      },
    }
    const html = await render([withOutcomes])
    expect(html).toContain('aria-label="Outcomes for auto"')
    expect(html).toMatch(/data-cell="small:ok"[^>]*>4</)
    expect(html).toMatch(/data-cell="small:escalated"[^>]*>2</)
    expect(html).toMatch(/data-cell="big:error"[^>]*>1</)
    expect(html).toMatch(/data-cell="big:bad"[^>]*>2</)
    expect(html).toMatch(/<th[^>]*>slow<\/th>/)
    expect(html).toMatch(/data-cell="big:slow"[^>]*>6</)
    expect(html).toMatch(/data-cell="small:slow"[^>]*>0</)
    expect(await render([router])).not.toContain('Outcomes for')
  })

  it('labels shadow and outcome scope like the other counts', async () => {
    const html = await render([{ ...router, shadowEnabled: true, stats: { ...router.stats!, scope: 'instance' } }])
    expect(html).toContain('This gateway instance, since start')
  })

  it('shows the context window, per-tier counts and shares, reasons and escalations', async () => {
    const html = await render([router])
    expect(html).toContain('context window: 256k')
    expect(html).toContain('requests: 4')
    expect(html).toContain('1 req (25%)')
    expect(html).toContain('3 req (75%)')
    expect(html).toContain('width:75%')
    expect(html).toContain('keyword: 1')
    expect(html).toContain('score: 3')
    expect(html).toMatch(/Escalated[\s\S]*small[\s\S]*to[\s\S]*big[\s\S]*: 2/)
    expect(html).toContain('All replicas, this month (UTC)')
  })

  it('labels per-instance counts when Redis is not shared', async () => {
    const html = await render([{ ...router, stats: { ...router.stats!, scope: 'instance' } }])
    expect(html).toContain('This gateway instance, since start')
    expect(html).not.toContain('All replicas')
  })

  it('renders zero counts and no window without stats', async () => {
    const { contextTokens: _c, stats: _s, ...bare } = router
    const html = await render([bare])
    expect(html).not.toContain('context window:')
    expect(html).toContain('requests: 0')
    expect(html).toContain('0 req (0%)')
    expect(html).not.toContain('Routing reasons')
    expect(html).not.toContain('Escalated')
  })

  it('handles zero-filled stats without dividing by zero', async () => {
    const zero: AdminRouterView = {
      ...router,
      stats: { scope: 'instance', requests: 0, tiers: [{ name: 'small', count: 0 }, { name: 'big', count: 0 }], reasons: [], escalations: [], shadow: [], outcomes: [] },
    }
    const html = await render([zero])
    expect(html).toContain('0 req (0%)')
    expect(html).not.toContain('NaN')
  })
})
