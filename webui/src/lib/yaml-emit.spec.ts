import { describe, expect, it } from 'vitest'

import {
  emitConfigSnippet,
  emitYamlDocument,
  emitYamlFlowList,
  emitYamlFlowMap,
  emitYamlKey,
  emitYamlScalar,
  toYamlValue,
  yamlKeyNeedsQuoting,
} from './yaml-emit'

describe('yamlKeyNeedsQuoting / emitYamlKey', () => {
  it.each([
    ['gx10/current', true],
    ['a:b', true],
    ['plain', false],
    ['eng-team', false],
    ['friends_2', false],
  ])('key %s needs quoting: %s', (key, expected) => {
    expect(yamlKeyNeedsQuoting(key)).toBe(expected)
  })

  it('quotes a key containing "/" — modelMeta\'s own routable-id convention', () => {
    expect(emitYamlKey('gx10/current')).toBe('"gx10/current"')
  })

  it('leaves a plain key unquoted', () => {
    expect(emitYamlKey('friends')).toBe('friends')
  })
})

/**
 * P2 item 11 — a table of tricky scalars a naive quoting rule gets wrong,
 * checked against BOTH emitYamlKey and emitYamlScalar (one shared rule,
 * needsQuoting — the fix instruction's own "apply the same rule to
 * keys"). Every "needs quoting: true" row here was independently
 * sanity-checked with PyYAML's own safe_load round trip during the
 * verify pass this fixes (no network access needed to reason about it:
 * PyYAML, like every YAML 1.1-lineage parser, resolves a flow-indicator
 * character anywhere in a plain scalar, a leading special/digit
 * character, a "key: value"-shaped substring, or a case-insensitive
 * boolean/null spelling, the same way regardless of position — this
 * table exercises each category at least once).
 */
describe('needsQuoting (P2 item 11): tricky inputs table', () => {
  it.each([
    // Flow indicators ANYWHERE, not just leading (auth.go:492 path.Match
    // globs are legitimate config — `gpt-[45]*` used to break a flow
    // list because only a LEADING `[`/`]` was quoted).
    ['gpt-[45]*', 'glob bracket mid-string', true],
    ['a]b', 'closing bracket mid-string', true],
    ['a}b', 'closing brace mid-string', true],
    ['a,b', 'comma mid-string', true],
    ['*m', 'leading flow/alias indicator', true],
    ['{m}', 'leading brace', true],
    ['- x', 'leading "- " (block sequence indicator)', true],
    // Leading/trailing space, and a comment start that used to be
    // truncated mid-scalar rather than quoted away entirely.
    [' leading-space', 'leading space', true],
    ['trailing-space ', 'trailing space', true],
    ['x #y', 'space then "#" (comment start)', true],
    // "key: value"-shaped substrings.
    ['a: b', 'colon-space (nested-mapping shape)', true],
    ['a:', 'trailing colon', true],
    ['12:30', 'time-of-day shaped', true],
    // YAML 1.1 type-changers: numbers, dates, booleans, null, "infinity".
    ['0x10', 'hex-looking number', true],
    ['1e3', 'exponent-looking number', true],
    ['2026-01-01', 'date-shaped', true],
    ['.inf', 'YAML 1.1 infinity literal', true],
    ['~', 'YAML null shorthand', true],
    ['on', 'YAML 1.1 boolean (lowercase)', true],
    ['Off', 'YAML 1.1 boolean (mixed case)', true],
    ['yes', 'YAML 1.1 boolean', true],
    ['NO', 'YAML 1.1 boolean (uppercase)', true],
    ['true', 'YAML 1.2 boolean', true],
    ['null', 'YAML null literal', true],
    ['y', 'YAML 1.1 single-letter boolean', true],
    // Safe: this module's real values (group/provider names, canonical
    // model ids) must stay unquoted for readability.
    ['plain', 'ordinary bare word', false],
    ['gx10-embed', 'hyphenated name', false],
    ['friends_2', 'underscored name', false],
    ['eng.team', 'dotted name', false],
  ])('%s (%s) needs quoting: %s', (input, _label, expected) => {
    expect(yamlKeyNeedsQuoting(input)).toBe(expected)
    const scalarRendered = emitYamlScalar(input)
    expect(scalarRendered === `"${input}"` || scalarRendered === input).toBe(true)
    expect(scalarRendered.startsWith('"')).toBe(expected)
  })

  it('a group named "on"/"yes"/"true"/"null" (all allowed by GrantForm/LimitsForm\'s NAME_PATTERN) renders as a quoted, still-a-string YAML key', () => {
    for (const name of ['on', 'yes', 'true', 'null']) {
      expect(emitYamlKey(name)).toBe(JSON.stringify(name))
    }
  })
})

