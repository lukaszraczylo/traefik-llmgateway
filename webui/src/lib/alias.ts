import type { AdminAliasView } from '@/types/api'

/** aliasNameSet indexes configured alias names (exact match, as a client sends them) for O(1) lookups. */
export function aliasNameSet(aliases: readonly AdminAliasView[] | undefined): Set<string> {
  return new Set((aliases ?? []).map((a) => a.alias))
}
