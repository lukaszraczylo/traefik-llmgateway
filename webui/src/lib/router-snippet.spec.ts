import { describe, expect, it } from 'vitest'

import { routerSnippet, validateRouter } from './snippets'
import type { RouterInput } from './snippets'

const base: RouterInput = {
  tiers: [
    { name: 'small', minScore: 0, models: ['openai/gpt-5-mini'] },
    { name: 'big', minScore: 400, models: ['anthropic/claude-opus', 'openai/gpt-5'] },
  ],
  keywordRules: [],
  selection: 'ordered',
  maxAttempts: 0,
  contextEscalation: true,
}

describe('routerSnippet: golden YAML, spec.plugin.llmgateway.smartRouters.<name>', () => {
  it('omits every default', () => {
    expect(routerSnippet('auto', base)).toBe(
      [
        'spec:',
        '  plugin:',
        '    llmgateway:',
        '      smartRouters:',
        '        auto:',
        '          tiers:',
        '            - {"name":"small","minScore":0,"models":["openai/gpt-5-mini"]}',
        '            - {"name":"big","minScore":400,"models":["anthropic/claude-opus","openai/gpt-5"]}',
      ].join('\n'),
    )
  })

  it('states non-default selection, attempts, escalation and keyword rules', () => {
    const out = routerSnippet('auto', {
      ...base,
      selection: 'hash',
      maxAttempts: 3,
      contextEscalation: false,
      keywordRules: [{ tier: 'big', keywords: ['prove'] }],
    })
    expect(out).toContain('keywordRules:')
    expect(out).toContain('selection: hash')
    expect(out).toContain('maxAttempts: 3')
    expect(out).toContain('contextEscalation: false')
    expect(out).toContain('"keywords":["prove"]')
  })
})

describe('validateRouter', () => {
  it('accepts a valid router', () => {
    expect(validateRouter(base)).toEqual([])
  })

  it('rejects no tiers', () => {
    expect(validateRouter({ ...base, tiers: [] })).toEqual(['Add at least one tier.'])
  })

  it('requires first minScore 0 and rising scores', () => {
    const errs = validateRouter({
      ...base,
      tiers: [
        { name: 'a', minScore: 100, models: ['m1'] },
        { name: 'b', minScore: 100, models: ['m2'] },
      ],
    })
    expect(errs.join(' ')).toContain('must have minScore 0')
    expect(errs.join(' ')).toContain('greater than the previous')
  })

  it('rejects overlapping models, duplicate tiers and empty tiers', () => {
    const errs = validateRouter({
      ...base,
      tiers: [
        { name: 'a', minScore: 0, models: ['m1'] },
        { name: 'a', minScore: 10, models: ['m1'] },
        { name: 'c', minScore: 20, models: [] },
      ],
    })
    expect(errs.join(' ')).toContain('used twice')
    expect(errs.join(' ')).toContain('must be disjoint')
    expect(errs.join(' ')).toContain('needs at least one model')
  })

  it('rejects a keyword rule for an unknown tier', () => {
    expect(validateRouter({ ...base, keywordRules: [{ tier: 'nope', keywords: ['x'] }] }).join(' ')).toContain('is not a tier')
  })
})
