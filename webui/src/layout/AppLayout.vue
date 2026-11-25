<script setup lang="ts">
import { AppShell, SidebarNav, Toaster } from '@perfect-design/ui'
import type { SidebarNavGroup } from '@perfect-design/ui'
import { useEventListener } from '@vueuse/core'
import { computed, ref } from 'vue'

import AppHeader from '@/layout/AppHeader.vue'
import CommandPalette from '@/layout/CommandPalette.vue'
import { NAV_GROUPS, navCounts } from '@/lib/nav-groups'
import type { PageId } from '@/lib/pages'
import { useDashboardStore } from '@/stores/dashboard'
import { useNavStore } from '@/stores/nav'

const nav = useNavStore()
const dashboard = useDashboardStore()

const collapsed = ref(false)
const open = ref(false)
const paletteOpen = ref(false)

const counts = computed(() => navCounts(dashboard.overview, dashboard.usage, dashboard.targets))

const groups = computed<SidebarNavGroup[]>(() =>
  NAV_GROUPS.map((g, i) => ({
    id: `group-${i}`,
    label: g.label ?? undefined,
    items: g.items.map((item) => ({
      id: item.id,
      label: item.label,
      icon: item.icon,
      active: nav.page === item.id,
      badge: item.countKey ? counts.value[item.countKey] : undefined,
    })),
  })),
)

useEventListener(window, 'keydown', (e: KeyboardEvent) => {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault()
    paletteOpen.value = !paletteOpen.value
  }
})

function onNavigate(item: { id?: string }, event: MouseEvent): void {
  event.preventDefault()
  if (item.id) nav.goTo(item.id as PageId)
  open.value = false
}
</script>

<template>
  <AppShell v-model:collapsed="collapsed" v-model:open="open" drawer-breakpoint="lg" sidebar-label="Primary">
    <template #header="{ toggle, triggerProps }">
      <AppHeader :trigger-props="triggerProps" @toggle-sidebar="toggle" @open-palette="paletteOpen = true" />
    </template>
    <template #sidebar>
      <SidebarNav v-model:collapsed="collapsed" :groups="groups" :current="nav.page" label="Primary" @navigate="onNavigate" />
    </template>
    <div class="min-w-0 p-4">
      <slot />
    </div>
  </AppShell>
  <CommandPalette v-model:open="paletteOpen" />
  <Toaster />
</template>
