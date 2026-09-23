<script setup lang="ts">
import { computed } from 'vue'

import DataTable from '@/components/DataTable.vue'
import EmptyState from '@/components/EmptyState.vue'
import ErrorState from '@/components/ErrorState.vue'
import SearchInput from '@/components/SearchInput.vue'
import SkeletonTable from '@/components/SkeletonTable.vue'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { filterModelRows } from '@/lib/model-filter'
import { loadState } from '@/lib/load-state'
import { modelCatalogColumns } from '@/lib/model-table-columns'
import type { ModelCatalogRow } from '@/lib/model-table-columns'
import { useNavStore } from '@/stores/nav'

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

const nav = useNavStore()

const filtered = computed<ModelCatalogRow[]>(() => filterModelRows(props.rows, props.query))

const columns = modelCatalogColumns()

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
      <Alert v-if="!latencyEnabled" variant="warn">
        <AlertTitle>Latency statistics are off</AlertTitle>
        <AlertDescription class="flex flex-wrap items-center gap-2">
          <span>Enable <code>admin.stats.latency</code> in the middleware config to see fleet p50/p95.</span>
          <Button type="button" variant="outline" size="sm" @click="nav.goTo('config')">Open Config</Button>
        </AlertDescription>
      </Alert>
      <SkeletonTable v-if="catalogLoadState === 'skeleton'" :rows="5" :cols="columns.length" />
      <ErrorState v-else-if="catalogLoadState === 'error'" :message="error ?? ''" :on-retry="onRetry" />
      <EmptyState v-else-if="catalogLoadState === 'empty'" title="No models configured." />
      <DataTable v-else :columns="columns" :data="filtered" :empty-message="emptyMessage" />
    </CardContent>
  </Card>
</template>
