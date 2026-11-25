<script setup lang="ts">
import { StatusDot } from '@perfect-design/ui'

import { useEntityLinks } from '@/composables/useEntityLinks'
import { kindLabel } from '@/lib/events-filter'
import { useNavStore } from '@/stores/nav'
import type { AdminEventView } from '@/types/api'

type Tone = 'danger' | 'warning' | 'info' | 'neutral'
const TONE: Record<string, Tone> = { rate_limit: 'warning', budget: 'warning', timeout: 'warning', capacity: 'warning', store_down: 'danger', upstream: 'danger', unpriced: 'info', paced: 'info' }

defineProps<{ events: AdminEventView[] }>()
const nav = useNavStore()
const { openEntity } = useEntityLinks()

function subject(e: AdminEventView): string {
  return e.user || e.provider || e.model || e.route || e.message
}

function clock(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })
}

function open(e: AdminEventView): void {
  if (e.user) openEntity('user', e.user)
  else if (e.provider) openEntity('provider', e.provider)
  else nav.goTo('events', {}, { replace: false })
}
</script>

<template>
  <ul class="m-0 flex list-none flex-col p-0" aria-label="Recent events">
    <li v-for="(e, i) in events" :key="`${e.time}-${e.replica}-${i}`">
      <button
        type="button"
        class="hover:bg-accent flex h-8 w-full items-center gap-2 rounded-md px-2 text-left text-sm"
        :title="`${kindLabel(e.kind)}: ${e.message}`"
        @click="open(e)"
      >
        <StatusDot class="shrink-0" :tone="TONE[e.kind] ?? 'neutral'" size="md" />
        <span class="text-muted-foreground shrink-0 font-mono text-xs">{{ clock(e.time) }}</span>
        <span class="text-muted-foreground shrink-0">{{ kindLabel(e.kind) }}</span>
        <span class="min-w-0 flex-1 truncate">{{ subject(e) }}</span>
      </button>
    </li>
  </ul>
</template>
