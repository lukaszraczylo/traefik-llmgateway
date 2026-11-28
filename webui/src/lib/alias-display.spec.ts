import { createPinia, setActivePinia } from 'pinia'
import { describe, expect, it } from 'vitest'

import { aliasTargetMap } from '@/lib/alias-display'
import { useCatalogStore } from '@/stores/catalog'
import type { AdminAliasView, AdminCatalogResponse } from '@/types/api'

const meta = {} as AdminAliasView['modelMeta']

describe('aliasTargetMap', () => {
  it('prefers the resolved canonical target', () => {
    const m = aliasTargetMap([{ alias: 'model-fast', target: 'deepseek-v4.1-flash-1017', resolvedTarget: 'uni/deepseek-v4.1-flash-1017', modelMeta: meta }])
    expect(m.get('model-fast')).toBe('uni/deepseek-v4.1-flash-1017')
  })

  it('falls back to the configured target when it does not resolve', () => {
    const m = aliasTargetMap([{ alias: 'ghost', target: 'no-such-model', modelMeta: meta }])
    expect(m.get('ghost')).toBe('no-such-model')
  })

  it('is empty for no aliases and ignores non-alias ids', () => {
    expect(aliasTargetMap(undefined).size).toBe(0)
    expect(aliasTargetMap([]).get('uni/x')).toBeUndefined()
  })
})

describe('useCatalogStore.aliasTargets', () => {
  it('indexes the fetched catalog aliases', () => {
    setActivePinia(createPinia())
    const catalog = useCatalogStore()
    expect(catalog.aliasTargets.size).toBe(0)
    catalog.data = {
      providers: [],
      aliases: [{ alias: 'model-fast', target: 'x', resolvedTarget: 'uni/x', modelMeta: meta }],
    } as AdminCatalogResponse
    expect(catalog.aliasTargets.get('model-fast')).toBe('uni/x')
    expect(catalog.aliasTargets.get('uni/x')).toBeUndefined()
  })
})
