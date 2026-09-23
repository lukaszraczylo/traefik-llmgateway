// nav.spec.ts covers useNavStore's two navigation actions (F6). Real
// Pinia + real useHistoryStore, no mocking needed: useAuthStore.
// isAuthenticated defaults to false in this 'node' test environment (no
// sessionStorage — key-storage.ts's own try/catch guard), so history.ts's
// setScope/setTab/setModelFilter run their synchronous state updates and
// their own fire-and-forget refresh()/fetchSeries() short-circuits
// immediately at its own `if (!auth.isAuthenticated) return` guard,
// exactly like dashboard.spec.ts's own precedent (see that file's header
// comment).
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it } from 'vitest'

import { useHistoryStore } from '@/stores/history'
import { useNavStore } from '@/stores/nav'

beforeEach(() => {
  setActivePinia(createPinia())
})

describe('useNavStore: initial state', () => {
  it('defaults activeTab to "providers" and usageQuery to empty', () => {
    const nav = useNavStore()
    expect(nav.activeTab).toBe('providers')
    expect(nav.usageQuery).toBe('')
  })
})

describe('useNavStore.goToCharts', () => {
  it('sets the history scope and switches activeTab to "charts"', () => {
    const nav = useNavStore()
    const history = useHistoryStore()
    nav.goToCharts('user:alice')
    expect(history.scope).toBe('user:alice')
    expect(nav.activeTab).toBe('charts')
  })

  it('accepts every scope prefix history.ts recognizes', () => {
    const nav = useNavStore()
    const history = useHistoryStore()
    for (const scope of ['total', 'user:alice', 'group:eng', 'model:openai/gpt-4']) {
      nav.goToCharts(scope)
      expect(history.scope).toBe(scope)
    }
  })

  it('switches the Charts tab back to "requests" first when it was showing "models" (no scope picker there)', () => {
    const nav = useNavStore()
    const history = useHistoryStore()
    history.setTab('models')
    nav.goToCharts('group:eng')
    expect(history.tab).toBe('requests')
    expect(history.scope).toBe('group:eng')
  })

  it('leaves a non-models tab untouched', () => {
    const nav = useNavStore()
    const history = useHistoryStore()
    history.setTab('cost')
    nav.goToCharts('total')
    expect(history.tab).toBe('cost')
  })
})

describe('useNavStore.goToModels', () => {
  it('switches to the Models tab, applies the prefix filter, and switches activeTab to "charts"', () => {
    const nav = useNavStore()
    const history = useHistoryStore()
    nav.goToModels('openai/')
    expect(history.tab).toBe('models')
    expect(history.modelFilter).toBe('openai/')
    expect(nav.activeTab).toBe('charts')
  })

  it('applies the filter AFTER switching tabs, so setTab\'s own modelFilter reset never wins the race', () => {
    const nav = useNavStore()
    const history = useHistoryStore()
    // Already on the Models tab with a stale filter from an earlier visit —
    // setTab('models') is a no-op (tab === this.tab guard, history.ts) and
    // so does NOT reset modelFilter on its own; goToModels's own explicit
    // setModelFilter call is what must still land the new prefix.
    history.setTab('models')
    history.setModelFilter('stale/')
    nav.goToModels('anthropic/')
    expect(history.modelFilter).toBe('anthropic/')
  })
})
