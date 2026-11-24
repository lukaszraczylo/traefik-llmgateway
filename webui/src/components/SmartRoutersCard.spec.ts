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
  },
}

function render(routers: AdminRouterView[]): Promise<string> {
  return renderToString(createSSRApp(SmartRoutersCard, { routers }))
}

describe('SmartRoutersCard', () => {
  it('shows an empty message with no routers', async () => {
    expect(await render([])).toContain('None configured.')
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
      stats: { scope: 'instance', requests: 0, tiers: [{ name: 'small', count: 0 }, { name: 'big', count: 0 }], reasons: [], escalations: [] },
    }
    const html = await render([zero])
    expect(html).toContain('0 req (0%)')
    expect(html).not.toContain('NaN')
  })
})
