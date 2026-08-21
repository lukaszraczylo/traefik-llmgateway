<script setup lang="ts">
import { computed, onMounted, onUnmounted, watch } from 'vue'

import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import UsageChart from '@/components/UsageChart.vue'
import { useDashboardStore } from '@/stores/dashboard'
import { type ChartTab, useHistoryStore, WINDOW_LABEL } from '@/stores/history'
import type { HistoryWindow } from '@/types/api'

const dashboard = useDashboardStore()
const history = useHistoryStore()

const scopeOptions = computed(() => {
  const users = (dashboard.usage?.users ?? []).map((u) => ({ value: `user:${u.id}`, label: u.id }))
  const groups = (dashboard.usage?.groups ?? []).map((g) => ({ value: `group:${g.id}`, label: g.id }))
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
