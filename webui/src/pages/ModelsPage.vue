<script setup lang="ts">
import { Badge, Card, CardAction, CardContent, CardHeader, CardTitle, FilterBar, Pagination, SearchInput, StatusDot } from '@perfect-design/ui'
import type { FilterField, FilterItem } from '@perfect-design/ui'
import { computed, onMounted, reactive, watch } from 'vue'

import { formatCompactCount, formatContextWindow, formatModelCostHover } from '@/lib/format'
import { loadState } from '@/lib/load-state'
import { NO_KIND, groupByProvider, priceStatus, selectModels, sortThenSlice } from '@/lib/models-view'
import type { ModelSelection, PriceStatus, SortSpec } from '@/lib/models-view'
import { pageWindow } from '@/lib/paging'
import { resolveSel } from '@/lib/panel-selection'
import { breakerRows, providerRateStatus, providerSuccessRate } from '@/lib/provider-rate'
import type { BreakerRow } from '@/lib/provider-rate'
import { routersByModel } from '@/lib/router-index'
import { wbColumns } from '@/lib/wb-columns'
import ModelPanel from '@/panels/ModelPanel.vue'
import ProviderPanel from '@/panels/ProviderPanel.vue'
import { useDashboardStore } from '@/stores/dashboard'
import { useFiltersStore } from '@/stores/filters'
import { useModelsStore } from '@/stores/models'
import { useNavStore } from '@/stores/nav'
import ModelName from '@/widgets/ModelName.vue'
import StateView from '@/widgets/StateView.vue'
import type { ViewState } from '@/widgets/StateView.vue'
import WbTable from '@/widgets/WbTable.vue'

const PAGE_SIZE = 12
const ROUTER_BADGES = 2
const FILTER_KEYS = ['provider', 'kind', 'price'] as const
type FilterKey = (typeof FILTER_KEYS)[number]
const PRICE_OPTIONS: Array<{ value: PriceStatus; label: string }> = [
  { value: 'priced', label: 'Priced' },
  { value: 'unpriced', label: 'Unpriced' },
  { value: 'free', label: 'Free' },
]
const HEALTH_TONE = { healthy: 'success', degraded: 'warning', severe: 'danger' } as const
const BREAKER_TONE = { closed: 'success', open: 'danger', 'half-open': 'warning', degraded: 'warning' } as const

interface Row {
  id: string
  name: string
  kind: string
  context: number | null
  price: PriceStatus
  priceTitle: string
  requests: number | null
  health: number | null
  healthTone: 'success' | 'warning' | 'danger' | null
  routers: string[]
}

const dashboard = useDashboardStore()
const filters = useFiltersStore()
const models = useModelsStore()
const nav = useNavStore()

onMounted(() => void models.fetch())
watch(
  () => filters.range,
  () => void models.fetch(),
)

// --- Filters (hash params q, provider, kind, price) ------------------------------

function setParams(patch: Record<string, string>): void {
  const next = { ...nav.params }
  for (const [k, v] of Object.entries(patch)) {
    if (v === '') delete next[k]
    else next[k] = v
  }
  nav.goTo('models', next)
}

const query = computed<string>({
  get: () => nav.params.q ?? '',
  set: (v) => setParams({ q: v }),
})
const selection = computed<ModelSelection>(() => ({
  q: query.value,
  provider: nav.params.provider,
  kind: nav.params.kind,
  price: nav.params.price,
}))
const filtering = computed(() => query.value.trim() !== '' || FILTER_KEYS.some((k) => nav.params[k]))

