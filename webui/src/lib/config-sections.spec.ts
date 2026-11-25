import { describe, expect, it } from 'vitest'

import { filterSections, splitConfigSections } from '@/lib/config-sections'

const YAML = ['admin:', '  enabled: true', 'agents:', '  a:', '    url: x', '  b:', '    url: y', '"odd/key":', '  v: 1', 'redis:', '  addr: r'].join('\n')

describe('splitConfigSections', () => {
  it('splits at each top-level key with 1-based start lines and line counts', () => {
    const s = splitConfigSections(YAML)
    expect(s.map((x) => [x.key, x.startLine, x.lineCount])).toEqual([
      ['admin', 1, 2],
      ['agents', 3, 5],
      ['odd/key', 8, 2],
      ['redis', 10, 2],
    ])
  })

  it('keeps each section text verbatim so the sections join back to the document', () => {
    expect(splitConfigSections(YAML).map((x) => x.text).join('\n')).toBe(YAML)
  })

  it('returns no sections for empty text', () => {
    expect(splitConfigSections('')).toEqual([])
  })

  it('does not treat list items or comments at column 0 as keys', () => {
    const s = splitConfigSections(['a:', '- x: 1', '# note', 'b: 2'].join('\n'))
    expect(s.map((x) => x.key)).toEqual(['a', 'b'])
    expect(s[0].lineCount).toBe(3)
  })
})

describe('filterSections', () => {
  const s = splitConfigSections(YAML)
  it('returns all sections for a blank query', () => {
    expect(filterSections(s, '  ')).toHaveLength(4)
  })
  it('matches keys case-insensitively by substring', () => {
    expect(filterSections(s, 'AG').map((x) => x.key)).toEqual(['agents'])
  })
  it('returns none when nothing matches', () => {
    expect(filterSections(s, 'zzz')).toEqual([])
  })
})
