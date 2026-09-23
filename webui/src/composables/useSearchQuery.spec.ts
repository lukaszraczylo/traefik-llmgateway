import { ref } from 'vue'
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

  // F9 (hash-state): a caller can supply the ref this composable reads and
  // writes instead of a private local one, so the query round-trips
  // through an external owner (nav.usageQuery, UsageView.vue) rather than
  // being lost on remount.
  describe('with an external ref', () => {
    it('reads and derives from the external ref\'s current value, rather than starting blank', () => {
      const external = ref('preset')
      const { query, normalized, hasQuery } = useSearchQuery(external)

      expect(query.value).toBe('preset')
      expect(normalized.value).toBe('preset')
      expect(hasQuery.value).toBe(true)
    })

    it('writes THROUGH to the external ref — the same ref instance, not a copy', () => {
      const external = ref('')
      const { query } = useSearchQuery(external)

      query.value = 'typed'

      expect(external.value).toBe('typed')
    })

    it('reflects a change made directly to the external ref (bidirectional, not a one-time snapshot)', () => {
      const external = ref('')
      const { query, normalized } = useSearchQuery(external)

      external.value = 'set-elsewhere'

      expect(query.value).toBe('set-elsewhere')
      expect(normalized.value).toBe('set-elsewhere')
    })

    it('without an external ref, two calls own independent state (no accidental sharing)', () => {
      const a = useSearchQuery()
      const b = useSearchQuery()

      a.query.value = 'alice'

      expect(b.query.value).toBe('')
    })
  })
})
