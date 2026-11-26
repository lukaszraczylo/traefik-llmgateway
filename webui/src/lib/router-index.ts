import { splitModelId } from '@/lib/model-id'
import type { AdminRouterView } from '@/types/api'

/** Router names per model id. A tier target is indexed as written and, when canonical, by its bare model id too, because configs list both forms. */
export function routersByModel(routers: AdminRouterView[] | undefined): Map<string, string[]> {
  const index = new Map<string, Set<string>>()
  const add = (key: string, router: string): void => {
    let set = index.get(key)
    if (!set) index.set(key, (set = new Set()))
    set.add(router)
  }
  for (const r of routers ?? []) {
    for (const tier of r.tiers ?? []) {
      for (const target of tier.models) {
        add(target, r.name)
        const { provider, model } = splitModelId(target)
        if (provider) add(model, r.name)
      }
    }
  }
  return new Map([...index].map(([k, v]) => [k, [...v].sort()]))
}
