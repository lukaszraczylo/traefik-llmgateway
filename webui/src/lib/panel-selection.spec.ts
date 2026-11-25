import { describe, expect, it } from 'vitest'

import { buildHash, parseHashState, parsePageParams } from './hash-state'
import { formatSel, parseSel, resolveSel } from './panel-selection'

describe('parseSel', () => {
  it('parses kind:id', () => {
    expect(parseSel('user:alice')).toEqual({ kind: 'user', id: 'alice' })
  })

  it('rejects unknown kind', () => {
    expect(parseSel('galaxy:alice')).toBeNull()
  })

  it('rejects absent, empty and id-less values', () => {
    expect(parseSel(undefined)).toBeNull()
    expect(parseSel('')).toBeNull()
    expect(parseSel('user')).toBeNull()
    expect(parseSel('user:')).toBeNull()
  })

  it('splits on the first colon only', () => {
    expect(parseSel('target:mcp:fetch')).toEqual({ kind: 'target', id: 'mcp:fetch' })
  })

  it('rejects a malformed percent escape instead of throwing', () => {
    expect(parseSel('user:%E0%A4%A')).toBeNull()
  })
})

describe('formatSel', () => {
  it('encodes ids with slashes (uni/glm-5.3-flash)', () => {
    const s = formatSel('model', 'uni/glm-5.3-flash')
    expect(s).toBe('model:uni%2Fglm-5.3-flash')
    expect(parseSel(s)).toEqual({ kind: 'model', id: 'uni/glm-5.3-flash' })
  })

  it('round-trips ids containing colons, percent signs and spaces', () => {
    for (const id of ['mcp:fetch', '100%', 'a b', 'x&y=z#w']) {
      expect(parseSel(formatSel('target', id))).toEqual({ kind: 'target', id })
    }
  })

  it('survives the hash round trip through buildHash and parseHashState', () => {
    const id = 'uni/glm-5.3-flash'
    const hash = buildHash('models', { sel: formatSel('model', id) })
    const { page, query } = parseHashState(hash)
    expect(page).toBe('models')
    expect(parseSel(parsePageParams(query).sel)).toEqual({ kind: 'model', id })
  })
})

describe('resolveSel', () => {
  const known = new Set(['user:alice'])
  const exists = (kind: string, id: string) => known.has(`${kind}:${id}`)

  it('keeps a known selection', () => {
    expect(resolveSel({ kind: 'user', id: 'alice' }, exists)).toEqual({ kind: 'user', id: 'alice' })
  })

  it('unknown id clears sel', () => {
    expect(resolveSel({ kind: 'user', id: 'ghost' }, exists)).toBeNull()
  })

  it('passes null through', () => {
    expect(resolveSel(null, exists)).toBeNull()
  })
})
