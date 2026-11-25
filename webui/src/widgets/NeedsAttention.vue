<script setup lang="ts">
import { StatusDot } from '@perfect-design/ui'

import { useEntityLinks } from '@/composables/useEntityLinks'
import type { AttentionItem, AttentionSeverity } from '@/lib/attention'

const TONE: Record<AttentionSeverity, 'danger' | 'warning' | 'info'> = { danger: 'danger', warning: 'warning', info: 'info' }

defineProps<{ items: AttentionItem[] }>()
const { openLink } = useEntityLinks()
</script>

<template>
  <p v-if="items.length === 0" class="text-muted-foreground m-0 flex h-8 items-center gap-2 text-sm">
    <StatusDot tone="success" size="md" />
    Nothing needs attention
  </p>
  <ul v-else class="m-0 flex list-none flex-col p-0" aria-label="Needs attention">
    <li v-for="(item, i) in items" :key="`${item.severity}-${i}`">
      <button
        type="button"
        class="hover:bg-accent flex min-h-8 w-full items-start gap-2 rounded-md px-2 py-1 text-left text-sm"
        :data-severity="item.severity"
        @click="openLink(item.link)"
      >
        <StatusDot class="mt-1.5 shrink-0" :tone="TONE[item.severity]" size="md" />
        <span class="min-w-0 break-words">{{ item.text }}</span>
      </button>
    </li>
  </ul>
</template>
