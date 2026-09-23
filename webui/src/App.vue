<script setup lang="ts">
import type { Component } from 'vue'
import { faGithub } from '@fortawesome/free-brands-svg-icons'
import { faCircleExclamation } from '@fortawesome/free-solid-svg-icons'
import { computed, defineAsyncComponent, onMounted } from 'vue'

import AuthGate from '@/components/AuthGate.vue'
import ConfigWarningsBanner from '@/components/ConfigWarningsBanner.vue'
import GlobalFilterBar from '@/components/GlobalFilterBar.vue'
import SidebarNav from '@/components/SidebarNav.vue'
import ToastViewport from '@/components/ToastViewport.vue'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { useHashState } from '@/composables/useHashState'
import type { PageId } from '@/lib/pages'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { useNavStore } from '@/stores/nav'

/** The module path (go.mod / .traefik.yml `import:`) — same repo the plugin ships from, so this is the one canonical URL rather than a guess. */
const GITHUB_REPO_URL = 'https://github.com/lukaszraczylo/traefik-llmgateway'

const auth = useAuthStore()
const dashboard = useDashboardStore()
const nav = useNavStore()

// The old flat-tab shell (activeTab/useTabHash) is superseded by the
// sidebar-page redesign (redesign-plan.md section 3.1): nav.page/params
// now drive routing, and useHashState() keeps them — plus the global
// filters (stores/filters.ts) — synced with a single
// "#<page>?range=...&cmp=...&scope=...&<page params>" URL hash. MUST be
// created here, before the `v-if="!auth.isAuthenticated"` branch below
// (load-with-hash-then-authenticate) — every store write it makes is
// safe to run before a key is stored, so the hash's selection is already
// restored by the time the reader authenticates.
useHashState()

/**
 * PAGES maps each PageId (lib/pages.ts) to its own top-level page
 * component, lazily loaded — an admin session only ever looks at one
 * page at a time, so only that page's own code (and, for the pages
 * other work packages are filling in with chart.js-backed views) ships
 * on first load, generalizing the same reasoning the old shell already
 * applied to its one chart-heavy tab.
 */
const PAGES: Record<PageId, Component> = {
  home: defineAsyncComponent(() => import('@/pages/HomePage.vue')),
  spend: defineAsyncComponent(() => import('@/pages/SpendPage.vue')),
  consumers: defineAsyncComponent(() => import('@/pages/ConsumersPage.vue')),
  models: defineAsyncComponent(() => import('@/pages/ModelsPage.vue')),
  reliability: defineAsyncComponent(() => import('@/pages/ReliabilityPage.vue')),
  config: defineAsyncComponent(() => import('@/pages/ConfigPage.vue')),
  targets: defineAsyncComponent(() => import('@/pages/TargetsPage.vue')),
}

const currentPage = computed<Component>(() => PAGES[nav.page])

// statusText reads dashboard.error/lastUpdated unconditionally — it is
// correct only while the template's own `v-if="auth.isAuthenticated"`
// below keeps it off-screen otherwise, so a first load with no stored
// key never shows a stale "refresh failed" line under the auth gate.
// Appended only on a genuinely successful poll — dashboard.overview.
// replica reflects WHICH replica answered that poll, so it must never be
// shown alongside a stale value during a failed or still-loading refresh.
// The unreachable 'loading...' fallback (verify-ui-states.md #5 fix) was
// dead code: isInitialStatusLoad below covers exactly
// `!dashboard.error && !dashboard.lastUpdated`, the ONLY state in which
// neither of this function's two `if` branches fires, and the template
// renders the Skeleton for that state instead of ever reading statusText.
const statusText = computed<string>(() => {
  if (dashboard.error) return `refresh failed: ${dashboard.error}`
  const replica = dashboard.overview?.replica
  const updated = dashboard.lastUpdated?.toLocaleTimeString() ?? ''
  return `last updated ${updated}${replica ? ` · replica ${replica}` : ''}`
})

/** isInitialStatusLoad gates the header status line's skeleton — the FIRST dashboard poll only (states-plan.md item 1: a skeleton only when there is no data yet, never on a background refresh — dashboard.lastUpdated stays set through every later poll, success or failure). */
const isInitialStatusLoad = computed<boolean>(() => !dashboard.error && !dashboard.lastUpdated)

onMounted(() => {
  dashboard.startPolling()
})
</script>

<template>
  <div class="flex w-full flex-col gap-4 px-4 py-6 sm:px-6 lg:px-8">
    <header class="flex flex-col gap-3">
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
      <!--
        No role="status" here (verify-ui-states.md #5 fix) — this line
        updates every 5s (dashboard.ts's POLL_MS), and a live region
        re-announces its FULL text on every single change; a screen
        reader would have read "last updated HH:MM:SS" once every 5
        seconds for as long as the session stayed open. A background
        refresh FAILURE (the one change actually worth announcing) is
        already a "meaningful change" a screen reader hears from
        elsewhere: stores/dashboard.ts's own toast (states-plan.md item 3,
        "background refresh failures when data is already shown") fires
        exactly once per failure streak, through ToastViewport's own
        assertive live region — a second, duplicate announcement here
        would only repeat it.
      -->
      <p v-if="auth.isAuthenticated" class="flex items-center gap-1.5 text-sm text-muted-foreground">
        <template v-if="isInitialStatusLoad">
          <Skeleton class="h-4 w-40" />
          <span class="sr-only">Loading dashboard status…</span>
        </template>
        <template v-else>
          <FontAwesomeIcon v-if="dashboard.error" :icon="faCircleExclamation" class="size-3.5 text-destructive" aria-hidden="true" />
          <span :class="dashboard.error ? 'text-destructive' : undefined">{{ statusText }}</span>
        </template>
      </p>
      <GlobalFilterBar v-if="auth.isAuthenticated" />
    </header>

    <!-- GET /admin/api/overview's config warnings — renders nothing itself once `warnings` is empty, so this is always safe to mount. -->
    <ConfigWarningsBanner
      v-if="auth.isAuthenticated"
      :warnings="dashboard.overview?.warnings ?? []"
      :warnings-dropped="dashboard.overview?.warningsDropped"
    />

    <AuthGate v-if="!auth.isAuthenticated" />

    <div v-else class="flex flex-col gap-4 md:flex-row">
      <SidebarNav />
      <main class="min-w-0 flex-1">
        <component :is="currentPage" />
      </main>
    </div>
  </div>
  <ToastViewport />
</template>
