import { useCatalogStore } from '@/stores/catalog'
import { useConfigStore } from '@/stores/config'
import { useConsumersStore } from '@/stores/consumers'
import { useDashboardStore } from '@/stores/dashboard'
import { useEventsStore } from '@/stores/events'
import { useModelsStore } from '@/stores/models'
import { useReliabilityStore } from '@/stores/reliability'
import { useSpendStore } from '@/stores/spend'

/** resetSessionData drops every store that holds admin API data, so a different key never shows the previous key's numbers. Filters, nav and toasts are UI state and stay. */
export function resetSessionData(): void {
  for (const store of [
    useCatalogStore(),
    useConfigStore(),
    useConsumersStore(),
    useDashboardStore(),
    useEventsStore(),
    useModelsStore(),
    useReliabilityStore(),
    useSpendStore(),
  ]) {
    store.$reset()
  }
}
