import { describe, expect, it } from 'vitest'

import { routersByModel } from '@/lib/router-index'
import type { AdminRouterView } from '@/types/api'

function router(name: string, ...tiers: string[][]): AdminRouterView {
  return { name, tiers: tiers.map((models, i) => ({ name: `t${i}`, models, minScore: i })) } as unknown as AdminRouterView
}

describe('routersByModel', () => {
  it('bare and canonical ids both indexed', () => {
    const idx = routersByModel([router('smartrouter/gx10', ['gx10/Qwen3.8-27B'], ['Inkling-2-Mini'])])
    expect(idx.get('gx10/Qwen3.8-27B')).toEqual(['smartrouter/gx10'])
    expect(idx.get('Qwen3.8-27B')).toEqual(['smartrouter/gx10'])
    expect(idx.get('Inkling-2-Mini')).toEqual(['smartrouter/gx10'])
  })

  it('lists each router once per model, sorted by name', () => {
    const idx = routersByModel([router('b', ['p/m'], ['p/m']), router('a', ['p/m'])])
    expect(idx.get('p/m')).toEqual(['a', 'b'])
  })

  it('keeps later slashes in the bare id', () => {
    const idx = routersByModel([router('r', ['openrouter/meta/llama'])])
    expect(idx.get('meta/llama')).toEqual(['r'])
  })

  it('is empty for no routers or routers without tiers', () => {
    expect(routersByModel([]).size).toBe(0)
    expect(routersByModel(undefined).size).toBe(0)
    expect(routersByModel([{ name: 'x' } as unknown as AdminRouterView]).size).toBe(0)
  })
})
