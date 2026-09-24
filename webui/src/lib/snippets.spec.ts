import { describe, expect, it, vi } from 'vitest'

import {
  API_KEY_PLACEHOLDER,
  generateApiKey,
  groupGrantSnippet,
  groupLimitsSnippet,
  isValidName,
  isValidNonNegative,
  isValidNonNegativeInt,
  isValidUserName,
  modelMetaSnippet,
  parseList,
  pricingSnippet,
  userGrantJsonFragment,
  userJsonLine,
  userLimitsJsonFragment,
  validatePersonalGrant,
} from './snippets'

describe('parseList', () => {
  it('splits on commas and trims whitespace', () => {
    expect(parseList('anthropic, openai ,  gx10')).toEqual(['anthropic', 'openai', 'gx10'])
  })

  it('drops empty entries (blank field, trailing comma, doubled comma)', () => {
    expect(parseList('')).toEqual([])
    expect(parseList('anthropic,')).toEqual(['anthropic'])
    expect(parseList('anthropic,,openai')).toEqual(['anthropic', 'openai'])
    expect(parseList('   ')).toEqual([])
  })

  it('de-duplicates, keeping first-seen order', () => {
    expect(parseList('anthropic, openai, anthropic')).toEqual(['anthropic', 'openai'])
  })
})

// Golden strings below are checked against the REAL production shapes in
// home-cluster/kubernetes/namespaces/traefik/base/llmgateway/middleware.yaml
// and users-configmap.yaml (read this session) — structure and field order
// only. Every value used here is fabricated/public (model ids, provider
// names, group names are not secrets); no api key or password literal from
// either production file appears anywhere in this file.

describe('groupLimitsSnippet: golden YAML, spec.plugin.llmgateway.groups.<name>.limits', () => {
  it('renders one configured field', () => {
    expect(groupLimitsSnippet('friends', { requestsPerDay: 1000 })).toBe(
      ['spec:', '  plugin:', '    llmgateway:', '      groups:', '        friends:', '          limits:', '            requestsPerDay: 1000'].join(
        '\n',
      ),
    )
  })

  it('renders every configured field in LimitsConfig field order, regardless of input key order', () => {
    const snippet = groupLimitsSnippet('eng', {
      costPerMonthUSD: 50,
      requestsPerMinute: 10,
      tokensPerDay: 500000,
    })
    expect(snippet).toBe(
      [
        'spec:',
        '  plugin:',
        '    llmgateway:',
        '      groups:',
        '        eng:',
        '          limits:',
        '            requestsPerMinute: 10',
        '            tokensPerDay: 500000',
        '            costPerMonthUSD: 50',
      ].join('\n'),
    )
  })

  it('omits an unset (0/undefined) field entirely, never emitting a fabricated 0', () => {
    const snippet = groupLimitsSnippet('friends', { requestsPerDay: 0, costPerDayUSD: 5 })
    expect(snippet).not.toContain('requestsPerDay')
    expect(snippet).toContain('costPerDayUSD: 5')
  })
})

describe('groupGrantSnippet: golden YAML, matches middleware.yaml groups.friends/groups.home shape', () => {
  it('emits every one of the four access lists explicitly, even when empty — "friends" shape (providers set, rest empty)', () => {
    const snippet = groupGrantSnippet('friends', { providers: ['minimax', 'uni'], models: [], mcpServers: [], agents: [] })
    expect(snippet).toBe(
      [
        'spec:',
        '  plugin:',
        '    llmgateway:',
        '      groups:',
        '        friends:',
        '          providers: [minimax, uni]',
        '          models: []',
        '          mcpServers: []',
        '          agents: []',
      ].join('\n'),
    )
  })

  it('renders "home" shape — every list empty (unrestricted)', () => {
    const snippet = groupGrantSnippet('home', { providers: [], models: [], mcpServers: [], agents: [] })
    expect(snippet).toBe(
      [
        'spec:',
        '  plugin:',
        '    llmgateway:',
        '      groups:',
        '        home:',
        '          providers: []',
        '          models: []',
        '          mcpServers: []',
        '          agents: []',
      ].join('\n'),
    )
  })
})

