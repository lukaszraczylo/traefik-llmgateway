<script setup lang="ts">
import { faGithub } from '@fortawesome/free-brands-svg-icons'
import { faCircleExclamation } from '@fortawesome/free-solid-svg-icons'
import { computed, defineAsyncComponent, onMounted } from 'vue'

import AuthGate from '@/components/AuthGate.vue'
import ConfigWarningsBanner from '@/components/ConfigWarningsBanner.vue'
import EventsView from '@/components/EventsView.vue'
import ProvidersView from '@/components/ProvidersView.vue'
import TargetsView from '@/components/TargetsView.vue'
import { Button } from '@/components/ui/button'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import UsageView from '@/components/UsageView.vue'
import { useHashState } from '@/composables/useHashState'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { useNavStore } from '@/stores/nav'

// Lazy: Chart.js (UsageChart.vue's dependency, pulled in transitively) is
// the single largest piece of this bundle, and most admin sessions open on
// Providers/Usage. Splitting it into its own chunk keeps the initial load
// lean; it fetches once, on first visit to the Charts tab.
const ChartsView = defineAsyncComponent(() => import('@/components/ChartsView.vue'))

/** The module path (go.mod / .traefik.yml `import:`) — same repo the plugin
 * ships from, so this is the one canonical URL rather than a guess. */
const GITHUB_REPO_URL = 'https://github.com/lukaszraczylo/traefik-llmgateway'

const auth = useAuthStore()
const dashboard = useDashboardStore()
const nav = useNavStore()

// The "Providers" tab is labeled that way (operator rename); the backend
// endpoint it fetches from, GET /admin/api/overview, and the store's own
// `overview` field (AdminOverviewResponse) keep their original names
// unchanged — the rename is UI-only, not an API surface change.

// F9 (dashboard-plan.md) supersedes Feature C (v0.22)'s useTabHash()
// composable: the Tabs v-model now binds nav.activeTab (stores/nav.ts)
// directly, and useHashState() keeps that field — plus history.ts's
// scope/window/tab/modelMetric, events.ts's kindFilter/userFilter, and
// nav.usageQuery — synced with a single "#<tab>?<query>" URL hash
// (lib/hash-state.ts), both on load and on every change. MUST be created
// here, before the `v-if="!auth.isAuthenticated"` branch below (Risks,
// dashboard-plan.md: "load-with-hash-then-authenticate") — every store
// action it calls is safe to run before a key is stored, so the hash's
// selection is already restored by the time the reader authenticates.
useHashState()

// statusText reads dashboard.error/lastUpdated unconditionally — it is
// correct only while the template's own `v-if="auth.isAuthenticated"` below
// keeps it off-screen otherwise. Without that gate a 401 (which resolves
// before touching error/lastUpdated) left the PRIOR "last updated ..." or
// "refresh failed: ..." line showing right under the auth gate, and a first
// load with no stored key showed "loading..." forever (review finding).
// F4 (dashboard-plan.md): appended only on a genuinely successful poll —
// dashboard.overview.replica reflects WHICH replica answered that poll,
// so it must never be shown alongside a stale value during a failed or
// still-loading refresh (a prior successful poll's overview object can
// still be sitting there while `error` is set, and showing its replica
// next to "refresh failed" would misattribute the failure to it).
const statusText = computed<string>(() => {
  if (dashboard.error) return `refresh failed: ${dashboard.error}`
  if (dashboard.lastUpdated) {
    const replica = dashboard.overview?.replica
    return `last updated ${dashboard.lastUpdated.toLocaleTimeString()}${replica ? ` · replica ${replica}` : ''}`
  }
  return 'loading...'
})

onMounted(() => {
  dashboard.startPolling()
})
</script>

<template>
  <div class="flex w-full flex-col gap-6 px-4 py-8 sm:px-6 lg:px-8">
    <header class="flex flex-col gap-1">
      <div class="flex items-center justify-between gap-2">
        <h1 class="text-xl font-semibold tracking-tight">LLM Gateway &mdash; Admin</h1>
        <Button
          as="a"
          :href="GITHUB_REPO_URL"
          target="_blank"
          rel="noopener noreferrer"
          variant="ghost"
          size="icon"
          aria-label="GitHub repository"
          class="text-muted-foreground hover:text-foreground"
        >
          <FontAwesomeIcon :icon="faGithub" class="size-4" aria-hidden="true" />
        </Button>
      </div>
      <p v-if="auth.isAuthenticated" class="flex items-center gap-1.5 text-sm text-muted-foreground">
        <FontAwesomeIcon v-if="dashboard.error" :icon="faCircleExclamation" class="size-3.5 text-destructive" aria-hidden="true" />
        <span :class="dashboard.error ? 'text-destructive' : undefined">{{ statusText }}</span>
      </p>
    </header>

    <!-- F10: GET /admin/api/overview's config warnings — renders nothing itself once `warnings` is empty, so this is always safe to mount. -->
    <ConfigWarningsBanner
      v-if="auth.isAuthenticated"
      :warnings="dashboard.overview?.warnings ?? []"
      :warnings-dropped="dashboard.overview?.warningsDropped"
    />

    <AuthGate v-if="!auth.isAuthenticated" />

    <Tabs v-else v-model="nav.activeTab">
      <TabsList>
        <TabsTrigger value="providers">Providers</TabsTrigger>
        <TabsTrigger value="usage">Usage</TabsTrigger>
        <TabsTrigger value="charts">Charts</TabsTrigger>
        <TabsTrigger value="targets">MCP & Agents</TabsTrigger>
        <TabsTrigger value="events">Events</TabsTrigger>
      </TabsList>
      <TabsContent value="providers">
        <ProvidersView />
      </TabsContent>
      <TabsContent value="usage">
        <UsageView />
      </TabsContent>
      <TabsContent value="charts">
        <ChartsView />
      </TabsContent>
      <TabsContent value="targets">
        <TargetsView />
      </TabsContent>
      <TabsContent value="events">
        <EventsView />
      </TabsContent>
    </Tabs>
  </div>
</template>
