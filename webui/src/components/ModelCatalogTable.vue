<script setup lang="ts">
import { computed } from 'vue'

import DataTable from '@/components/DataTable.vue'
import EmptyState from '@/components/EmptyState.vue'
import ErrorState from '@/components/ErrorState.vue'
import SearchInput from '@/components/SearchInput.vue'
import SkeletonTable from '@/components/SkeletonTable.vue'
import StatDisabledAlert from '@/components/StatDisabledAlert.vue'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { filterModelRows } from '@/lib/model-filter'
import { loadState } from '@/lib/load-state'
import { modelCatalogColumns } from '@/lib/model-table-columns'
import type { ModelCatalogRow } from '@/lib/model-table-columns'

/**
 * ModelCatalogTable (redesign-plan.md section 3.4, Models & providers
 * page) is the catalog-driven table: every configured model (not just
 * ones with observed traffic), joined with the current range's ranking
 * and fleet performance — ModelCatalogRow's own doc comment
 * (lib/model-table-columns.ts) has the full join contract.
 *
 * `query` is a controlled v-model, not owned locally: ModelsPage.vue keeps
 * it so the Models page's own `q` hash param (redesign-plan.md section
 * 3.1) round-trips through the URL, the same controlled-component
 * convention SearchInput.vue itself already documents.
 */
const props = defineProps<{
  rows: ModelCatalogRow[]
  query: string
  /** GET /admin/api/performance's own latencyEnabled (AdminPerfResponse) — false means admin.stats.latency is off, so p50/p95 read "—" for every row, not "no traffic yet". */
  latencyEnabled: boolean
  loading: boolean
  /** models.error (verify-ui-states.md #3 fix) — ModelsPage.vue's own current-fetch error, '' when there is none. Previously this component always passed error: '' into loadState below, so a fetch that failed with no rows yet rendered "No models configured." (a false fleet fact) instead of ErrorState. */
  error?: string
  /** Renders ErrorState's own "Retry" button — ModelsPage.vue's own models.fetch. */
  onRetry?: () => void | Promise<void>
}>()

const emit = defineEmits<{ 'update:query': [value: string] }>()

const filtered = computed<ModelCatalogRow[]>(() => filterModelRows(props.rows, props.query))

const columns = modelCatalogColumns()

interface CatalogSection {
  key: string
  /** '' for the ungrouped (no-kind) rows. */
  title: string
  rows: ModelCatalogRow[]
}

/**
 * sections splits the filtered rows into one table per (provider, kind): a
 * provider's plain models first, then one sub-section per kind, sorted.
 * With no kind set anywhere this is a single untitled section, so an
 * unconfigured catalog renders exactly as the flat table did.
 */
const sections = computed<CatalogSection[]>(() => {
  if (!filtered.value.some((r) => r.kind)) return [{ key: 'all', title: '', rows: filtered.value }]
  const providers: string[] = []
  const byKey = new Map<string, CatalogSection>()
  for (const r of filtered.value) {
    if (!providers.includes(r.providerName)) providers.push(r.providerName)
    const key = `${r.providerName}\u0000${r.kind}`
    let sec = byKey.get(key)
    if (!sec) {
      sec = { key, title: r.kind ? `${r.providerName} · ${r.kind}` : r.providerName, rows: [] }
      byKey.set(key, sec)
    }
    sec.rows.push(r)
  }
  const kindsOf = (provider: string): string[] =>
    [...byKey.values()]
      .filter((sec) => sec.rows[0].providerName === provider)
      .map((sec) => sec.rows[0].kind)
      .sort((a, b) => (a === '' ? -1 : b === '' ? 1 : a.localeCompare(b)))
  return providers.flatMap((p) => kindsOf(p).map((k) => byKey.get(`${p}\u0000${k}`) as CatalogSection))
})

/**
 * catalogLoadState (lib/load-state.ts, verify-ui-states.md #3 fix) now
 * reads the REAL `error` prop, not a hardcoded ''. A not-yet-loaded (or
 * failed) catalog used to render "No models configured." — a false fleet
 * fact — for BOTH "still fetching" and "the fetch failed with zero rows"
 * cases alike, since `error: ''` meant loadState could only ever resolve
 * to 'skeleton' or 'empty', never 'error'. ModelsPage.vue's own top-level
 * `models.error` paragraph stays alongside this (it also covers a
 * background refresh failure while rows are already on screen, which
 * never reaches this component's own ErrorState — loadState's 'ready'
 * precedence keeps existing rows on screen through that case).
 */
const catalogLoadState = computed(() => loadState({ loading: props.loading, hasData: props.rows.length > 0, error: props.error ?? '' }))

const emptyMessage = computed<string>(() => {
  if (props.query.trim()) return `no models match "${props.query}"`
  return 'none'
})
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>Model catalog</CardTitle>
      <CardDescription>
        Every configured model: price source, per-model usage for the current range, fleet p50/p95, and error rate.
      </CardDescription>
      <SearchInput
        :model-value="query"
        placeholder="Search models or aliases..."
        class="mt-2 max-w-sm"
        @update:model-value="emit('update:query', $event)"
      />
    </CardHeader>
    <CardContent class="flex flex-col gap-3">
      <StatDisabledAlert
        v-if="!latencyEnabled"
        title="Latency statistics are off"
        config-key="admin.stats.latency"
        purpose="to see fleet p50/p95."
      />
      <SkeletonTable v-if="catalogLoadState === 'skeleton'" :rows="5" :cols="columns.length" />
      <ErrorState v-else-if="catalogLoadState === 'error'" :message="error ?? ''" :on-retry="onRetry" />
      <EmptyState v-else-if="catalogLoadState === 'empty'" title="No models configured." />
      <template v-else>
        <section v-for="sec in sections" :key="sec.key" class="flex flex-col gap-2">
          <h3 v-if="sec.title" class="text-sm font-medium text-muted-foreground">{{ sec.title }}</h3>
          <DataTable :label="sec.title ? `Model catalog: ${sec.title}` : 'Model catalog'" :columns="columns" :data="sec.rows" :empty-message="emptyMessage" />
        </section>
      </template>
    </CardContent>
  </Card>
</template>
