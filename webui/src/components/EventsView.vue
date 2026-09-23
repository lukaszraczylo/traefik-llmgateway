<script setup lang="ts">
import { faTriangleExclamation } from '@fortawesome/free-solid-svg-icons'
import { storeToRefs } from 'pinia'
import { computed, onMounted, onUnmounted } from 'vue'

import DataTable from '@/components/DataTable.vue'
import SearchInput from '@/components/SearchInput.vue'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { useSearchQuery } from '@/composables/useSearchQuery'
import { eventsColumns } from '@/lib/events-columns'
import { EVENT_KIND_LABEL, EVENT_KINDS, filterEvents } from '@/lib/events-filter'
import { useEventsStore } from '@/stores/events'

// This component backs the new "Events" tab (App.vue, F3) — GET
// /admin/api/events (admin.go/events.go: adminEventsResponse). Polled only
// while this view is mounted (onMounted/onUnmounted below), unlike the
// Providers/Usage/Targets dashboard poll which runs for the app's whole
// session — see stores/events.ts's own doc comment.
const events = useEventsStore()

const ALL_KINDS = 'all'

// kindFilter/userFilter live on the store, not as local refs (F9,
// hash-state): composables/useHashState.ts reads/writes them directly so
// this filter round-trips through the URL hash. userFilter binds through
// useSearchQuery's own external-ref option — see useSearchQuery.ts's own
// doc comment for the other current external-ref caller
// (ConsumerDirectory.vue).
const { kindFilter, userFilter } = storeToRefs(events)
const { normalized: normalizedUserQuery } = useSearchQuery(userFilter)

/** kindSelectValue adapts store's '' ("every kind") to the Select's own ALL_KINDS sentinel — Radix Select items cannot use an empty-string value. */
const kindSelectValue = computed(() => (kindFilter.value === '' ? ALL_KINDS : kindFilter.value))

const filteredEvents = computed(() =>
  filterEvents(events.events, {
    kind: kindFilter.value,
    user: normalizedUserQuery.value,
  }),
)

function onKindChange(value: unknown): void {
  if (typeof value !== 'string') return
  events.setKindFilter(value === ALL_KINDS ? '' : value)
}

const columns = eventsColumns()

/**
 * emptyMessage mirrors TargetsView.vue/ConsumerDirectory.vue's own
 * three-way pattern: distinguishes "nothing recorded at all" from
 * "nothing matches the active filter".
 *
 * P9 review fix: gated on events.lastUpdated (null until the first fetch
 * settles, stores/events.ts) — this used to read events.events.length
 * directly, which is an empty array from the store's OWN initial state,
 * before any fetch has even started. That produced "no events recorded"
 * for a fraction of a second (or longer, on a slow poll) on every mount,
 * indistinguishable from the real "genuinely nothing has ever happened"
 * case — a false negative, not a loading state.
 */
const emptyMessage = computed(() => {
  if (!events.lastUpdated) return 'loading…'
  if (events.events.length === 0) return 'no events recorded'
  if (kindFilter.value !== '' || normalizedUserQuery.value) return 'no events match the current filter'
  return 'none'
})

/**
 * sourceCaption states plainly whether the list below is fleet-wide or
 * this-replica-only — the Events view must never present a replica-only
 * fallback as if it were the fleet-wide picture.
 *
 * P9 review fix: gated on events.lastUpdated, mirroring emptyMessage's own
 * doc comment above — the store's initial state (source: 'replica',
 * replica: '', capacity: 0) otherwise rendered as "This replica only
 * (unknown) — Redis is unreachable or unconfigured" before the first
 * fetch had even resolved, misreporting a genuine degraded-Redis state
 * that had not actually been observed yet.
 */
const sourceCaption = computed(() => {
  if (!events.lastUpdated) return 'loading…'
  if (events.source === 'redis') return 'Fleet-wide, across every replica'
  return `This replica only (${events.replica || 'unknown'}) — Redis is unreachable or unconfigured`
})

onMounted(() => {
  events.startPolling()
})
onUnmounted(() => {
  events.stopPolling()
})
</script>

<template>
  <div class="flex flex-col gap-6">
    <Alert v-if="events.degraded" variant="destructive">
      <FontAwesomeIcon :icon="faTriangleExclamation" class="size-4" aria-hidden="true" />
      <AlertTitle>Fleet-wide event log unavailable</AlertTitle>
      <AlertDescription>
        Redis is configured but could not be read — showing this replica's own in-memory events only, which may miss
        events recorded by other replicas.
      </AlertDescription>
    </Alert>

    <Card>
      <CardHeader class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
        <div>
          <CardTitle>Events</CardTitle>
          <!-- P9: a loading state until the first fetch settles (events.lastUpdated), instead of showing the store's initial (pre-fetch) source/capacity as if it were real. -->
          <CardDescription v-if="!events.lastUpdated">Loading…</CardDescription>
          <CardDescription v-else>
            {{ sourceCaption }} &mdash; rate limit, budget, and upstream events, newest first (capacity
            {{ events.capacity }}).
          </CardDescription>
        </div>
        <div class="flex flex-wrap items-center gap-3">
          <SearchInput v-model="userFilter" placeholder="Filter by user or group" class="w-56" />
          <Select :model-value="kindSelectValue" @update:model-value="onKindChange">
            <SelectTrigger class="w-44" aria-label="Filter by kind">
              <SelectValue placeholder="Kind" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem :value="ALL_KINDS">All kinds</SelectItem>
              <SelectItem v-for="k in EVENT_KINDS" :key="k" :value="k">{{ EVENT_KIND_LABEL[k] }}</SelectItem>
            </SelectContent>
          </Select>
        </div>
      </CardHeader>
      <CardContent>
        <p v-if="events.error" class="mb-3 text-sm text-destructive">{{ events.error }}</p>
        <DataTable :columns="columns" :data="filteredEvents" :empty-message="emptyMessage" />
      </CardContent>
    </Card>
  </div>
</template>
