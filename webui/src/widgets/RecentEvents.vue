<script setup lang="ts">
import { StatusDot } from '@perfect-design/ui'
import { computed } from 'vue'

import { useEntityLinks } from '@/composables/useEntityLinks'
import { eventLink } from '@/lib/event-link'
import { collapseEvents, eventSubject, formatEventTime } from '@/lib/event-feed'
import type { EventGroup } from '@/lib/event-feed'
import { kindLabel } from '@/lib/events-filter'
import { useNavStore } from '@/stores/nav'
import type { AdminEventView } from '@/types/api'

const CLOCK_LENGTH = 5
type Tone = 'danger' | 'warning' | 'info' | 'neutral'
const TONE: Record<string, Tone> = {
  rate_limit: 'warning',
  budget: 'warning',
  timeout: 'warning',
  capacity: 'warning',
  store_down: 'danger',
  upstream: 'danger',
  unpriced: 'info',
  paced: 'info',
  router_escalation: 'info',
  router_timeout_fallback: 'warning',
}

const props = withDefaults(defineProps<{ events?: AdminEventView[]; groups?: EventGroup[]; label?: string; withMessage?: boolean }>(), {
  events: () => [],
  groups: undefined,
  label: 'Recent events',
  withMessage: false,
})
const nav = useNavStore()
const { openLink } = useEntityLinks()

// A link back to the page already showing, with nothing to open, would do nothing.
function linkOf(e: AdminEventView) {
  const link = eventLink(e)
  return link && !(link.page === nav.page && !link.sel && !link.params) ? link : null
}

const rows = computed(() => {
  const now = new Date()
  return (props.groups ?? collapseEvents(props.events)).map((g) => ({ ...g, time: formatEventTime(g.event.time, now), link: linkOf(g.event) }))
})
// Dated rows are wider than a bare clock; a fixed width keeps the kind column aligned.
const dated = computed(() => rows.value.some((r) => r.time.length > CLOCK_LENGTH))
</script>

<template>
  <ul class="m-0 flex list-none flex-col p-0" :aria-label="label">
    <li v-for="r in rows" :key="r.key">
      <component
        :is="r.link ? 'button' : 'div'"
        :type="r.link ? 'button' : undefined"
        class="flex min-h-8 w-full flex-wrap items-center gap-x-2 rounded-md px-2 py-1 text-left text-sm sm:h-8 sm:flex-nowrap sm:py-0"
        :class="r.link ? 'hover:bg-accent' : ''"
        :title="`${kindLabel(r.event.kind)}${r.count > 1 ? ` ×${r.count}` : ''}: ${r.event.message}`"
        @click="r.link && openLink(r.link)"
      >
        <StatusDot class="shrink-0" :tone="TONE[r.event.kind] ?? 'neutral'" size="md" />
        <span class="text-muted-foreground shrink-0 font-mono text-xs tabular-nums" :class="dated ? 'w-[5.5rem]' : ''">{{ r.time }}</span>
        <span class="text-muted-foreground shrink-0">{{ kindLabel(r.event.kind) }}</span>
        <span v-if="r.count > 1" class="text-muted-foreground shrink-0 font-mono text-xs tabular-nums" :aria-label="`${r.count} times`">×{{ r.count }}</span>
        <span class="min-w-0 max-w-full truncate" :class="withMessage ? 'sm:max-w-[40%] sm:shrink-0' : 'flex-1'">{{ eventSubject(r.event) }}</span>
        <span v-if="withMessage" class="text-muted-foreground min-w-0 basis-full truncate pl-4 text-xs sm:flex-1 sm:basis-0 sm:pl-0 sm:text-sm">{{ r.event.message }}</span>
      </component>
    </li>
  </ul>
</template>
