import { computed } from 'vue'

import { aliasNameSet } from '@/lib/alias'
import { useDashboardStore } from '@/stores/dashboard'

/** useAliasNames exposes isAlias(id): true when id is a configured model alias (from the polled overview). */
export function useAliasNames() {
  const dashboard = useDashboardStore()
  const names = computed(() => aliasNameSet(dashboard.overview?.aliases))
  return { isAlias: (id: string): boolean => names.value.has(id) }
}
