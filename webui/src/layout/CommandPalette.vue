<script setup lang="ts">
import { faArrowRightArrowLeft, faClock, faCircleHalfStroke } from '@fortawesome/free-solid-svg-icons'
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  Icon,
  useThemeStore,
} from '@perfect-design/ui'
import { computed, ref } from 'vue'

import { NAV_GROUPS } from '@/lib/nav-groups'
import { paletteEntities } from '@/lib/palette-items'
import { PANEL_PAGE } from '@/lib/panel-selection'
import type { PanelKind } from '@/lib/panel-selection'
import { RANGE_KEYS } from '@/lib/range'
import { useDashboardStore } from '@/stores/dashboard'
import { useFiltersStore } from '@/stores/filters'
import { useNavStore } from '@/stores/nav'

const open = defineModel<boolean>('open', { default: false })

const nav = useNavStore()
const filters = useFiltersStore()
const dashboard = useDashboardStore()
const theme = useThemeStore()

const search = ref('')
const pages = NAV_GROUPS.flatMap((g) => g.items)
const entities = computed(() => paletteEntities(dashboard.overview, dashboard.usage, dashboard.targets))

/** Runs an action, then closes the palette and resets the query. */
function run(action: () => void): void {
  action()
  open.value = false
  search.value = ''
}

function openEntity(kind: PanelKind, id: string): void {
  run(() => {
    nav.goTo(PANEL_PAGE[kind])
    nav.openPanel(kind, id)
  })
}
</script>

<template>
  <CommandDialog v-model:open="open" v-model:search="search" title="Command palette" description="Jump to a page or entity, or run an action.">
    <CommandInput placeholder="Search pages, entities and actions" />
    <CommandList>
      <CommandEmpty>No matches.</CommandEmpty>
      <CommandGroup heading="Pages">
        <CommandItem v-for="p in pages" :key="p.id" :value="`page ${p.label}`" @select="run(() => nav.goTo(p.id))">
          <Icon :icon="p.icon" size="sm" />
          {{ p.label }}
        </CommandItem>
      </CommandGroup>
      <CommandGroup heading="Range">
        <CommandItem
          v-for="r in RANGE_KEYS"
          :key="r"
          :value="`range ${r}`"
          @select="run(() => filters.setFilters({ range: r }))"
        >
          <Icon :icon="faClock" size="sm" />
          Range {{ r }}
        </CommandItem>
      </CommandGroup>
      <CommandGroup heading="Actions">
        <CommandItem value="action toggle comparison previous period" @select="run(() => filters.toggleCmp())">
          <Icon :icon="faArrowRightArrowLeft" size="sm" />
          Toggle comparison with previous period
        </CommandItem>
        <CommandItem
          v-for="m in (['light', 'dark', 'system'] as const)"
          :key="m"
          :value="`action theme ${m}`"
          @select="run(() => theme.setMode(m))"
        >
          <Icon :icon="faCircleHalfStroke" size="sm" />
          Theme: {{ m }}
        </CommandItem>
      </CommandGroup>
      <CommandGroup v-if="entities.length" heading="Entities">
        <CommandItem
          v-for="e in entities"
          :key="`${e.kind}:${e.id}`"
          :value="`${e.kind} ${e.label}`"
          @select="openEntity(e.kind, e.id)"
        >
          <span class="text-muted-foreground w-16 shrink-0 text-xs">{{ e.kind }}</span>
          <span class="truncate">{{ e.label }}</span>
        </CommandItem>
      </CommandGroup>
    </CommandList>
  </CommandDialog>
</template>
