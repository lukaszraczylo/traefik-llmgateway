import { describe, expect, it } from 'vitest'

import { aliasNameSet } from './alias'
import type { AdminAliasView } from '@/types/api'

const view = (alias: string): AdminAliasView => ({ alias, target: 'openai/gpt-5', modelMeta: {} })

describe('aliasNameSet', () => {
  it.each<{ name: string; input: AdminAliasView[] | undefined; has: string[]; hasNot: string[] }>([
    { name: 'undefined', input: undefined, has: [], hasNot: ['x'] },
    { name: 'empty', input: [], has: [], hasNot: ['x'] },
    { name: 'two aliases', input: [view('fast'), view('smart')], has: ['fast', 'smart'], hasNot: ['openai/gpt-5', 'Fast', ''] },
  ])('$name', ({ input, has, hasNot }) => {
    const set = aliasNameSet(input)
    for (const h of has) expect(set.has(h)).toBe(true)
    for (const h of hasNot.filter((x) => !has.includes(x))) expect(set.has(h)).toBe(false)
  })
})
