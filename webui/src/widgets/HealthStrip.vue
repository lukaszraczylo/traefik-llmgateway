<script setup lang="ts">
import { StatusDot } from '@perfect-design/ui'
import { computed } from 'vue'

import { openBreakerCount } from '@/lib/kpi'
import { useNavStore } from '@/stores/nav'
import type { AdminProviderView, AdminRedisView } from '@/types/api'

type ChipTone = 'success' | 'warning' | 'danger' | 'neutral'
interface Chip {
  key: string
  tone: ChipTone
  text: string
  title?: string
  link?: boolean
}

const props = defineProps<{ providers: AdminProviderView[]; redis: AdminRedisView; eventsSource: 'redis' | 'replica' | null }>()
const nav = useNavStore()

const failing = computed(() => props.providers.filter((p) => p.healthState !== 'closed' || !!p.lastErr))
const healthy = computed(() => props.providers.length - failing.value.length)
const breakers = computed(() => openBreakerCount(props.providers))

const chips = computed<Chip[]>(() => {
  const total = props.providers.length
  const out: Chip[] = []
  const providerTone: ChipTone = total === 0 ? 'neutral' : healthy.value === total ? 'success' : healthy.value === 0 ? 'danger' : 'warning'
  out.push({ key: 'providers', tone: providerTone, text: total === 0 ? 'No providers' : `${healthy.value} of ${total} providers healthy` })
  if (failing.value.length > 0) out.push({ key: 'failing', tone: 'danger', text: `${failing.value.length} failing`, link: true })
  if (!props.redis.configured) out.push({ key: 'redis', tone: 'neutral', text: 'Redis not configured' })
  else if (props.eventsSource === null) out.push({ key: 'redis', tone: 'neutral', text: 'Redis checking' })
  else if (props.eventsSource === 'redis') out.push({ key: 'redis', tone: 'success', text: 'Redis connected' })
  else out.push({ key: 'redis', tone: 'danger', text: 'Redis unreachable', title: props.redis.lastErr })
  out.push({
    key: 'breakers',
    tone: breakers.value > 0 ? 'danger' : 'success',
    text: breakers.value > 0 ? `${breakers.value} open breaker${breakers.value === 1 ? '' : 's'}` : 'No open breakers',
  })
  return out
})
</script>

<template>
  <ul class="m-0 flex list-none flex-wrap gap-2 p-0" aria-label="Fleet health">
    <li v-for="chip in chips" :key="chip.key" class="min-w-0 max-w-full">
      <component
        :is="chip.link ? 'button' : 'span'"
        class="border-border bg-card text-foreground inline-flex h-8 max-w-full sm:max-w-md items-center gap-2 rounded-md border px-3 text-sm"
        :class="chip.link ? 'hover:bg-accent cursor-pointer' : ''"
        :title="chip.title"
        :type="chip.link ? 'button' : undefined"
        @click="chip.link && nav.goTo('reliability', {}, { replace: false })"
      >
        <StatusDot :tone="chip.tone" size="md" />
        <span class="truncate">{{ chip.text }}</span>
      </component>
    </li>
  </ul>
</template>