const overviewProviders = computed(() => dashboard.overview?.providers ?? [])
const fields = computed<FilterField[]>(() => {
  const kinds = [...new Set(models.rows.map((r) => r.kind).filter((k) => k !== ''))].sort()
  const providers = [...new Set([...overviewProviders.value.map((p) => p.name), ...models.rows.map((r) => r.providerName)])].sort()
  return [
    { id: 'provider', label: 'Provider', type: 'select', operators: ['eq'], options: providers.map((p) => ({ value: p, label: p })) },
    { id: 'kind', label: 'Kind', type: 'select', operators: ['eq'], options: [{ value: NO_KIND, label: 'No kind' }, ...kinds.map((k) => ({ value: k, label: k }))] },
    { id: 'price', label: 'Price', type: 'select', operators: ['eq'], options: PRICE_OPTIONS },
  ]
})
const filterItems = computed<FilterItem[]>(() =>
  FILTER_KEYS.filter((k) => nav.params[k]).map((k) => ({ id: k, field: k, operator: 'eq', value: nav.params[k]! })),
)
function applyFilters(items: FilterItem[]): void {
  const patch: Record<string, string> = { provider: '', kind: '', price: '' }
  for (const it of items) {
    if ((FILTER_KEYS as readonly string[]).includes(it.field) && typeof it.value === 'string') patch[it.field as FilterKey] = it.value
  }
  setParams(patch)
}

const providerNames = computed(() =>
  [...new Set([...overviewProviders.value.map((p) => p.name), ...models.rows.map((r) => r.providerName)])].sort(),
)
watch(
  [() => providerNames.value.join(','), () => filters.range],
  () => void models.fetchProviderRequests(providerNames.value),
  { immediate: true },
)
const providerRequests = (provider: string): number | null => models.providerRequests?.[provider] ?? null

// --- Rows and groups ---------------------------------------------------------------

const selected = computed(() => selectModels(models.rows, selection.value))
const routerIndex = computed(() => routersByModel(models.routers))
const providerViews = computed(() => new Map(overviewProviders.value.map((p) => [p.name, p])))
const breakers = computed(() => new Map(breakerRows(dashboard.overview).map((b) => [b.provider, b])))

const groups = computed(() => {
  const grouped = groupByProvider(selected.value)
  if (!filtering.value) {
    const seen = new Set(grouped.map((g) => g.provider))
    for (const p of overviewProviders.value) if (!seen.has(p.name)) grouped.push({ provider: p.name, rows: [], requests: 0 })
  }
  const total = (p: string): number => providerRequests(p) ?? -1
  return grouped.sort((a, b) => total(b.provider) - total(a.provider) || a.provider.localeCompare(b.provider))
})

// --- Sort (hash params sort, dir) ------------------------------------------------

const SORT_KEYS: readonly string[] = ['name', 'kind', 'context', 'price', 'requests']
const sort = computed<SortSpec | null>(() => {
  const key = nav.params.sort
  return key && SORT_KEYS.includes(key) ? { key, direction: nav.params.dir === 'desc' ? 'desc' : 'asc' } : null
})
function onSort(next: SortSpec | null): void {
  pages.clear()
  setParams({ sort: next?.key ?? '', dir: next?.direction === 'desc' ? 'desc' : '' })
}

function breakerStatus(provider: string): keyof typeof BREAKER_TONE {
  const b: BreakerRow | undefined = breakers.value.get(provider)
  if (!b) return 'closed'
  return b.degraded ? 'degraded' : b.state
}

function toRow(r: (typeof models.rows)[number]): Row {
  const rate = providerViews.value.get(r.providerName)?.modelRates?.[r.model]
  const success = rate ? providerSuccessRate(rate.attemptsDay, rate.failuresDay) : null
  const status = providerRateStatus(success)
  const known = r.inputPerMTokUsd !== undefined && r.outputPerMTokUsd !== undefined
  const routers = [...new Set([...(r.routers ?? []), ...(routerIndex.value.get(r.id) ?? [])])]
  return {
    id: r.id,
    name: r.model,
    kind: r.kind,
    context: r.contextTokens ?? null,
    price: priceStatus(r),
    priceTitle: known ? formatModelCostHover(r.inputPerMTokUsd as number, r.outputPerMTokUsd as number) : r.priceSource,
    requests: r.ranked === false ? null : r.requests,
    health: success === null ? null : Math.round(success * 1000) / 10,
    healthTone: status === 'no-traffic' ? null : HEALTH_TONE[status],
    routers,
  }
}

const columns = wbColumns<Row>([
  { key: 'name', header: 'Model', kind: 'text' },
  { key: 'kind', header: 'Kind', kind: 'text' },
  { key: 'context', header: 'Context', kind: 'number' },
  { key: 'price', header: 'Price', kind: 'text' },
  { key: 'requests', header: 'Requests', kind: 'number' },
  { key: 'health', header: 'Health', kind: 'number', sortable: false },
  { key: 'routers', header: 'Routers', kind: 'text', sortable: false },
])

