import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it } from 'vitest'

import { useFiltersStore } from '@/stores/filters'

beforeEach(() => {
  setActivePinia(createPinia())
})

describe('useFiltersStore: defaults', () => {
  it('defaults to range=7d, cmp=none, scope=all', () => {
    const filters = useFiltersStore()
    expect(filters.range).toBe('7d')
    expect(filters.cmp).toBe('none')
    expect(filters.scope).toBe('all')
  })

  it('derives window/span from the default range', () => {
    const filters = useFiltersStore()
    expect(filters.window).toBe('day')
    expect(filters.span).toBe(7)
  })
})

describe('useFiltersStore.setFilters', () => {
  it('applies only the provided fields, leaving the rest untouched', () => {
    const filters = useFiltersStore()
    filters.setFilters({ range: '30d' })
    expect(filters.range).toBe('30d')
    expect(filters.cmp).toBe('none')
    expect(filters.scope).toBe('all')
  })

  it('updates window/span when range changes', () => {
    const filters = useFiltersStore()
    filters.setFilters({ range: '12mo' })
    expect(filters.window).toBe('month')
    expect(filters.span).toBe(12)
  })

  it('applies range/cmp/scope together in one call', () => {
    const filters = useFiltersStore()
    filters.setFilters({ range: '14d', cmp: 'prev', scope: 'group:eng' })
    expect(filters.range).toBe('14d')
    expect(filters.cmp).toBe('prev')
    expect(filters.scope).toBe('group:eng')
  })
})

describe('useFiltersStore.cmpSpec', () => {
  it('requested is false when cmp is none, even for a comparable range', () => {
    const filters = useFiltersStore()
    expect(filters.cmpSpec.requested).toBe(false)
  })

  it('requested is true when cmp is prev and the range supports it', () => {
    const filters = useFiltersStore()
    filters.setFilters({ cmp: 'prev' })
    expect(filters.cmpSpec.available).toBe(true)
    expect(filters.cmpSpec.requested).toBe(true)
    expect(filters.cmpSpec.offset).toBe(7)
  })

  it('requested stays false when cmp is prev but the range does not support comparison', () => {
    const filters = useFiltersStore()
    filters.setFilters({ cmp: 'prev', range: '48h' })
    expect(filters.cmpSpec.available).toBe(false)
    expect(filters.cmpSpec.requested).toBe(false)
    expect(filters.cmpSpec.reason).toBe('retention limit')
  })
})