describe('modelMetaSnippet: golden YAML flow map, matches middleware.yaml modelMeta entries exactly', () => {
  it('matches the gx10-embed entry shape: "{ contextTokens: 16384, free: true }"', () => {
    const snippet = modelMetaSnippet('gx10-embed/text-embedding-qwen3-0.6b', { contextTokens: 16384, free: true })
    expect(snippet).toBe(
      ['spec:', '  plugin:', '    llmgateway:', '      modelMeta:', '        "gx10-embed/text-embedding-qwen3-0.6b": { contextTokens: 16384, free: true }'].join(
        '\n',
      ),
    )
  })

  it('matches the "gx10/current" entry shape: "{ free: true }"', () => {
    const snippet = modelMetaSnippet('gx10/current', { free: true })
    expect(snippet.split('\n').at(-1)).toBe('        "gx10/current": { free: true }')
  })

  // P2 item 11: modelMeta keys are ALWAYS quoted in production, even a
  // bare id with no "/" — unlike pricingSnippet below, whose own golden
  // test (examples/kubernetes.yaml, read this session) shows an
  // unquoted bare id in that DIFFERENT config section.
  it('a bare model id with no "/" or ":" is still quoted — modelMeta always quotes, unlike pricing', () => {
    const snippet = modelMetaSnippet('ornith-397b', { contextTokens: 262144, free: true })
    expect(snippet.split('\n').at(-1)).toBe('        "ornith-397b": { contextTokens: 262144, free: true }')
  })

  it('field order is always contextTokens, free — contextTokens alone', () => {
    const snippet = modelMetaSnippet('openai/gpt-4', { contextTokens: 128000 })
    expect(snippet.split('\n').at(-1)).toBe('        "openai/gpt-4": { contextTokens: 128000 }')
  })

  it('omits every field the caller did not set — an empty ModelMeta renders "{}"', () => {
    const snippet = modelMetaSnippet('openai/gpt-4', {})
    expect(snippet.split('\n').at(-1)).toBe('        "openai/gpt-4": {}')
  })
})

describe('pricingSnippet: golden YAML flow map, matches examples/kubernetes.yaml pricing entry exactly', () => {
  it('matches the "gpt-5-mini" entry shape: "pricing:\\n  gpt-5-mini: {inputPerM: 0.25, outputPerM: 2.0}"', () => {
    const snippet = pricingSnippet('gpt-5-mini', { inputPerM: 0.25, outputPerM: 2.0 })
    expect(snippet).toBe(
      ['spec:', '  plugin:', '    llmgateway:', '      pricing:', '        gpt-5-mini: { inputPerM: 0.25, outputPerM: 2 }'].join('\n'),
    )
  })

  it('nests under spec.plugin.llmgateway.pricing, NOT modelMeta — a different config section from ModelMetaForm', () => {
    const snippet = pricingSnippet('openai/gpt-4', { inputPerM: 0.5, outputPerM: 1.5 })
    expect(snippet).toContain('      pricing:')
    expect(snippet).not.toContain('modelMeta')
  })

  it('quotes a "provider/model" canonical id (contains "/")', () => {
    const snippet = pricingSnippet('openai/gpt-4', { inputPerM: 0.5, outputPerM: 1.5 })
    expect(snippet.split('\n').at(-1)).toBe('        "openai/gpt-4": { inputPerM: 0.5, outputPerM: 1.5 }')
  })

  it('field order is always inputPerM then outputPerM', () => {
    const snippet = pricingSnippet('bare-model', { inputPerM: 1, outputPerM: 2 })
    expect(snippet.split('\n').at(-1)).toBe('        bare-model: { inputPerM: 1, outputPerM: 2 }')
  })
})

describe('userLimitsJsonFragment / userGrantJsonFragment: bare JSON fields for an EXISTING users.json entry', () => {
  it('renders the limits fragment in LimitsConfig field order', () => {
    expect(userLimitsJsonFragment({ costPerMonthUSD: 5, requestsPerDay: 100 })).toBe('"limits": {"requestsPerDay": 100, "costPerMonthUSD": 5}')
  })

  it('renders the grant fragment — providers then models, both present', () => {
    expect(userGrantJsonFragment({ providers: ['gx10'], models: ['gx10/current'] })).toBe('"providers": ["gx10"], "models": ["gx10/current"]')
  })

  it('renders only providers when models is empty — matches barteq\'s real shape ("providers": ["gx10"]) structurally', () => {
    expect(userGrantJsonFragment({ providers: ['gx10'], models: [] })).toBe('"providers": ["gx10"]')
  })
})

