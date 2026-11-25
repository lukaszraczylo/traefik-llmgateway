<script setup lang="ts">
import { faArrowRightArrowLeft, faBars, faMagnifyingGlass } from '@fortawesome/free-solid-svg-icons'
import { Button, Icon, Input, Kbd, OfflineIndicator, SegmentedControl, StaleNotice, ThemeToggle } from '@perfect-design/ui'
import { computed, ref, watch } from 'vue'

import { SCOPE_PATTERN } from '@/lib/hash-state'
import { RANGE_KEYS } from '@/lib/range'
import type { RangeKey } from '@/lib/range'
import { useDashboardStore } from '@/stores/dashboard'
import { DEFAULT_SCOPE, useFiltersStore } from '@/stores/filters'

defineProps<{ triggerProps: Record<string, string> }>()
const emit = defineEmits<{ toggleSidebar: []; openPalette: [] }>()

const RANGE_OPTIONS = RANGE_KEYS.map((key) => ({ value: key, label: key }))
/** Matches the dashboard store's 5s poll: data older than this means polling has stalled. */
const STALE_AFTER_MS = 30_000

const filters = useFiltersStore()
const dashboard = useDashboardStore()

const cmpDisabled = computed<boolean>(() => filters.cmp !== 'prev' && !filters.cmpSpec.available)
const cmpTitle = computed<string>(() => {
  if (filters.cmp === 'prev' && !filters.cmpSpec.available) {
    return `Comparison unavailable for this range (${filters.cmpSpec.reason}) — click to turn off`
  }
  if (filters.cmpSpec.available) return 'Compare against the previous equal-length period'
  return `Comparison unavailable for this range: ${filters.cmpSpec.reason}`
})

// A draft buffer: a half-typed scope must not refetch every store per keystroke, and an invalid commit reverts the field only.
const scopeDraft = ref(filters.scope)
watch(
  () => filters.scope,
  (scope) => {
    scopeDraft.value = scope
  },
)

function commitScope(): void {
  const value = String(scopeDraft.value).trim() || DEFAULT_SCOPE
  if (SCOPE_PATTERN.test(value)) filters.setFilters({ scope: value })
  else scopeDraft.value = filters.scope
}

function onRange(value: string): void {
  filters.setFilters({ range: value as RangeKey })
}
</script>

<template>
  <div class="flex w-full flex-wrap items-center gap-x-3 gap-y-2 px-3 py-2">
    <Button variant="ghost" size="sm" class="lg:hidden" aria-label="Toggle navigation" v-bind="triggerProps" @click="emit('toggleSidebar')">
      <Icon :icon="faBars" size="sm" />
    </Button>
    <span class="text-sm font-semibold tracking-tight">LLM Gateway</span>

    <div class="max-w-full overflow-x-auto">
      <SegmentedControl :model-value="filters.range" :options="RANGE_OPTIONS" size="sm" aria-label="Time range" @update:model-value="onRange" />
    </div>

    <Button
      variant="outline"
      size="sm"
      :aria-pressed="filters.cmp === 'prev'"
      :disabled="cmpDisabled"
      :title="cmpTitle"
      @click="filters.toggleCmp()"
    >
      <Icon :icon="faArrowRightArrowLeft" size="sm" />
      vs previous
    </Button>

    <Input
      v-model="scopeDraft"
      size="sm"
      class="w-40"
      placeholder="scope: all"
      aria-label="Scope"
      title="all, group:<name>, user:<name>, or provider:<name>"
      @keyup.enter="commitScope"
      @blur="commitScope"
    />

    <div class="ml-auto flex items-center gap-2">
      <StaleNotice
        v-if="dashboard.lastUpdated"
        variant="inline"
        :timestamp="dashboard.lastUpdated"
        :stale-after="STALE_AFTER_MS"
        :refreshing="dashboard.refreshing"
        refreshable
        hide-when-fresh
        @refresh="dashboard.refresh()"
      />
      <OfflineIndicator variant="pill" />
      <span v-if="dashboard.error" class="text-destructive text-xs" role="status">refresh failed</span>
      <Button variant="outline" size="sm" aria-label="Search ⌘K" @click="emit('openPalette')">
        <Icon :icon="faMagnifyingGlass" size="sm" />
        <span class="hidden sm:inline">Search</span>
        <Kbd>⌘K</Kbd>
      </Button>
      <ThemeToggle />
      <span v-if="dashboard.overview?.version" class="text-muted-foreground hidden text-xs md:inline">
        v{{ dashboard.overview.version }}
      </span>
    </div>
  </div>
</template>
