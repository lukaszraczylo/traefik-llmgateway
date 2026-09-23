import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it } from 'vitest'

import { useNavStore } from '@/stores/nav'

beforeEach(() => {
  setActivePinia(createPinia())
})

describe('useNavStore: initial state', () => {
  it('defaults page to "home" and params to an empty record', () => {
    const nav = useNavStore()
    expect(nav.page).toBe('home')
    expect(nav.params).toEqual({})
  })
})

describe('useNavStore.goTo', () => {
  it('sets page and params together', () => {
    const nav = useNavStore()
    nav.goTo('consumers', { user: 'alice' })
    expect(nav.page).toBe('consumers')
    expect(nav.params).toEqual({ user: 'alice' })
  })

  it('defaults params to an empty record when omitted', () => {
    const nav = useNavStore()
    nav.goTo('models', { model: 'openai/gpt-5' })
    nav.goTo('spend')
    expect(nav.page).toBe('spend')
    expect(nav.params).toEqual({})
  })

  it('replaces, never merges, the previous page\'s params', () => {
    const nav = useNavStore()
    nav.goTo('consumers', { user: 'alice', view: 'directory' })
    nav.goTo('consumers', { view: 'matrix' })
    expect(nav.params).toEqual({ view: 'matrix' })
  })

  it('accepts every page id', () => {
    const nav = useNavStore()
    for (const page of ['home', 'spend', 'consumers', 'models', 'reliability', 'config', 'targets'] as const) {
      nav.goTo(page)
      expect(nav.page).toBe(page)
    }
  })
})
