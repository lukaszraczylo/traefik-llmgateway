<script setup lang="ts">
import { Card, CardAction, CardContent, CardHeader, CardTitle, Pagination, SearchInput, StatusBadge } from '@perfect-design/ui'
import { computed, ref, watch } from 'vue'

import { loadState } from '@/lib/load-state'
import { pageWindow } from '@/lib/paging'
import { resolveSel } from '@/lib/panel-selection'
import { resolveTargetRef } from '@/lib/target-columns'
import { findTarget, targetRows } from '@/lib/target-rows'
import type { TargetRow } from '@/lib/target-rows'
import { sortThenSlice } from '@/lib/table-sort'
import type { SortSpec } from '@/lib/table-sort'
import { wbColumns } from '@/lib/wb-columns'
import TargetPanel from '@/panels/TargetPanel.vue'
import { useDashboardStore } from '@/stores/dashboard'
import { useNavStore } from '@/stores/nav'
import StateView from '@/widgets/StateView.vue'
import type { ViewState } from '@/widgets/StateView.vue'
import WbTable from '@/widgets/WbTable.vue'

const PAGE_SIZE = 12
const HEALTH_TONE = { healthy: 'success', unhealthy: 'danger', unknown: 'neutral' } as const

const dashboard = useDashboardStore()
const nav = useNavStore()

const all = computed(() => targetRows(dashboard.targets))
const query = computed<string>({
  get: () => nav.params.q ?? '',
  set: (v) => {
    const { q: _q, ...rest } = nav.params
    nav.goTo('targets', v ? { ...rest, q: v } : rest)
  },
})
const needle = computed(() => query.value.trim().toLowerCase())
const rows = computed(() => (needle.value === '' ? all.value : all.value.filter((r) => r.name.toLowerCase().includes(needle.value) || r.host.toLowerCase().includes(needle.value))))

const columns = wbColumns<TargetRow>([
  { key: 'kind', header: 'Kind', kind: 'text' },
  { key: 'name', header: 'Name', kind: 'text' },
  { key: 'health', header: 'Health', kind: 'status', sortable: true },
  { key: 'host', header: 'Host', kind: 'text' },
])

const sort = ref<SortSpec | null>(null)
const page = ref(1)
const sortSpec = computed<SortSpec | null>(() => (sort.value?.key === 'health' ? { ...sort.value, key: 'healthRank' } : sort.value))
const paged = computed(() => sortThenSlice(rows.value, sortSpec.value, PAGE_SIZE, pageWindow(rows.value.length, PAGE_SIZE, page.value - 1).index))
function onSort(next: SortSpec | null): void {
  sort.value = next
  page.value = 1
}
watch(needle, () => {
  page.value = 1
})

const state = computed<ViewState>(() => {
  const s = loadState({ loading: dashboard.lastUpdated === null, hasData: dashboard.targets !== null, error: dashboard.error })
  if (s === 'skeleton') return 'loading'
  if (s === 'error') return 'error'
  return rows.value.length === 0 ? 'empty' : 'ready'
})
const emptyTitle = computed(() => (needle.value !== '' ? `No targets match "${query.value.trim()}"` : 'No MCP servers or agents configured'))
const emptyDescription = computed(() => (needle.value !== '' ? undefined : 'Add an mcpServers or agents entry to the middleware config to see them here.'))
const unhealthy = computed(() => all.value.filter((r) => r.health === 'unhealthy').length)
const summary = computed(() => (all.value.length === 0 ? '' : `${all.value.length.toLocaleString('en-US')} targets · ${unhealthy.value} unhealthy`))

const selection = computed(() => nav.selection)
const activeKey = computed(() => (selection.value?.kind === 'target' ? selection.value.id : null))
// A selection may name a target by bare name; rows carry the composed id.
const activeRow = computed(() => {
  const res = activeKey.value === null ? null : resolveTargetRef(activeKey.value, dashboard.targets)
  return res?.kind === 'ref' ? res.ref : null
})

function exists(kind: string, id: string): boolean {
  return kind === 'target' && findTarget(dashboard.targets, id) !== null
}
watch(
  [selection, () => dashboard.targets],
  ([sel, loaded]) => {
    if (!sel || loaded === null) return
    if (resolveSel(sel, exists) === null) nav.closePanel()
  },
  { immediate: true },
)
const openSel = computed(() => (dashboard.targets !== null ? resolveSel(selection.value, exists) : null))
</script>

<template>
  <div class="flex flex-col gap-4">
    <Card size="sm">
      <CardHeader class="items-center">
        <CardTitle>MCP servers and agents</CardTitle>
        <CardAction v-if="summary" class="text-muted-foreground self-center text-xs">{{ summary }}</CardAction>
      </CardHeader>
      <CardContent class="flex flex-col gap-2">
        <SearchInput v-model="query" size="sm" class="max-w-sm" placeholder="Filter by name or host" aria-label="Filter targets" />
        <StateView :state="state" :empty-title="emptyTitle" :empty-description="emptyDescription" :error-message="dashboard.error" :updated-at="dashboard.lastUpdated ?? undefined" @retry="dashboard.refresh()">
          <WbTable
            class="wb-clip"
            :columns="columns"
            :rows="paged"
            :sort="sort"
            row-key="id"
            :active-key="activeRow"
            label="MCP servers and agents"
            @update:sort="onSort"
            @row-activate="(row: TargetRow) => nav.openPanel('target', row.id)"
          >
            <template #cell-name="{ value }"><span class="font-mono">{{ value }}</span></template>
            <template #cell-health="{ value }"><StatusBadge :status="value as string" :tones="HEALTH_TONE" size="sm" /></template>
            <template #cell-host="{ value }"><span class="font-mono">{{ value }}</span></template>
          </WbTable>
          <div v-if="rows.length > PAGE_SIZE" class="flex items-center justify-between gap-2">
            <span class="text-muted-foreground text-xs">{{ rows.length.toLocaleString('en-US') }} targets</span>
            <Pagination v-model:page="page" :total="rows.length" :page-size="PAGE_SIZE" variant="compact" size="sm" />
          </div>
        </StateView>
      </CardContent>
    </Card>

    <TargetPanel v-if="openSel?.kind === 'target'" :id="openSel.id" @close="nav.closePanel()" />
  </div>
</template>
