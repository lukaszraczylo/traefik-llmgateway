import { describe, expect, it } from 'vitest'

import { useSearchQuery } from './useSearchQuery'

describe('useSearchQuery', () => {
  it('normalized trims leading/trailing whitespace', () => {
    const { query, normalized } = useSearchQuery()
    query.value = '  alice  '
    expect(normalized.value).toBe('alice')
  })

  it('normalized lowercases the query', () => {
    const { query, normalized } = useSearchQuery()
    query.value = 'AlIcE'
    expect(normalized.value).toBe('alice')
  })

  it('hasQuery is false for a whitespace-only query', () => {
    const { query, hasQuery } = useSearchQuery()
    query.value = '   '
    expect(hasQuery.value).toBe(false)
  })
})
