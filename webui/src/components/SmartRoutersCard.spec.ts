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
})
