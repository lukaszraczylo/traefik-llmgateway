<script setup lang="ts">
import { Card, CardAction, CardContent, CardHeader, CardTitle, Pagination, SearchInput, SegmentedControl } from '@perfect-design/ui'
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'

import { useNow } from '@/composables/useNow'
import { formatCompactCount } from '@/lib/format'
import { formatLastSeen } from '@/lib/last-seen'
import { loadState } from '@/lib/load-state'
import { isHot, primaryLimit } from '@/lib/meters'
import { pageWindow } from '@/lib/paging'
import { sortThenSlice } from '@/lib/table-sort'
import type { SortSpec } from '@/lib/table-sort'
import { resolveSel } from '@/lib/panel-selection'
import { wbColumns } from '@/lib/wb-columns'
import GroupPanel from '@/panels/GroupPanel.vue'
import UserPanel from '@/panels/UserPanel.vue'
import { useDashboardStore } from '@/stores/dashboard'
import { useNavStore } from '@/stores/nav'
import type { AdminUsageEntryView } from '@/types/api'
import LimitBadge from '@/widgets/LimitBadge.vue'
import StateView from '@/widgets/StateView.vue'
import type { ViewState } from '@/widgets/StateView.vue'
import WbTable from '@/widgets/WbTable.vue'

const PAGE_SIZE = 12
type Tab = 'users' | 'groups'
const TAB_OPTIONS = [
  { value: 'users', label: 'Users' },
  { value: 'groups', label: 'Groups' },
]

interface Row {
  id: string
  name: string
  group: string
  members: number
  lastSeen: string
  requests: number
  tokens: number
  share: number | null
  limit: number | null
  limitUnit: string
  hot: boolean
}

const dashboard = useDashboardStore()
const nav = useNavStore()
const { now, start: startClock, stop: stopClock } = useNow()

const usage = computed(() => dashboard.usage)
const tab = computed<Tab>(() => (nav.params.tab === 'groups' ? 'groups' : 'users'))

function pick(next: unknown): void {
  if (next !== 'users' && next !== 'groups') return
  const { tab: _tab, ...rest } = nav.params
  nav.goTo('users', next === 'groups' ? { ...rest, tab: 'groups' } : rest)
}

const memberCounts = computed(() => new Map((dashboard.overview?.groups ?? []).map((g) => [g.name, g.memberCount])))
// Fleet total can lag the per-scope counters, so never let a share exceed the visible sum.
const totalRequests = computed(() => Math.max(usage.value?.total.requestsPerDay ?? 0, (usage.value?.users ?? []).reduce((a, u) => a + u.requestsPerDay, 0)))

function toRow(e: AdminUsageEntryView): Row {
  const { limit, unit } = primaryLimit(e.limits)
  return {
    id: e.id,
    name: e.id,
    group: (e.groups ?? (e.groupName ? [e.groupName] : [])).join(', ') || '—',
    members: memberCounts.value.get(e.id) ?? 0,
    lastSeen: formatLastSeen(e.lastSeen, now.value),
    requests: e.requestsPerDay,
    tokens: e.tokensInPerDay + e.tokensOutPerDay,
    share: totalRequests.value > 0 ? Math.min(1, e.requestsPerDay / totalRequests.value) : null,
    limit,
    limitUnit: unit,
    hot: isHot(e, e.limits),
  }
}

const byRequests = (a: Row, b: Row): number => b.requests - a.requests || a.name.localeCompare(b.name)
const userRows = computed<Row[]>(() => (usage.value?.users ?? []).map(toRow).sort(byRequests))
const groupRows = computed<Row[]>(() => (usage.value?.groups ?? []).map(toRow).sort(byRequests))
const query = computed<string>({
  get: () => nav.params.q ?? '',
  set: (v) => {
    const { q: _q, ...rest } = nav.params
    nav.goTo('users', v ? { ...rest, q: v } : rest)
  },
})
const needle = computed(() => query.value.trim().toLowerCase())
const allRows = computed(() => (tab.value === 'users' ? userRows.value : groupRows.value))
const rows = computed(() =>
  needle.value === '' ? allRows.value : allRows.value.filter((r) => r.name.toLowerCase().includes(needle.value) || (tab.value === 'users' && r.group.toLowerCase().includes(needle.value))),
)

const userColumns = wbColumns<Row>([
  { key: 'name', header: 'Name', kind: 'text' },
  { key: 'group', header: 'Group', kind: 'text' },
  { key: 'lastSeen', header: 'Last seen', kind: 'time', sortable: false },
  { key: 'requests', header: 'Req today', kind: 'number' },
  { key: 'tokens', header: 'Tokens today', kind: 'number' },
  { key: 'share', header: 'Share', kind: 'share' },
  { key: 'limit', header: 'Limit', kind: 'limit', sortable: false },
])
const groupColumns = wbColumns<Row>([
  { key: 'name', header: 'Name', kind: 'text' },
  { key: 'members', header: 'Members', kind: 'number' },
  { key: 'requests', header: 'Req today', kind: 'number' },
  { key: 'tokens', header: 'Tokens today', kind: 'number' },
  { key: 'share', header: 'Share', kind: 'share' },
  { key: 'limit', header: 'Limit', kind: 'limit', sortable: false },
])
const columns = computed(() => (tab.value === 'users' ? userColumns : groupColumns))

