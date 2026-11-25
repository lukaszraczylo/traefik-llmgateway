<script setup lang="ts">
import type { Component } from 'vue'
import { computed, defineAsyncComponent, onMounted, watch } from 'vue'

import AuthGate from '@/components/AuthGate.vue'
import ConfigWarningsBanner from '@/components/ConfigWarningsBanner.vue'
import PageLoadError from '@/components/PageLoadError.vue'
import ToastViewport from '@/components/ToastViewport.vue'
import { useHashState } from '@/composables/useHashState'
import AppLayout from '@/layout/AppLayout.vue'
import type { PageId } from '@/lib/pages'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { useNavStore } from '@/stores/nav'
import { resetSessionData } from '@/stores/session'

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
function lazyPage(loader: () => Promise<Component>): Component {
  return defineAsyncComponent({ loader, errorComponent: PageLoadError, timeout: 30_000 })
}

// Legacy pages stand in until their redesigned replacements land: routers
// still lives inside the Models page and events inside Reliability.
const PAGES: Record<PageId, Component> = {
  overview: lazyPage(() => import('@/pages/OverviewPage.vue')),
  usage: lazyPage(() => import('@/pages/SpendPage.vue')),
  reliability: lazyPage(() => import('@/pages/ReliabilityPage.vue')),
  users: lazyPage(() => import('@/pages/ConsumersPage.vue')),
  models: lazyPage(() => import('@/pages/ModelsPage.vue')),
  routers: lazyPage(() => import('@/pages/ModelsPage.vue')),
  targets: lazyPage(() => import('@/pages/TargetsPage.vue')),
  events: lazyPage(() => import('@/pages/ReliabilityPage.vue')),
  config: lazyPage(() => import('@/pages/ConfigPage.vue')),
}

const currentPage = computed<Component>(() => PAGES[nav.page])

// A different key may carry different privileges, so never keep the previous key's data on screen.
watch(
  () => auth.apiKey,
  (next, prev) => {
    if (next !== prev) resetSessionData()
  },
)

onMounted(() => {
  dashboard.startPolling()
})
</script>

<template>
  <div v-if="!auth.isAuthenticated" class="px-4 py-6 sm:px-6 lg:px-8">
    <AuthGate />
  </div>
  <AppLayout v-else>
    <!-- GET /admin/api/overview's config warnings — renders nothing itself once `warnings` is empty, so this is always safe to mount. -->
    <ConfigWarningsBanner :warnings="dashboard.overview?.warnings ?? []" :warnings-dropped="dashboard.overview?.warningsDropped" />
    <component :is="currentPage" />
  </AppLayout>
  <ToastViewport />
</template>