// --- Expansion and per-group paging ------------------------------------------------

const toggled = reactive(new Map<string, boolean>())
const pages = reactive(new Map<string, number>())
watch([selection], () => {
  toggled.clear()
  pages.clear()
})

function isOpen(provider: string, index: number, count: number): boolean {
  if (count === 0) return false
  return toggled.get(provider) ?? (filtering.value || index === 0)
}
function toggle(provider: string, open: boolean): void {
  toggled.set(provider, !open)
}
function pageOf(provider: string, total: number): number {
  return pageWindow(total, PAGE_SIZE, (pages.get(provider) ?? 1) - 1).index + 1
}
function rowsOf(g: (typeof groups.value)[number]): Row[] {
  return sortThenSlice(g.rows.map(toRow), sort.value, PAGE_SIZE, pageOf(g.provider, g.rows.length) - 1)
}

// --- State, selection ----------------------------------------------------------------

const state = computed<ViewState>(() => {
  const s = loadState({ loading: models.lastUpdated === null, hasData: models.lastUpdated !== null, error: models.error })
  if (s === 'skeleton') return 'loading'
  if (s === 'error') return 'error'
  return groups.value.length === 0 ? 'empty' : 'ready'
})
const emptyTitle = computed(() => (filtering.value ? 'No models match these filters' : 'No models configured'))
const summary = computed(() => `${selected.value.length.toLocaleString('en-US')} models · ${groups.value.length.toLocaleString('en-US')} providers`)

const sel = computed(() => nav.selection)
const activeModel = computed(() => (sel.value?.kind === 'model' ? sel.value.id : null))

function exists(kind: string, id: string): boolean {
  if (kind === 'model') return models.rows.some((r) => r.id === id)
  if (kind === 'provider') return providerViews.value.has(id) || models.rows.some((r) => r.providerName === id)
  return false
}
watch(
  [sel, () => models.lastUpdated, () => dashboard.overview],
  ([s, loaded, overview]) => {
    if (!s || loaded === null || !overview) return
    if (resolveSel(s, exists) === null) nav.closePanel()
  },
  { immediate: true },
)
const openSel = computed(() => (models.lastUpdated !== null && dashboard.overview ? resolveSel(sel.value, exists) : null))

</script>

