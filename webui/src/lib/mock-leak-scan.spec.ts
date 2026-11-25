import { describe, expect, it } from 'vitest'

import { leakTerms, type LeakTerm } from '../../mock/leak-terms'

const fixtures = import.meta.glob('../../mock/fixtures/*.json', {
  query: '?raw',
  import: 'default',
  eager: true,
}) as Record<string, string>

const env = (globalThis as { process?: { env: Record<string, string | undefined> } }).process?.env ?? {}

function matches(text: string, term: LeakTerm): boolean {
  return typeof term === 'string' ? text.toLowerCase().includes(term.toLowerCase()) : term.test(text)
}

describe('mock fixtures', () => {
  it('has fixtures to scan', () => {
    expect(Object.keys(fixtures).length).toBeGreaterThan(0)
  })

  it('committed fixtures contain no private identifiers', () => {
    const hits: string[] = []
    for (const [file, text] of Object.entries(fixtures)) {
      for (const term of leakTerms(env.LEAK_SCAN_EXTRA_TERMS)) {
        if (matches(text, term)) hits.push(`${file}: ${String(term)}`)
      }
    }
    expect(hits).toEqual([])
  })

  it('detects a planted leak', () => {
    const planted = leakTerms('zz-extra-name').filter((t) => matches('{"u":"zz-extra-name","h":"10.1.2.3","e":"a@b.co"}', t))
    expect(planted.length).toBe(3)
  })
})