const page = ref(1)
watch([tab, needle], () => {
  page.value = 1
  sort.value = null
})
const window_ = computed(() => pageWindow(rows.value.length, PAGE_SIZE, page.value - 1))
const sort = ref<SortSpec | null>(null)
const paged = computed(() => sortThenSlice(rows.value, sort.value, PAGE_SIZE, window_.value.index))
function onSort(next: SortSpec | null): void {
  sort.value = next
  page.value = 1
}

const state = computed<ViewState>(() => {
  const s = loadState({ loading: dashboard.lastUpdated === null, hasData: usage.value !== null, error: dashboard.error })
  if (s === 'skeleton') return 'loading'
  if (s === 'error') return 'error'
  return rows.value.length === 0 ? 'empty' : 'ready'
})
const emptyTitle = computed(() =>
  needle.value !== '' ? `No ${tab.value} match "${query.value.trim()}"` : tab.value === 'users' ? 'No users configured' : 'No groups configured',
)

const selection = computed(() => nav.selection)
const activeKey = computed(() => (selection.value?.kind === (tab.value === 'users' ? 'user' : 'group') ? selection.value.id : null))

function exists(kind: string, id: string): boolean {
  if (kind === 'user') return (usage.value?.users ?? []).some((u) => u.id === id)
  if (kind === 'group') return (usage.value?.groups ?? []).some((g) => g.id === id)
  return false
}
// An unknown or foreign sel is dropped in place once usage has loaded.
watch(
  [selection, usage],
  ([sel, u]) => {
    if (!sel || !u) return
    if (resolveSel(sel, (k, id) => exists(k, id)) === null) nav.closePanel()
  },
  { immediate: true },
)
const openSel = computed(() => (usage.value ? resolveSel(selection.value, (k, id) => exists(k, id)) : null))

function open(row: Row): void {
  nav.openPanel(tab.value === 'users' ? 'user' : 'group', row.id)
}

onMounted(startClock)
onUnmounted(stopClock)
</script>

<template>
  <div class="flex flex-col gap-4">
    <Card size="sm">
      <CardHeader class="items-center">
        <CardTitle>{{ tab === 'users' ? 'Users' : 'Groups' }}</CardTitle>
        <CardAction class="self-center">
          <SegmentedControl :model-value="tab" :options="TAB_OPTIONS" size="sm" aria-label="Users or groups" @update:model-value="pick" />
        </CardAction>
      </CardHeader>
      <CardContent class="flex flex-col gap-2">
        <SearchInput v-model="query" size="sm" class="max-w-sm" :placeholder="tab === 'users' ? 'Filter by name or group' : 'Filter by name'" :aria-label="`Filter ${tab}`" />
        <StateView :state="state" :empty-title="emptyTitle" :error-message="dashboard.error" :updated-at="dashboard.lastUpdated ?? undefined" @retry="dashboard.refresh()">
          <WbTable
            class="wb-clip"
            :columns="columns"
            :rows="paged"
            :sort="sort"
            row-key="id"
            :active-key="activeKey"
            :label="tab === 'users' ? 'Users' : 'Groups'"
            @update:sort="onSort"
            @row-activate="open"
          >
            <template #cell-members="{ value }">{{ formatCompactCount(value as number) }}</template>
            <template #cell-requests="{ value }">{{ formatCompactCount(value as number) }}</template>
            <template #cell-tokens="{ value }">{{ formatCompactCount(value as number) }}</template>
            <template #cell-limit="{ row }">
              <LimitBadge :limit="(row as Row).limit" :unit="(row as Row).limitUnit" :hot="(row as Row).hot" />
            </template>
          </WbTable>
          <div v-if="rows.length > PAGE_SIZE" class="flex items-center justify-between gap-2">
            <span class="text-muted-foreground text-xs">{{ rows.length.toLocaleString('en-US') }} {{ tab }}</span>
            <Pagination v-model:page="page" :total="rows.length" :page-size="PAGE_SIZE" variant="compact" size="sm" />
          </div>
        </StateView>
      </CardContent>
    </Card>

    <UserPanel v-if="openSel?.kind === 'user'" :id="openSel.id" @close="nav.closePanel()" />
    <GroupPanel v-else-if="openSel?.kind === 'group'" :id="openSel.id" @close="nav.closePanel()" />
  </div>
</template>
