<script setup lang="ts">
import { faCheck, faCopy } from '@fortawesome/free-solid-svg-icons'
import { ref, useTemplateRef } from 'vue'

import { Badge } from '@/components/ui/badge'
import { copyText } from '@/lib/clipboard'

/**
 * ModelChip is one clickable, copyable model-id chip in a provider's
 * expanded model list (OverviewView.vue's provider-model accordion). id
 * is the full routable id (lib/format.ts's routableModelId) — what
 * actually gets copied, so a pasted value works straight into a
 * `model: "..."` request field.
 */
const props = defineProps<{ id: string }>()

const labelRef = useTemplateRef<HTMLElement>('label')
const state = ref<'idle' | 'copied' | 'selected'>('idle')
let resetTimer: ReturnType<typeof setTimeout> | undefined

async function onClick(): Promise<void> {
  const result = await copyText(props.id, labelRef.value ?? undefined)
  if (result === 'failed') return
  state.value = result
  clearTimeout(resetTimer)
  resetTimer = setTimeout(() => {
    state.value = 'idle'
  }, 1500)
}
</script>

<template>
  <Badge
    as="button"
    type="button"
    variant="secondary"
    class="cursor-pointer gap-1 font-mono text-xs font-normal transition-colors hover:bg-accent hover:text-accent-foreground"
    :title="state === 'idle' ? `Copy ${id}` : state === 'copied' ? 'Copied' : 'Selected — press Ctrl/Cmd+C'"
    @click="onClick"
  >
    <span ref="label">{{ id }}</span>
    <FontAwesomeIcon :icon="state === 'idle' ? faCopy : faCheck" class="size-2.5 shrink-0" aria-hidden="true" />
  </Badge>
</template>