<template>
  <div class="flex flex-col gap-4">
    <Card size="sm">
      <CardHeader class="items-center">
        <CardTitle>Models &amp; providers</CardTitle>
        <CardAction class="text-muted-foreground self-center text-xs">{{ summary }}</CardAction>
      </CardHeader>
      <CardContent class="flex flex-col gap-2">
        <div class="flex flex-wrap items-center gap-2">
          <SearchInput v-model="query" size="sm" class="w-full max-w-xs" placeholder="Filter by model or alias" aria-label="Filter models" />
          <FilterBar :model-value="filterItems" :fields="fields" variant="compact" size="sm" clearable @update:model-value="applyFilters" />
        </div>
        <StateView :state="state" :empty-title="emptyTitle" :error-message="models.error" :updated-at="models.lastUpdated ?? undefined" @retry="models.fetch()">
          <div class="flex flex-col gap-2">
            <section v-for="(g, i) in groups" :key="g.provider" class="rounded-md border" :aria-label="`${g.provider} models`">
              <div class="bg-muted/40 flex h-8 items-center gap-2 px-3 text-sm">
                <button
                  type="button"
                  class="text-muted-foreground hover:text-foreground flex size-5 shrink-0 items-center justify-center rounded disabled:opacity-40"
                  :aria-expanded="isOpen(g.provider, i, g.rows.length)"
                  :aria-label="`${isOpen(g.provider, i, g.rows.length) ? 'Collapse' : 'Expand'} ${g.provider} models`"
                  :disabled="g.rows.length === 0"
                  @click="toggle(g.provider, isOpen(g.provider, i, g.rows.length))"
                >
                  <span aria-hidden="true" class="inline-block transition-transform" :class="isOpen(g.provider, i, g.rows.length) ? 'rotate-90' : ''">›</span>
                </button>
                <button type="button" class="hover:underline min-w-0 truncate font-medium" :aria-label="`Open provider ${g.provider}`" @click="nav.openPanel('provider', g.provider)">
                  {{ g.provider }}
                </button>
                <StatusDot :tone="BREAKER_TONE[breakerStatus(g.provider)]" size="md" class="shrink-0" />
                <span v-if="breakerStatus(g.provider) !== 'closed'" class="text-muted-foreground shrink-0 text-xs">{{ breakerStatus(g.provider) }}</span>
                <span class="text-muted-foreground ml-auto shrink-0 font-mono text-xs tabular-nums">{{ g.rows.length.toLocaleString('en-US') }} models</span>
                <span class="text-muted-foreground hidden shrink-0 font-mono text-xs tabular-nums sm:inline">{{ providerRequests(g.provider) === null ? '—' : formatCompactCount(providerRequests(g.provider) as number) }} req</span>
              </div>
              <div v-if="isOpen(g.provider, i, g.rows.length)" class="flex flex-col gap-2 p-1">
                <WbTable
                  :columns="columns"
                  :rows="rowsOf(g)"
                  row-key="id"
                  :active-key="activeModel"
                  :sort="sort"
                  @update:sort="onSort"
                  :label="`${g.provider} models`"
                  @row-activate="(row: Row) => nav.openPanel('model', row.id)"
                >
                  <template #cell-name="{ row }">
                    <ModelName :id="(row as Row).id" bare />
                  </template>
                  <template #cell-kind="{ value }">
                    <span :class="value ? '' : 'text-muted-foreground'">{{ value || '—' }}</span>
                  </template>
                  <template #cell-context="{ value }">{{ value === null ? '—' : formatContextWindow(value as number) }}</template>
                  <template #cell-price="{ row }">
                    <Badge :tone="(row as Row).price === 'unpriced' ? 'warning' : 'neutral'" variant="outline" size="sm" :title="(row as Row).priceTitle">{{ (row as Row).price === 'none' ? '—' : (row as Row).price }}</Badge>
                  </template>
                  <template #cell-requests="{ value }">{{ value === null ? '—' : formatCompactCount(value as number) }}</template>
                  <template #cell-health="{ row }">
                    <span v-if="(row as Row).health === null" class="text-muted-foreground">—</span>
                    <span v-else class="inline-flex items-center justify-end gap-1.5">
                      <StatusDot v-if="(row as Row).healthTone" :tone="(row as Row).healthTone!" size="md" />
                      {{ (row as Row).health }}%
                    </span>
                  </template>
                  <template #cell-routers="{ value }">
                    <span v-if="(value as string[]).length === 0" class="text-muted-foreground">—</span>
                    <span v-else class="flex items-center gap-1" :title="(value as string[]).join(', ')">
                      <Badge v-for="r in (value as string[]).slice(0, ROUTER_BADGES)" :key="r" tone="info" variant="outline" size="sm" class="max-w-44 truncate font-mono">{{ r }}</Badge>
                      <span v-if="(value as string[]).length > ROUTER_BADGES" class="text-muted-foreground text-xs">+{{ (value as string[]).length - ROUTER_BADGES }}</span>
                    </span>
                  </template>
                </WbTable>
                <div v-if="g.rows.length > PAGE_SIZE" class="flex items-center justify-between gap-2 px-2 pb-1">
                  <span class="text-muted-foreground text-xs">{{ g.rows.length.toLocaleString('en-US') }} models</span>
                  <Pagination :page="pageOf(g.provider, g.rows.length)" :total="g.rows.length" :page-size="PAGE_SIZE" variant="compact" size="sm" @update:page="pages.set(g.provider, $event)" />
                </div>
              </div>
            </section>
          </div>
        </StateView>
      </CardContent>
    </Card>

    <ModelPanel v-if="openSel?.kind === 'model'" :id="openSel.id" @close="nav.closePanel()" />
    <ProviderPanel v-else-if="openSel?.kind === 'provider'" :id="openSel.id" @close="nav.closePanel()" />
  </div>
</template>