describe('emitYamlScalar', () => {
  it.each([
    [null, 'null'],
    [true, 'true'],
    [false, 'false'],
    [0, '0'],
    [5, '5'],
    [0.5, '0.5'],
    ['anthropic', 'anthropic'],
  ])('renders %p as %s', (value, expected) => {
    expect(emitYamlScalar(value as never)).toBe(expected)
  })

  it('quotes an empty string', () => {
    expect(emitYamlScalar('')).toBe('""')
  })

  it('quotes a string that would otherwise parse as a boolean literal', () => {
    expect(emitYamlScalar('true')).toBe('"true"')
  })

  it('quotes a string that would otherwise parse as a number', () => {
    expect(emitYamlScalar('123')).toBe('"123"')
  })

  it('quotes a string containing ": " (would misparse as a nested mapping)', () => {
    expect(emitYamlScalar('a: b')).toBe('"a: b"')
  })

  // N6 (verify-redesign-final.md): JSON.stringify leaves these four code
  // points as literal bytes/code units — valid JSON, but a YAML 1.1
  // scanner (PyYAML, go-yaml v3) treats U+0085/U+2028/U+2029 as line
  // breaks (folded into whitespace, or a scanner error on a key) and
  // rejects U+007F outright as non-printable. Each must come back
  // escaped, never as a raw character.
  it.each([
    ['NEL (U+0085)', '\u0085', '\\x85'],
    ['a C1 control (U+009F)', '\u009f', '\\x9f'],
    ['LINE SEPARATOR (U+2028)', '\u2028', '\\u2028'],
    ['PARAGRAPH SEPARATOR (U+2029)', '\u2029', '\\u2029'],
    ['DEL (U+007F)', '\u007f', '\\x7f'],
  ])('escapes %s inside a quoted scalar', (_label, char, escape) => {
    const rendered = emitYamlScalar(`a${char}b`)
    expect(rendered).toBe(`"a${escape}b"`)
    expect(rendered).not.toContain(char)
  })

  it('escapes the same control characters inside a quoted key', () => {
    const rendered = emitYamlKey('a\u0085b')
    expect(rendered).toBe('"a\\x85b"')
    expect(rendered).not.toContain('\u0085')
  })

  it('round-trips a string that is ONLY the unsafe character, still quoted and escaped', () => {
    expect(emitYamlScalar('\u2028')).toBe('"\\u2028"')
  })
})

describe('emitYamlFlowList', () => {
  it('renders an empty list as "[]" — the production file\'s own "allow everything" convention', () => {
    expect(emitYamlFlowList([])).toBe('[]')
  })

  it('renders a short string list inline, matching middleware.yaml\'s alibaba/xiaomi provider entries', () => {
    expect(emitYamlFlowList(['qwen3.8-max', 'qwen3.8-flash'])).toBe('[qwen3.8-max, qwen3.8-flash]')
  })

  it('renders numbers unquoted inside a flow list', () => {
    expect(emitYamlFlowList([1, 2, 3])).toBe('[1, 2, 3]')
  })
})

