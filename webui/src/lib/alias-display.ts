import type { AdminAliasView } from '@/types/api'

/** Maps each alias id to the target a reader should see: the canonical `provider/model` when the gateway resolved it, else the configured target. */
export function aliasTargetMap(aliases: readonly AdminAliasView[] | undefined): Map<string, string> {
  const map = new Map<string, string>()
  for (const a of aliases ?? []) map.set(a.alias, a.resolvedTarget || a.target)
  return map
}
