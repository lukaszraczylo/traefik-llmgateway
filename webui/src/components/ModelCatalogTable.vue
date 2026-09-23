<script setup lang="ts">
import { computed } from 'vue'

import DataTable from '@/components/DataTable.vue'
import SearchInput from '@/components/SearchInput.vue'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { filterModelRows } from '@/lib/model-filter'
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
}>()

const emit = defineEmits<{ 'update:query': [value: string] }>()

const filtered = computed<ModelCatalogRow[]>(() => filterModelRows(props.rows, props.query))

const columns = modelCatalogColumns()

const emptyMessage = computed<string>(() => {
  if (props.loading) return 'loading…'
  if (props.rows.length === 0) return 'no models configured'
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
        <AlertDescription>
          Latency percentiles (p50/p95) are off for this deployment. Enable <code>admin.stats.latency</code> to see them.
        </AlertDescription>
      </Alert>
      <DataTable :columns="columns" :data="filtered" :empty-message="emptyMessage" />
    </CardContent>
  </Card>
</template>
