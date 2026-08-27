<script setup lang="ts">
import { computed, onMounted, onUnmounted, watch } from 'vue'

import SearchInput from '@/components/SearchInput.vue'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import ModelUsageChart from '@/components/ModelUsageChart.vue'
import UsageChart from '@/components/UsageChart.vue'
import { useSearchQuery } from '@/composables/useSearchQuery'
import { groupMatches, userMatches } from '@/lib/usage-search'
import { useDashboardStore } from '@/stores/dashboard'
import {
  type ChartTab,
  MODEL_METRIC_LABEL,
  type ModelMetric,
  type TimeSeriesTab,
  useHistoryStore,
  WINDOW_LABEL,
} from '@/stores/history'
import type { HistoryWindow } from '@/types/api'

const dashboard = useDashboardStore()
const history = useHistoryStore()

// --- user/group search filter (operator feature) ---
//
// Filters the scope picker's user/group list only — "total" always stays
// reachable (unconditionally the list's first entry, below), and the
// CURRENTLY SELECTED scope is always kept in the list even if it stops
// matching a new query: the filter narrows what's OFFERED, it must never
// force-switch (or hide the label of) whatever chart is already rendered.
// Same shared SearchInput/useSearchQuery and lib/usage-search.ts matching
// helpers as UsageView.vue — one implementation, no second copy.
const { query: scopeQuery, normalized: normalizedScopeQuery, hasQuery: hasScopeQuery } = useSearchQuery()

const scopeOptions = computed(() => {
  const allUsers = dashboard.usage?.users ?? []
  const allGroups = dashboard.usage?.groups ?? []
  const users = allUsers
    .filter((u) => !hasScopeQuery.value || userMatches(u, normalizedScopeQuery.value) || `user:${u.id}` === history.scope)
    .map((u) => ({ value: `user:${u.id}`, label: u.id }))
  const groups = allGroups
    .filter(
      (g) =>
        !hasScopeQuery.value || groupMatches(g, allUsers, normalizedScopeQuery.value) || `group:${g.id}` === history.scope,
    )
    .map((g) => ({ value: `group:${g.id}`, label: g.id }))
  // Models come from the ranking endpoint, which returns only models with
  // non-zero traffic (operator requirement: the catalog is hundreds of
  // models, so the picker must never list idle ones). Matched on the plain
  // id rather than through lib/usage-search's helpers — those match a
  // user's or group's own shape, and a model entry has neither.
  const models = (history.modelOptions ?? [])
    .filter(
      (m) =>
        !hasScopeQuery.value ||
        m.id.toLowerCase().includes(normalizedScopeQuery.value) ||
        `model:${m.id}` === history.scope,
    )
    .map((m) => ({ value: `model:${m.id}`, label: m.id }))
  // A model already being charted stays in the list even once it drops out
  // of modelOptions entirely — which happens the moment it goes idle for
  // the selected window, since that list is non-zero-only. Without this,
  // the watch below would read it as a vanished scope and force-switch the
  // reader to "total" mid-look, for the ordinary reason that a model
  // simply stopped receiving traffic.
  if (history.scope.startsWith('model:') && !models.some((m) => m.value === history.scope)) {
    models.push({ value: history.scope, label: history.scope.slice('model:'.length) })
  }
  return [{ value: 'total', label: 'Total (all traffic)' }, ...groups, ...users, ...models]
})

const windows: HistoryWindow[] = ['hour', 'day', 'month']
const modelMetrics: ModelMetric[] = ['cost', 'req', 'tokin', 'tokout']

/**
 * UsageChart only ever renders on a time-series tab (the template's v-else
 * below), but a `v-else` narrows nothing for the type checker — this does.
 * The 'requests' stand-in is never displayed: it is the value handed over
 * on the one tab where the component is not rendered at all.
 */
const timeSeriesTab = computed<TimeSeriesTab>(() => (history.tab === 'models' ? 'requests' : history.tab))

function onTabChange(value: string | number): void {
  history.setTab(value as ChartTab)
}
function onScopeChange(value: unknown): void {
  if (typeof value === 'string') history.setScope(value)
}
function onModelMetricChange(value: unknown): void {
  if (typeof value === 'string') history.setModelMetric(value as ModelMetric)
}

onMounted(() => {
  void history.refresh()
  history.startAutoRefresh()
})
onUnmounted(() => {
  history.stopAutoRefresh()
})

// The scope list is only known once the Usage response has loaded — if the
// previously-selected scope disappears (a user/group config change), fall
// back to the total scope rather than requesting a now-unknown id.
watch(scopeOptions, (options) => {
  if (!options.some((o) => o.value === history.scope)) {
    history.setScope('total')
  }
})
</script>

<template>
  <Card>
    <CardHeader class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
      <CardTitle>Usage charts</CardTitle>
      <div class="flex flex-wrap items-center gap-3">
        <SearchInput v-model="scopeQuery" placeholder="Filter users, groups or models" class="w-56" />

        <Select
          v-if="history.tab === 'models'"
          :model-value="history.modelMetric"
          @update:model-value="onModelMetricChange"
        >
          <SelectTrigger class="w-40" aria-label="Rank models by">
            <SelectValue placeholder="Rank by" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem v-for="m in modelMetrics" :key="m" :value="m">
              {{ MODEL_METRIC_LABEL[m] }}
            </SelectItem>
          </SelectContent>
        </Select>

        <Select v-else :model-value="history.scope" @update:model-value="onScopeChange">
          <SelectTrigger class="w-56">
            <SelectValue placeholder="Scope" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem v-for="opt in scopeOptions" :key="opt.value" :value="opt.value">
              {{ opt.label }}
            </SelectItem>
          </SelectContent>
        </Select>

        <Tabs :model-value="history.window" @update:model-value="(v) => history.setWindow(v as HistoryWindow)">
          <TabsList>
            <TabsTrigger v-for="w in windows" :key="w" :value="w">{{ WINDOW_LABEL[w] }}</TabsTrigger>
          </TabsList>
        </Tabs>
      </div>
    </CardHeader>
    <CardContent class="flex flex-col gap-4">
      <Tabs :model-value="history.tab" @update:model-value="onTabChange">
        <TabsList>
          <TabsTrigger value="requests">Requests</TabsTrigger>
          <TabsTrigger value="tokens">Tokens</TabsTrigger>
          <TabsTrigger value="cost">Cost</TabsTrigger>
          <TabsTrigger value="models">Models</TabsTrigger>
        </TabsList>
      </Tabs>

      <p v-if="history.error" class="text-sm text-destructive">{{ history.error }}</p>
      <template v-if="history.tab === 'models'">
        <p class="text-sm text-muted-foreground">
          Models with usage in the selected window, ranked by
          {{ MODEL_METRIC_LABEL[history.modelMetric].toLowerCase() }}. Each row is the provider that served the
          traffic, so a model reached through more than one provider appears once per provider.
        </p>
        <ModelUsageChart :metric="history.modelMetric" :models="history.modelRanking" />
      </template>
      <UsageChart v-else :tab="timeSeriesTab" :window="history.window" :series-by-metric="history.seriesByMetric" />
    </CardContent>
  </Card>
</template>
