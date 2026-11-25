import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it } from 'vitest'

import { PAGE_IDS } from '@/lib/pages'
import { useNavStore } from '@/stores/nav'

beforeEach(() => {
  setActivePinia(createPinia())
})

describe('useNavStore: initial state', () => {
  it('defaults page to "home" and params to an empty record', () => {
    const nav = useNavStore()
    expect(nav.page).toBe('overview')
    expect(nav.params).toEqual({})
  })
})

describe('useNavStore.goTo', () => {
  it('sets page and params together', () => {
    const nav = useNavStore()
    nav.goTo('users', { user: 'alice' })
    expect(nav.page).toBe('users')
    expect(nav.params).toEqual({ user: 'alice' })
  })

  it('defaults params to an empty record when omitted', () => {
    const nav = useNavStore()
    nav.goTo('models', { model: 'openai/gpt-5' })
    nav.goTo('usage')
    expect(nav.page).toBe('usage')
    expect(nav.params).toEqual({})
  })

  it('replaces, never merges, the previous page\'s params', () => {
    const nav = useNavStore()
    nav.goTo('users', { user: 'alice', view: 'directory' })
    nav.goTo('users', { view: 'matrix' })
    expect(nav.params).toEqual({ view: 'matrix' })
  })

  it('accepts every page id', () => {
    const nav = useNavStore()
    for (const page of PAGE_IDS) {
      nav.goTo(page)
      expect(nav.page).toBe(page)
    }
  })
})

describe('useNavStore panels', () => {
  it('openPanel sets sel, keeps other params and requests a history push', () => {
    const nav = useNavStore()
    nav.goTo('models', { q: 'gpt' })
    nav.openPanel('model', 'uni/glm-5.3-flash')
    expect(nav.params).toEqual({ q: 'gpt', sel: 'model:uni%2Fglm-5.3-flash' })
    expect(nav.pushPending).toBe(true)
    expect(nav.selection).toEqual({ kind: 'model', id: 'uni/glm-5.3-flash' })
  })

  it('openPanel on the already-open panel does not request a second push', () => {
    const nav = useNavStore()
    nav.openPanel('user', 'alice')
    nav.pushPending = false
    nav.openPanel('user', 'alice')
    expect(nav.pushPending).toBe(false)
  })

  it('closePanel without a pushed entry clears sel in place and requests no push', () => {
    const nav = useNavStore()
    nav.goTo('users', { q: 'a', sel: 'user:alice' })
    nav.closePanel()
    expect(nav.params).toEqual({ q: 'a' })
    expect(nav.pushPending).toBe(false)
    expect(nav.selection).toBeNull()
  })

  it('goTo with replace:false requests a push; default does not', () => {
    const nav = useNavStore()
    nav.goTo('users')
    expect(nav.pushPending).toBe(false)
    nav.goTo('models', {}, { replace: false })
    expect(nav.pushPending).toBe(true)
  })

  it('goTo resets the pushed-panel marker so a later close replaces', () => {
    const nav = useNavStore()
    nav.openPanel('user', 'alice')
    expect(nav.panelPushed).toBe(true)
    nav.goTo('users', { sel: 'user:alice' })
    expect(nav.panelPushed).toBe(false)
  })
})
