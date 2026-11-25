<script setup lang="ts">
import { Button, Card, CardAction, CardContent, CardHeader, CardTitle, FilterBar, StatusDot } from '@perfect-design/ui'
import type { FilterField, FilterItem } from '@perfect-design/ui'
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'

import { EVENT_KIND_LABEL, EVENT_KINDS, eventFilterFromParams, filterEvents, itemsFromFilter, paramsFromItems } from '@/lib/events-filter'
import { loadState } from '@/lib/load-state'
import { EVENTS_POLL_MS, useEventsStore } from '@/stores/events'
import { useNavStore } from '@/stores/nav'
import RecentEvents from '@/widgets/RecentEvents.vue'
import StateView from '@/widgets/StateView.vue'
import type { ViewState } from '@/widgets/StateView.vue'

const PAGE_STEP = 50
const POLL_SECONDS = EVENTS_POLL_MS / 1000

const events = useEventsStore()
const nav = useNavStore()

// --- Filters (all state lives in the hash: kind, user, provider, route) ---
const filter = computed(() => eventFilterFromParams(nav.params))
const items = computed<FilterItem[]>(() => itemsFromFilter(filter.value))

const providerOptions = computed(() => {
  const names = new Set(events.events.flatMap((e) => (e.provider ? [e.provider] : [])))
  if (filter.value.provider) names.add(filter.value.provider)
  return [...names].sort().map((value) => ({ value, label: value }))
})
const fields = computed<FilterField[]>(() => [
  { id: 'kind', label: 'Kind', type: 'select', operators: ['eq'], options: EVENT_KINDS.map((k) => ({ value: k, label: EVENT_KIND_LABEL[k] })) },
  { id: 'user', label: 'User or group', type: 'text', operators: ['contains'], placeholder: 'name' },
  // The select needs at least one option; until events name a provider it is a plain text field.
  { id: 'provider', label: 'Provider', type: providerOptions.value.length > 0 ? 'select' : 'text', operators: ['eq'], options: providerOptions.value },
  { id: 'route', label: 'Route', type: 'text', operators: ['contains'], placeholder: 'chat/completions' },
])

function setItems(next: FilterItem[]): void {
  const keep = Object.fromEntries(Object.entries(nav.params).filter(([k]) => !(k in filter.value)))
  nav.goTo('events', { ...keep, ...paramsFromItems(next) })
}

const filtered = computed(() => filterEvents(events.events, filter.value))
const shown = ref(PAGE_STEP)
watch(filter, () => (shown.value = PAGE_STEP))
const visible = computed(() => filtered.value.slice(0, shown.value))
const remaining = computed(() => filtered.value.length - visible.value.length)

// --- Live tail: the store polls while mounted and not paused ---
const live = ref(true)
function toggleLive(): void {
  live.value = !live.value
}
watch(live, (on) => (on ? events.startPolling() : events.stopPolling()))
onMounted(() => events.startPolling())
onUnmounted(() => events.stopPolling())

const state = computed<ViewState>(() => {
  const s = loadState({ loading: events.refreshing, hasData: events.lastUpdated !== null, error: events.error })
  if (s === 'skeleton') return 'loading'
  if (s === 'error') return 'error'
  return filtered.value.length === 0 ? 'empty' : 'ready'
})
const filterActive = computed(() => items.value.length > 0)
const emptyTitle = computed(() => (filterActive.value && events.events.length > 0 ? 'No events match the filter' : 'No events recorded'))
const emptyDescription = computed(() =>
  filterActive.value && events.events.length > 0 ? 'Change or clear a filter to see more of the last events.' : 'Rate-limit, budget and upstream events show up here as they happen.',
)
const sourceNote = computed(() => {
  if (events.lastUpdated === null) return ''
  return events.source === 'redis' ? 'All replicas' : `This replica only (${events.replica || 'unknown'})`
})
const countNote = computed(() => (filtered.value.length === events.events.length ? `${events.events.length.toLocaleString('en-US')} events` : `${filtered.value.length.toLocaleString('en-US')} of ${events.events.length.toLocaleString('en-US')} events`))
</script>

<template>
  <div class="flex flex-col gap-4">
    <Card size="sm">
      <CardHeader class="items-center">
        <CardTitle>Events</CardTitle>
        <CardAction class="flex items-center gap-2 self-center">
          <span class="text-muted-foreground flex items-center gap-1.5 text-xs" data-testid="events-live-status" role="status">
            <StatusDot :tone="live ? 'success' : 'neutral'" size="md" />
            {{ live ? `Live, every ${POLL_SECONDS}s` : 'Paused' }}
          </span>
          <Button variant="outline" size="sm" :aria-pressed="!live" @click="toggleLive">{{ live ? 'Pause' : 'Resume' }}</Button>
        </CardAction>
      </CardHeader>
      <CardContent class="flex flex-col gap-2">
        <p v-if="events.degraded" role="note" class="border-border bg-card text-muted-foreground m-0 rounded-md border px-3 py-2 text-sm">
          Redis could not be read. Showing this replica's own events, which may miss events from other replicas.
        </p>
        <FilterBar :model-value="items" :fields="fields" variant="compact" size="sm" clearable @update:model-value="setItems" />
        <div v-if="events.lastUpdated !== null" class="text-muted-foreground flex flex-wrap items-center justify-between gap-2 text-xs">
          <span>{{ countNote }}</span>
          <span>{{ sourceNote }}</span>
        </div>
        <StateView :state="state" :empty-title="emptyTitle" :empty-description="emptyDescription" :error-message="events.error" :updated-at="events.lastUpdated ?? undefined" @retry="events.refresh()">
          <RecentEvents :events="visible" label="Events" with-message />
          <div v-if="remaining > 0" class="flex items-center justify-between gap-2">
            <span class="text-muted-foreground text-xs">Showing {{ visible.length.toLocaleString('en-US') }} of {{ filtered.length.toLocaleString('en-US') }}</span>
            <Button variant="outline" size="sm" @click="shown += PAGE_STEP">Show {{ Math.min(PAGE_STEP, remaining) }} more</Button>
          </div>
        </StateView>
      </CardContent>
    </Card>
  </div>
</template>