describe('userJsonLine: golden JSON, matches users-configmap.yaml entry shape exactly (fabricated apiKey, never a real one)', () => {
  it('matches the "barteq" entry shape: name, apiKey, group, providers', () => {
    const line = userJsonLine({ name: 'barteq', apiKey: 'sk-test-FAKEKEY0000000000000000000000000000', group: 'friends', providers: ['gx10'] })
    expect(line).toBe('{"name": "barteq", "apiKey": "sk-test-FAKEKEY0000000000000000000000000000", "group": "friends", "providers": ["gx10"]}')
  })

  it('matches the "admin" entry shape: name, apiKey, group, admin', () => {
    const line = userJsonLine({ name: 'admin', apiKey: 'sk-test-FAKEKEY0000000000000000000000000000', group: 'home', admin: true })
    expect(line).toBe('{"name": "admin", "apiKey": "sk-test-FAKEKEY0000000000000000000000000000", "group": "home", "admin": true}')
  })

  it('matches a minimal entry shape: name, apiKey, group only (no providers/admin)', () => {
    const line = userJsonLine({ name: 'yalso', apiKey: 'sk-test-FAKEKEY0000000000000000000000000000', group: 'home' })
    expect(line).toBe('{"name": "yalso", "apiKey": "sk-test-FAKEKEY0000000000000000000000000000", "group": "home"}')
  })

  it('emits "groups" (plural) instead of "group" when multiple groups are set', () => {
    const line = userJsonLine({ name: 'multi', apiKey: 'sk-test-x', groups: ['home', 'friends'] })
    expect(line).toBe('{"name": "multi", "apiKey": "sk-test-x", "groups": ["home", "friends"]}')
  })

  it('includes a nested limits object and admin, in key order name/apiKey/group/groups/providers/models/limits/admin', () => {
    const line = userJsonLine({
      name: 'full',
      apiKey: 'sk-test-x',
      group: 'friends',
      providers: ['gx10'],
      models: ['gx10/current'],
      limits: { requestsPerDay: 10 },
      admin: true,
    })
    expect(line).toBe(
      '{"name": "full", "apiKey": "sk-test-x", "group": "friends", "providers": ["gx10"], "models": ["gx10/current"], "limits": {"requestsPerDay": 10}, "admin": true}',
    )
  })

  it('omits admin entirely when false (never emits "admin": false, matching every non-admin production entry)', () => {
    const line = userJsonLine({ name: 'plain', apiKey: 'sk-test-x', group: 'home', admin: false })
    expect(line).not.toContain('admin')
  })
})

describe('validatePersonalGrant: mirrors auth.go\'s models-without-providers rejection', () => {
  it('rejects models with no providers', () => {
    expect(validatePersonalGrant([], ['gx10/current'])).not.toBeNull()
  })

  it('accepts models with at least one provider', () => {
    expect(validatePersonalGrant(['gx10'], ['gx10/current'])).toBeNull()
  })

  it('accepts providers with no models (providers-only grant)', () => {
    expect(validatePersonalGrant(['gx10'], [])).toBeNull()
  })

  it('accepts an entirely empty grant', () => {
    expect(validatePersonalGrant([], [])).toBeNull()
  })
})

describe('isValidName', () => {
  it.each([
    ['gx10-embed', true],
    ['friends_2', true],
    ['eng.team', true],
    ['has space', false],
    ['', false],
    ['slash/here', false],
    ['wild*card', false],
  ])('%s -> %s', (name, expected) => {
    expect(isValidName(name)).toBe(expected)
  })
})

describe('isValidNonNegative', () => {
  it.each([
    [0, true],
    [1, true],
    [0.5, true],
    [-1, false],
    [NaN, false],
    [Infinity, false],
  ])('%s -> %s', (n, expected) => {
    expect(isValidNonNegative(n)).toBe(expected)
  })
})

// P2 item 12: LimitsConfig.requestsPerMinute/requestsPerDay/tokensPerDay/
// tokensPerMonth and ModelMetaConfig.ContextTokens are Go int64/int —
// isValidNonNegative alone (fractions allowed) lets a value through that
// json.Unmarshal rejects server-side.
describe('isValidNonNegativeInt', () => {
  it.each([
    [0, true],
    [1, true],
    [1000, true],
    [0.5, false],
    [-1, false],
    [NaN, false],
    [Infinity, false],
  ])('%s -> %s', (n, expected) => {
    expect(isValidNonNegativeInt(n)).toBe(expected)
  })
})

// P3 item 27: auth.go's buildEntry only requires a non-empty user name —
// no configNamePattern character restriction (that pattern exists only
// because provider/mcpServers/agents names are embedded as URL path
// segments; a user name never is).
describe('isValidUserName', () => {
  it.each([
    ['alice', true],
    ['jan@x', true],
    ["O'Brien", true],
    ['has space', true],
    ['', false],
    ['   ', false],
  ])('%s -> %s', (name, expected) => {
    expect(isValidUserName(name)).toBe(expected)
  })
})

describe('generateApiKey / API_KEY_PLACEHOLDER', () => {
  it('the placeholder is visibly not a real key', () => {
    expect(API_KEY_PLACEHOLDER).toBe('sk-llmgw-REPLACE_ME')
  })

  it('generates a "sk-llmgw-" prefixed base64url token from 32 random bytes', () => {
    const spy = vi.spyOn(crypto, 'getRandomValues').mockImplementation(((arr: Uint8Array) => {
      arr.fill(7)
      return arr
    }) as typeof crypto.getRandomValues)
    try {
      const key = generateApiKey()
      expect(key.startsWith('sk-llmgw-')).toBe(true)
      expect(key).not.toContain('+')
      expect(key).not.toContain('/')
      expect(key).not.toContain('=')
      expect(key.length).toBeGreaterThan('sk-llmgw-'.length)
    } finally {
      spy.mockRestore()
    }
  })

  it('two calls produce different keys (real CSPRNG, not mocked)', () => {
    expect(generateApiKey()).not.toBe(generateApiKey())
  })
})
