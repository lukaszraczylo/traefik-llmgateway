<script setup lang="ts">
import { faCircleCheck, faTriangleExclamation } from '@fortawesome/free-solid-svg-icons'
import { storeToRefs } from 'pinia'
import { computed, onMounted, onUnmounted } from 'vue'

import DataTable from '@/components/DataTable.vue'
import EmptyState from '@/components/EmptyState.vue'
import ErrorState from '@/components/ErrorState.vue'
import SearchInput from '@/components/SearchInput.vue'
import SkeletonTable from '@/components/SkeletonTable.vue'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { useSearchQuery } from '@/composables/useSearchQuery'
import { eventsColumns } from '@/lib/events-columns'
import { EVENT_KIND_LABEL, EVENT_KINDS, filterEvents } from '@/lib/events-filter'
import { loadState } from '@/lib/load-state'
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
 * eventsLoadState (lib/load-state.ts) replaces the old lone
 * `!events.lastUpdated` check this view used everywhere: 'skeleton' for
 * the first fetch, 'error' when that first fetch itself failed (P9's own
 * false-negative bug — an initial-load FAILURE used to render the exact
 * same "loading…" text as a genuine in-flight load, forever, since
 * events.error was never even read here), 'ready' once there is a
 * successful fetch to show (an EMPTY events list after a successful
 * fetch is still 'ready' — DataTable's own emptyMessage below renders
 * the real "no events recorded" row, not this card-level state). A
 * LATER background-poll failure, with events already on screen, stays
 * 'ready' too (stores/events.ts's own toast covers that case instead —
 * states-plan.md item 3 — so content is never wiped/replaced by a
 * skeleton or error here).
 */
const eventsLoadState = computed(() => loadState({ loading: events.refreshing, hasData: events.lastUpdated !== null, error: events.error }))

/**
 * noEventsRecorded is the GENUINE "nothing has ever happened" case
 * (states-plan.md item 2: "Events/Reliability: 'No failures recorded'
 * (positive tone, success icon)") — renders EmptyState instead of an
 * empty DataTable row, since a clean fleet is worth a reassuring visual,
 * not just quiet table text. Distinct from "nothing matches the active
 * filter" below, which stays a plain inline DataTable row: a filter
 * producing zero rows is a routine, low-stakes interaction, not a
 * fleet-health signal worth the same visual weight.
 *
 * The copy below deliberately does NOT say "in this range" (verify-ui-
 * states.md #11 fix): the events store fetches the last EVENTS_LIMIT
 * events (stores/events.ts: GET /admin/api/events?limit=200), unscoped by
 * the global range filter — "in this range" would misleadingly imply a
 * narrower window filtered this list down to zero.
 */
const noEventsRecorded = computed(() => events.events.length === 0)

/** emptyMessage is DataTable's own inline empty-row text for the "some events exist, but the active filter matches none" case — only ever rendered while noEventsRecorded is false (see the template below). */
const emptyMessage = computed(() => {
  if (kindFilter.value !== '' || normalizedUserQuery.value) return 'no events match the current filter'
  return 'none'
})

/**
 * sourceCaption states plainly whether the list below is fleet-wide or
 * this-replica-only — the Events view must never present a replica-only
 * fallback as if it were the fleet-wide picture. Only rendered while
 * eventsLoadState is 'ready' (see the template below), so this never
 * reads the store's pristine initial source/replica/capacity values.
 */
const sourceCaption = computed(() => {
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
          <!-- Only rendered once there is a real fetch to describe (eventsLoadState 'ready') — 'skeleton'/'error' show nothing here, the CardContent body below already signals which of those it is. -->
          <CardDescription v-if="eventsLoadState === 'ready'">
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
        <SkeletonTable v-if="eventsLoadState === 'skeleton'" :rows="5" :cols="columns.length" />
        <ErrorState v-else-if="eventsLoadState === 'error'" :message="events.error" :on-retry="events.refresh" />
        <EmptyState
          v-else-if="noEventsRecorded"
          :icon="faCircleCheck"
          title="No failures recorded."
          description="Rate-limit, budget, and upstream events will show up here as they happen."
        />
        <DataTable v-else :columns="columns" :data="filteredEvents" :empty-message="emptyMessage" />
      </CardContent>
    </Card>
  </div>
</template>