describe('emitYamlFlowMap', () => {
  it('renders an empty map as "{}"', () => {
    expect(emitYamlFlowMap([])).toBe('{}')
  })

  // Golden strings: middleware.yaml's own modelMeta entries, verbatim.
  it('matches "{ free: true }" (middleware.yaml modelMeta, e.g. gx10/current)', () => {
    expect(emitYamlFlowMap([['free', true]])).toBe('{ free: true }')
  })

  it('matches "{ contextTokens: 16384, free: true }" (middleware.yaml, gx10-embed model)', () => {
    expect(
      emitYamlFlowMap([
        ['contextTokens', 16384],
        ['free', true],
      ]),
    ).toBe('{ contextTokens: 16384, free: true }')
  })
})

describe('emitYamlDocument', () => {
  it('renders nested maps at 2-space-per-level indent, matching middleware.yaml exactly', () => {
    const doc = emitYamlDocument({
      spec: {
        plugin: {
          llmgateway: {
            admin: { enabled: true },
          },
        },
      },
    })
    expect(doc).toBe(['spec:', '  plugin:', '    llmgateway:', '      admin:', '        enabled: true'].join('\n'))
  })

  it('drops undefined keys entirely (omitempty parity), never emitting them as null', () => {
    const doc = emitYamlDocument({ a: 1, b: undefined, c: 'x' })
    expect(doc).toBe('a: 1\nc: x')
  })

  it('renders an explicitly-empty nested map as "key: {}", not omitted', () => {
    const doc = emitYamlDocument({ cache: {} })
    expect(doc).toBe('cache: {}')
  })

  it('renders an empty string-array field as "[]" inline, matching middleware.yaml\'s groups.home block (every field explicit and empty, not a bare "{}")', () => {
    const doc = emitYamlDocument({
      home: { providers: [], models: [], mcpServers: [], agents: [] },
    })
    expect(doc).toBe(['home:', '  providers: []', '  models: []', '  mcpServers: []', '  agents: []'].join('\n'))
  })

  // Defensive-only path: this schema never actually produces an array of
  // objects, but a generic render of an unforeseen server shape must not
  // throw.
  it('falls back to block "-" items with JSON.stringify per element for an array containing a non-primitive (defensive, never hit by this schema)', () => {
    const doc = emitYamlDocument({ weird: [{ a: 1 }] })
    expect(doc).toBe('weird:\n  - {"a":1}')
  })
})

describe('emitConfigSnippet: matches middleware.yaml\'s own indent depth exactly', () => {
  it('wraps a group\'s access-list body under spec.plugin.llmgateway.groups.<name> (middleware.yaml groups.home shape)', () => {
    const snippet = emitConfigSnippet(['groups', 'home'], {
      providers: [],
      models: [],
      mcpServers: [],
      agents: [],
    })
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

  it('wraps a group\'s limits at one further nesting level', () => {
    const snippet = emitConfigSnippet(['groups', 'friends'], { limits: { requestsPerDay: 1000 } })
    expect(snippet).toBe(
      ['spec:', '  plugin:', '    llmgateway:', '      groups:', '        friends:', '          limits:', '            requestsPerDay: 1000'].join(
        '\n',
      ),
    )
  })

  it('wraps a top-level, non-nested section (e.g. admin) with no extra name level', () => {
    const snippet = emitConfigSnippet(['admin'], { enabled: true })
    expect(snippet).toBe(['spec:', '  plugin:', '    llmgateway:', '      admin:', '        enabled: true'].join('\n'))
  })
})

describe('toYamlValue', () => {
  it('passes primitives through unchanged', () => {
    expect(toYamlValue('x')).toBe('x')
    expect(toYamlValue(5)).toBe(5)
    expect(toYamlValue(true)).toBe(true)
    expect(toYamlValue(null)).toBe(null)
  })

  it('recurses through nested arrays and objects', () => {
    expect(toYamlValue({ a: [1, { b: 'c' }] })).toEqual({ a: [1, { b: 'c' }] })
  })

  it('never throws on an unexpected type, falling back to String()', () => {
    const weird = (() => 1) as unknown
    expect(toYamlValue(weird)).toBe(String(weird))
  })
})
