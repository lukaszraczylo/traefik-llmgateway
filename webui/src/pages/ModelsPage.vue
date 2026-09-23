<script setup lang="ts">
import { computed, onMounted, watch } from 'vue'

import ModelCatalogTable from '@/components/ModelCatalogTable.vue'
import ProviderHealthPanel from '@/components/ProviderHealthPanel.vue'
import { useFiltersStore } from '@/stores/filters'
import { useModelsStore } from '@/stores/models'
import { useNavStore } from '@/stores/nav'

/**
 * ModelsPage (redesign-plan.md section 3.4, "Models & providers") — the
 * catalog table (ModelCatalogTable.vue), the Providers accordion moved
 * from the pre-redesign ProvidersView.vue (ProviderHealthPanel.vue, now
 * also carrying fleet-wide performance), and the model-alias table
 * (bundled into ProviderHealthPanel.vue — see that component's own doc
 * comment for why it stayed with the accordion it was already part of).
 *
 * Page params (redesign-plan.md section 3.1): `q` (free-text search,
 * ModelCatalogTable.vue's own SearchInput) and `model` (a canonical
 * "provider/model" id — EntityLink.vue's own deep-link target for a model
 * row elsewhere in the panel). Both are folded into the SAME `query`
 * computed getter/setter below (ConsumerDirectory.vue's own `usageQuery`
 * convention) — bound directly to nav.params rather than a local ref, so a
 * `q`/`model` change from anywhere else (a ProviderHealthPanel "view
 * models" link, browser back/forward, a hand-edited hash) is picked up
 * reactively instead of only once on mount.
 */
const filters = useFiltersStore()
const models = useModelsStore()
const nav = useNavStore()

const query = computed<string>({
  get: () => nav.params.q ?? nav.params.model ?? '',
  set: (value) => {
    // `query` is the single source of truth from here on — a stale
    // `model` param from a deep link is dropped (not merged forward),
    // since `q` now carries the exact same information. An empty
    // `value` is fine to pass straight through: buildHash
    // (lib/hash-state.ts) already omits an empty-string param from the
    // URL.
    const { model: _model, ...rest } = nav.params
    nav.goTo('models', { ...rest, q: value })
  },
})

onMounted(() => void models.fetch())

watch(
  () => filters.range,
  () => void models.fetch(),
)
</script>

<template>
  <div class="flex flex-col gap-6">
    <ProviderHealthPanel />
    <!-- verify-ui-states-2.md #8: shown only for a BACKGROUND failure
    (rows already on screen) — a first-load failure already renders via
    ModelCatalogTable's own ErrorState below, and showing both duplicated
    the same message twice. -->
    <p v-if="models.error && models.rows.length > 0" class="text-sm break-words text-destructive">{{ models.error }}</p>
    <ModelCatalogTable
      :rows="models.rows"
      :query="query"
      :latency-enabled="models.latencyEnabled"
      :loading="models.loading"
      :error="models.error"
      :on-retry="models.fetch"
      @update:query="query = $event"
    />
  </div>
</template>
