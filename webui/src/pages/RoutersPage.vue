<script setup lang="ts">
import { Card, CardAction, CardContent, CardHeader, CardTitle, Pagination } from '@perfect-design/ui'
import { computed, onMounted, ref, watch } from 'vue'

import { formatCompactCount, formatContextWindow } from '@/lib/format'
import { loadState } from '@/lib/load-state'
import { pageWindow } from '@/lib/paging'
import { resolveSel } from '@/lib/panel-selection'
import { sortThenSlice } from '@/lib/table-sort'
import type { SortSpec } from '@/lib/table-sort'
import { wbColumns } from '@/lib/wb-columns'
import RouterPanel from '@/panels/RouterPanel.vue'
import { useModelsStore } from '@/stores/models'
import { useNavStore } from '@/stores/nav'
import StateView from '@/widgets/StateView.vue'
import type { ViewState } from '@/widgets/StateView.vue'
import WbTable from '@/widgets/WbTable.vue'

const PAGE_SIZE = 12

interface Row {
  id: string
  name: string
  classifier: string
  tiers: string
  tierModels: string
  context: number | null
  requests: number | null
  escalations: number | null
}

const models = useModelsStore()
const nav = useNavStore()

onMounted(() => void models.fetch())

const rows = computed<Row[]>(() =>
  models.routers.map((r) => ({
    id: r.name,
    name: r.name,
    classifier: r.shadowClassifier ? `${r.classifier} (shadow ${r.shadowClassifier})` : r.classifier,
    tiers: r.tiers.map((t) => t.name).join(' › '),
    tierModels: r.tiers.map((t) => `${t.name}: ${t.models.join(', ')}`).join('\n'),
    context: r.contextTokens ?? null,
    requests: r.stats ? r.stats.requests : null,
    escalations: r.stats ? r.stats.escalations.reduce((a, e) => a + e.count, 0) : null,
  })),
)

const columns = wbColumns<Row>([
  { key: 'name', header: 'Router', kind: 'text' },
  { key: 'classifier', header: 'Classifier', kind: 'text' },
  { key: 'tiers', header: 'Tiers', kind: 'text' },
  { key: 'context', header: 'Context', kind: 'number' },
  { key: 'requests', header: 'Requests', kind: 'number' },
  { key: 'escalations', header: 'Escalations', kind: 'number' },
])

const sort = ref<SortSpec | null>(null)
const page = ref(1)
function onSort(next: SortSpec | null): void {
  sort.value = next
  page.value = 1
}
const paged = computed(() => sortThenSlice(rows.value, sort.value, PAGE_SIZE, pageWindow(rows.value.length, PAGE_SIZE, page.value - 1).index))

const state = computed<ViewState>(() => {
  const s = loadState({ loading: models.lastUpdated === null, hasData: models.lastUpdated !== null, error: models.error })
  if (s === 'skeleton') return 'loading'
  if (s === 'error') return 'error'
  return rows.value.length === 0 ? 'empty' : 'ready'
})
const scopeNote = computed(() => {
  if (models.routers.length === 0) return ''
  return models.routers.some((r) => r.stats?.scope === 'shared') ? 'Counts: all replicas, this month (UTC)' : 'Counts: this gateway instance, since start'
})

const selection = computed(() => nav.selection)
const activeKey = computed(() => (selection.value?.kind === 'router' ? selection.value.id : null))

function exists(kind: string, id: string): boolean {
  return kind === 'router' && models.routers.some((r) => r.name === id)
}
watch(
  [selection, () => models.lastUpdated],
  ([sel, loaded]) => {
    if (!sel || loaded === null) return
    if (resolveSel(sel, exists) === null) nav.closePanel()
  },
  { immediate: true },
)
const openSel = computed(() => (models.lastUpdated !== null ? resolveSel(selection.value, exists) : null))
</script>

<template>
  <div class="flex flex-col gap-4">
    <Card size="sm">
      <CardHeader class="items-center">
        <CardTitle>Smart routers</CardTitle>
        <CardAction v-if="scopeNote" class="text-muted-foreground self-center text-xs">{{ scopeNote }}</CardAction>
      </CardHeader>
      <CardContent class="flex flex-col gap-2">
        <StateView :state="state" empty-title="No smart routers configured" :error-message="models.error" :updated-at="models.lastUpdated ?? undefined" @retry="models.fetch()">
          <WbTable
            class="wb-clip"
            :columns="columns"
            :rows="paged"
            :sort="sort"
            row-key="id"
            :active-key="activeKey"
            label="Smart routers"
            @update:sort="onSort"
            @row-activate="(row: Row) => nav.openPanel('router', row.id)"
          >
            <template #cell-name="{ value }"><span class="font-mono">{{ value }}</span></template>
            <template #cell-tiers="{ row }"><span class="font-mono" :title="(row as Row).tierModels">{{ (row as Row).tiers || '—' }}</span></template>
            <template #cell-context="{ value }">{{ value === null ? '—' : formatContextWindow(value as number) }}</template>
            <template #cell-requests="{ value }">{{ value === null ? '—' : formatCompactCount(value as number) }}</template>
            <template #cell-escalations="{ value }">{{ value === null ? '—' : formatCompactCount(value as number) }}</template>
          </WbTable>
          <div v-if="rows.length > PAGE_SIZE" class="flex items-center justify-between gap-2">
            <span class="text-muted-foreground text-xs">{{ rows.length.toLocaleString('en-US') }} routers</span>
            <Pagination v-model:page="page" :total="rows.length" :page-size="PAGE_SIZE" variant="compact" size="sm" />
          </div>
        </StateView>
      </CardContent>
    </Card>

    <RouterPanel v-if="openSel?.kind === 'router'" :id="openSel.id" @close="nav.closePanel()" />
  </div>
</template>
