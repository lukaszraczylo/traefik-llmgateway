<script setup lang="ts">
import { faCircleExclamation } from '@fortawesome/free-solid-svg-icons'
import { computed, defineAsyncComponent, onMounted, ref } from 'vue'

import AuthGate from '@/components/AuthGate.vue'
import OverviewView from '@/components/OverviewView.vue'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import UsageView from '@/components/UsageView.vue'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'

// Lazy: Chart.js (UsageChart.vue's dependency, pulled in transitively) is
// the single largest piece of this bundle, and most admin sessions open on
// Overview/Usage. Splitting it into its own chunk keeps the initial load
// lean; it fetches once, on first visit to the Charts tab.
const ChartsView = defineAsyncComponent(() => import('@/components/ChartsView.vue'))

const auth = useAuthStore()
const dashboard = useDashboardStore()

const activeView = ref<'overview' | 'usage' | 'charts'>('overview')

const statusText = computed<string>(() => {
  if (dashboard.error) return `refresh failed: ${dashboard.error}`
  if (dashboard.lastUpdated) return `last updated ${dashboard.lastUpdated.toLocaleTimeString()}`
  return 'loading...'
})

onMounted(() => {
  dashboard.startPolling()
})
</script>

<template>
  <div class="flex w-full flex-col gap-6 px-4 py-8 sm:px-6 lg:px-8">
    <header class="flex flex-col gap-1">
      <h1 class="text-xl font-semibold tracking-tight">LLM Gateway &mdash; Admin</h1>
      <p class="flex items-center gap-1.5 text-sm text-muted-foreground">
        <FontAwesomeIcon v-if="dashboard.error" :icon="faCircleExclamation" class="size-3.5 text-destructive" />
        <span :class="dashboard.error ? 'text-destructive' : undefined">{{ statusText }}</span>
      </p>
    </header>

    <AuthGate v-if="!auth.isAuthenticated" />

    <Tabs v-else v-model="activeView">
      <TabsList>
        <TabsTrigger value="overview">Overview</TabsTrigger>
        <TabsTrigger value="usage">Usage</TabsTrigger>
        <TabsTrigger value="charts">Charts</TabsTrigger>
      </TabsList>
      <TabsContent value="overview">
        <OverviewView />
      </TabsContent>
      <TabsContent value="usage">
        <UsageView />
      </TabsContent>
      <TabsContent value="charts">
        <ChartsView />
      </TabsContent>
    </Tabs>
  </div>
</template>
