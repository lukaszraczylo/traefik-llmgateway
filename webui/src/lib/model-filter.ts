import type { AdminUsageModelEntry } from '@/types/api'

/**
 * filterModelsByPrefix narrows the Models tab's already-fetched ranking to
 * entries whose canonical "provider/model" id starts with `prefix` (F6,
 * dashboard-plan.md) — the client-side filter ChartsView.vue applies on
 * top of stores/history.ts's own `modelFilter` state, set by
 * ProvidersView.vue's provider-header link (nav.goToModels, stores/nav.ts
 * — WP-B1). An empty prefix is "no filter", returning `models` unchanged
 * — the default state before any provider link has been clicked.
 */
export function filterModelsByPrefix(models: AdminUsageModelEntry[], prefix: string): AdminUsageModelEntry[] {
  if (!prefix) return models
  return models.filter((m) => m.id.startsWith(prefix))
}
