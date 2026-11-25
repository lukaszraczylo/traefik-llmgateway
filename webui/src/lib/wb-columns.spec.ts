import { describe, expect, it } from 'vitest'

import { wbColumns } from '@/lib/wb-columns'

describe('wbColumns', () => {
  it('number columns are numeric right aligned', () => {
    const [col] = wbColumns([{ key: 'req', header: 'Requests', kind: 'number' }])
    expect(col.numeric).toBe(true)
    expect(col.align).toBe('right')
    expect(col.class).toContain('wb-num')
  })

  it('share columns are not sortable by default', () => {
    const [def, opt] = wbColumns([
      { key: 'share', header: 'Share', kind: 'share' },
      { key: 'share2', header: 'Share', kind: 'share', sortable: true },
    ])
    expect(def.sortable).toBe(false)
    expect(opt.sortable).toBe(true)
  })

  it('limit columns render badge slot', () => {
    const [col] = wbColumns([{ key: 'limit', header: 'Limit', kind: 'limit' }])
    expect(col.kind).toBe('limit')
    expect(col.class).toContain('wb-limit')
    expect(col.align).not.toBe('right')
  })

  it('text, status and time columns are left aligned; text and time sort by default', () => {
    const cols = wbColumns([
      { key: 'a', header: 'A', kind: 'text' },
      { key: 'b', header: 'B', kind: 'status' },
      { key: 'c', header: 'C', kind: 'time', width: '8rem' },
    ])
    expect(cols.map((c) => c.numeric ?? false)).toEqual([false, false, false])
    expect(cols.map((c) => c.sortable)).toEqual([true, false, true])
    expect(cols[2].width).toBe('8rem')
  })

  it('explicit sortable wins over the kind default', () => {
    const [col] = wbColumns([{ key: 'a', header: 'A', kind: 'number', sortable: false }])
    expect(col.sortable).toBe(false)
  })
})
