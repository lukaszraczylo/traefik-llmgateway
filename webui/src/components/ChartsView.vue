<script setup lang="ts">
import { faXmark } from '@fortawesome/free-solid-svg-icons'
import { computed, onMounted, onUnmounted, watch } from 'vue'

import SearchInput from '@/components/SearchInput.vue'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import CsvExportButton from '@/components/CsvExportButton.vue'
import ModelUsageChart from '@/components/ModelUsageChart.vue'
import UsageChart from '@/components/UsageChart.vue'
import { useSearchQuery } from '@/composables/useSearchQuery'
import { filterModelsByPrefix } from '@/lib/model-filter'
import { modelRankingCsv } from '@/lib/usage-csv'
import { groupMatches, userMatches } from '@/lib/usage-search'
import { useDashboardStore } from '@/stores/dashboard'
import {
  type ChartTab,
  MODEL_METRIC_LABEL,
  MODEL_RANKING_LIMIT,
  type ModelMetric,
  type TimeSeriesTab,
  useHistoryStore,
  WINDOW_LABEL,
  WINDOW_SPAN,
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

// --- Models-tab prefix filter (F6, dashboard-plan.md) ---
//
// history.modelFilter is set by ProvidersView.vue's provider-header link
// (nav.goToModels(`${p.name}/`), stores/nav.ts — WP-B1) or restored from
// the `#charts?tab=models&filter=...` hash (lib/hash-state.ts). P3 review
// fix: setModelFilter now also sends the prefix to the SERVER and
// refetches (stores/history.ts's own state doc comment on modelFilter),
// so this re-slice is a harmless second pass over an already-narrowed
// ranking, not the only filtering step — see lib/model-filter.ts's own
// doc comment.
const filteredModelRanking = computed(() => filterModelsByPrefix(history.modelRanking, history.modelFilter))

/**
 * Exports exactly the ranking rows on screen (filter applied, server order
 * kept). P6 review fix: passes the resolved DISPLAY label constants
 * (MODEL_METRIC_LABEL/WINDOW_LABEL), not the raw enum values
 * ("cost"/"hour") modelRankingCsv's own doc comment always said the caller
 * should — the header used to read the literal "cost (hour, span 24)"
 * with no stated unit. `isCostMetric` states whether the CURRENTLY ranked
 * metric is cost, so modelRankingCsv can convert the raw micro-USD `value`
 * to a plain decimal USD number and name the unit in the header.
 */
function modelsCsv(): string {
  return modelRankingCsv(
    filteredModelRanking.value,
    MODEL_METRIC_LABEL[history.modelMetric],
    WINDOW_LABEL[history.window],
    WINDOW_SPAN[history.window],
    history.modelMetric === 'cost',
  )
}

function clearModelFilter(): void {
  history.setModelFilter('')
}

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
  // The picker's model list is fetched here (initial load) and again on
  // any window change (history.ts's own setWindow) — nowhere else (Q1,
  // dashboard-plan.md DECISIONS). The selection's own series/ranking
  // fetch no longer needs a separate call here: startAutoRefresh's poller
  // (lib/polling.ts) ticks once immediately on start, which is that
  // initial fetch.
  void history.fetchModelOptions()
  history.startAutoRefresh()
})
onUnmounted(() => {
  history.stopAutoRefresh()
})

// The scope list is only known once the Usage response has loaded — if the
// previously-selected scope disappears (a user/group config change), fall
// back to the total scope rather than requesting a now-unknown id. This is
// ALSO what guards a bogus scope restored from a hand-edited URL hash (F9,
// dashboard-plan.md: lib/hash-state.ts's parseChartsParams only validates
// SHAPE — "total"/"user:.+"/"group:.+"/"model:.+" — never that the id
// actually exists): `watch` without `{ immediate: true }` skips its first
// evaluation, so a hash-restored scope like "user:ghost" survives the
// (likely still-empty, pre-fetch) initial scopeOptions and is only judged
// once dashboard.usage genuinely loads and this computed's value actually
// changes — at which point a truly nonexistent id resets to 'total' here,
// exactly like any other vanished scope.
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
        <!-- The Models tab has no scope picker to filter (its own Select branch below is the ranking-metric picker instead) — a visible search box that silently does nothing is worse than no box at all. -->
        <SearchInput
          v-if="history.tab !== 'models'"
          v-model="scopeQuery"
          placeholder="Filter users, groups or models"
          class="w-56"
        />

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
      <!-- Only until the current selection's first fetch lands (history.loaded) — background refreshes never flash it over rendered data. -->
      <p v-else-if="history.loading && !history.loaded" class="text-sm text-muted-foreground">Loading…</p>
      <template v-if="history.tab === 'models'">
        <p class="text-sm text-muted-foreground">
          Models with usage over the last {{ WINDOW_LABEL[history.window] }}, ranked by
          {{ MODEL_METRIC_LABEL[history.modelMetric].toLowerCase() }}. Each row is the provider that served the
          traffic, so a model reached through more than one provider appears once per provider.
        </p>
        <p v-if="history.modelFilter" class="flex items-center gap-1.5 text-sm">
          <span class="text-muted-foreground">Filtered to</span>
          <span class="font-mono text-xs">{{ history.modelFilter }}</span>
          <Button variant="ghost" size="icon-xs" aria-label="Clear model filter" @click="clearModelFilter">
            <FontAwesomeIcon :icon="faXmark" class="size-3.5" aria-hidden="true" />
          </Button>
        </p>
        <!-- P3: the ranking is a top-N (MODEL_RANKING_LIMIT), never the complete set — say so whenever the fetched ranking is exactly that long, so a full list doesn't silently read as "this is everything". -->
        <p v-if="history.modelRanking.length === MODEL_RANKING_LIMIT" class="text-xs text-muted-foreground">
          Top {{ MODEL_RANKING_LIMIT }} models
        </p>
        <ModelUsageChart :metric="history.modelMetric" :models="filteredModelRanking" />
        <div class="flex justify-end">
          <CsvExportButton :filename="`usage-models-${history.window}.csv`" :build="modelsCsv" />
        </div>
      </template>
      <UsageChart v-else :tab="timeSeriesTab" :window="history.window" :series-by-metric="history.seriesByMetric" />
    </CardContent>
  </Card>
</template>
