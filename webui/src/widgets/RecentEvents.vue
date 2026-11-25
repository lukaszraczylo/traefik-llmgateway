<script setup lang="ts">
import { StatusDot } from '@perfect-design/ui'
import { computed } from 'vue'

import { useEntityLinks } from '@/composables/useEntityLinks'
import { eventLink } from '@/lib/event-link'
import { kindLabel } from '@/lib/events-filter'
import { useNavStore } from '@/stores/nav'
import type { AdminEventView } from '@/types/api'

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

const props = withDefaults(defineProps<{ events: AdminEventView[]; label?: string; withMessage?: boolean }>(), { label: 'Recent events', withMessage: false })
const nav = useNavStore()
const { openLink } = useEntityLinks()

function subject(e: AdminEventView): string {
  return e.user || e.group || e.provider || e.model || e.route || e.message
}

function clock(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })
}

// A link back to the page already showing, with nothing to open, would do nothing.
function linkOf(e: AdminEventView) {
  const link = eventLink(e)
  return link && !(link.page === nav.page && !link.sel) ? link : null
}

const rows = computed(() => {
  const seen = new Map<string, number>()
  return props.events.map((e) => {
    const base = `${e.time}|${e.replica}|${e.kind}|${e.message}`
    const n = seen.get(base) ?? 0
    seen.set(base, n + 1)
    return { e, key: `${base}|${n}`, link: linkOf(e) }
  })
})
</script>

<template>
  <ul class="m-0 flex list-none flex-col p-0" :aria-label="label">
    <li v-for="r in rows" :key="r.key">
      <component
        :is="r.link ? 'button' : 'div'"
        :type="r.link ? 'button' : undefined"
        class="flex h-8 w-full items-center gap-2 rounded-md px-2 text-left text-sm"
        :class="r.link ? 'hover:bg-accent' : ''"
        :title="`${kindLabel(r.e.kind)}: ${r.e.message}`"
        @click="r.link && openLink(r.link)"
      >
        <StatusDot class="shrink-0" :tone="TONE[r.e.kind] ?? 'neutral'" size="md" />
        <span class="text-muted-foreground shrink-0 font-mono text-xs tabular-nums">{{ clock(r.e.time) }}</span>
        <span class="text-muted-foreground shrink-0">{{ kindLabel(r.e.kind) }}</span>
        <span class="min-w-0 flex-1 truncate">
          {{ subject(r.e) }}<span v-if="withMessage" class="text-muted-foreground hidden sm:inline"> · {{ r.e.message }}</span>
        </span>
      </component>
    </li>
  </ul>
</template>
