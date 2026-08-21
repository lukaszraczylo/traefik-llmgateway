<script setup lang="ts">
import { faMagnifyingGlass, faXmark } from '@fortawesome/free-solid-svg-icons'
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'

import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import UsageChart from '@/components/UsageChart.vue'
import { groupMatches, userMatches } from '@/lib/usage-search'
import { useDashboardStore } from '@/stores/dashboard'
import { type ChartTab, useHistoryStore, WINDOW_LABEL } from '@/stores/history'
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
// Same matching helpers as UsageView.vue (lib/usage-search.ts) — one
// implementation, no second copy.
const scopeQuery = ref('')
const normalizedScopeQuery = computed(() => scopeQuery.value.trim().toLowerCase())
const hasScopeQuery = computed(() => normalizedScopeQuery.value.length > 0)

function clearScopeQuery(): void {
  scopeQuery.value = ''
}

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
  return [{ value: 'total', label: 'Total (all traffic)' }, ...groups, ...users]
})

const windows: HistoryWindow[] = ['hour', 'day', 'month']

function onTabChange(value: string | number): void {
  history.setTab(value as ChartTab)
}
function onScopeChange(value: unknown): void {
  if (typeof value === 'string') history.setScope(value)
}

onMounted(() => {
  void history.fetchSeries()
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
        <div class="relative w-48">
          <FontAwesomeIcon
            :icon="faMagnifyingGlass"
            class="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground"
            aria-hidden="true"
          />
          <Input
            v-model="scopeQuery"
            type="text"
            placeholder="Filter users or groups"
            aria-label="Filter users or groups"
            class="pr-8 pl-8"
          />
          <button
            v-if="hasScopeQuery"
            type="button"
            aria-label="Clear search"
            class="absolute top-1/2 right-2 -translate-y-1/2 rounded text-muted-foreground hover:text-foreground"
            @click="clearScopeQuery"
          >
            <FontAwesomeIcon :icon="faXmark" class="size-3.5" />
          </button>
        </div>

        <Select :model-value="history.scope" @update:model-value="onScopeChange">
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
        </TabsList>
      </Tabs>

      <p v-if="history.error" class="text-sm text-destructive">{{ history.error }}</p>
      <UsageChart :tab="history.tab" :window="history.window" :series-by-metric="history.seriesByMetric" />
    </CardContent>
  </Card>
</template>
