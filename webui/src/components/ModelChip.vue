<script setup lang="ts">
import { faCheck, faCopy, faTriangleExclamation } from '@fortawesome/free-solid-svg-icons'
import { computed, onUnmounted, ref, useTemplateRef } from 'vue'

import { Badge } from '@/components/ui/badge'
import { copyText } from '@/lib/clipboard'

/**
 * ModelChip is one clickable, copyable model-id chip in a provider's
 * expanded model list (ProvidersView.vue's provider-model accordion). id
 * is the full routable id (lib/format.ts's routableModelId) — what
 * actually gets copied, so a pasted value works straight into a
 * `model: "..."` request field.
 *
 * The label span (labelRef below) is copyText's select-text fallback
 * target — not a rare edge case here: navigator.clipboard is undefined
 * outright on any plain-HTTP admin deployment (the Clipboard API
 * requires a secure context — https, or localhost), so the fallback is
 * the ONLY path a click ever takes there, not a backstop for an unlikely
 * failure. labelRef is a template ref on an element this component
 * always renders (below), so it is populated by the time onClick can
 * ever run — a click handler cannot fire before mount.
 */
const props = defineProps<{ id: string }>()

const labelRef = useTemplateRef<HTMLElement>('label')
const state = ref<'idle' | 'copied' | 'selected' | 'failed'>('idle')
let resetTimer: ReturnType<typeof setTimeout> | undefined

function scheduleReset(): void {
  clearTimeout(resetTimer)
  resetTimer = setTimeout(() => {
    state.value = 'idle'
  }, 1500)
}

async function onClick(): Promise<void> {
  const result = await copyText(props.id, labelRef.value ?? undefined)
  // 'failed' (neither the Clipboard API nor the selection fallback
  // worked) gets its own visible state rather than a silent no-op — a
  // click that visibly does nothing reads as a broken button, not as
  // "nothing to report".
  state.value = result
  scheduleReset()
}

const icon = {
  idle: faCopy,
  copied: faCheck,
  selected: faCheck,
  failed: faTriangleExclamation,
}
const title = computed(() => ({
  idle: `Copy ${props.id}`,
  copied: 'Copied',
  selected: 'Selected — press Ctrl/Cmd+C',
  failed: 'Copy failed',
}))

onUnmounted(() => {
  clearTimeout(resetTimer)
})
</script>

<template>
  <Badge
    as="button"
    type="button"
    variant="secondary"
    class="cursor-pointer gap-1 font-mono text-xs font-normal transition-colors hover:bg-accent hover:text-accent-foreground"
    :class="state === 'failed' ? 'text-destructive' : ''"
    :title="title[state]"
    @click="onClick"
  >
    <span ref="label" class="select-text">{{ id }}</span>
    <FontAwesomeIcon :icon="icon[state]" class="size-2.5 shrink-0" aria-hidden="true" />
  </Badge>
</template>
