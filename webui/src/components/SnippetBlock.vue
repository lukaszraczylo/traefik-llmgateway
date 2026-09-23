<script setup lang="ts">
import { faCheck, faCopy, faTriangleExclamation } from '@fortawesome/free-solid-svg-icons'
import { onUnmounted, ref, useTemplateRef } from 'vue'

import { Button } from '@/components/ui/button'
import { copyText } from '@/lib/clipboard'
import { useToastsStore } from '@/stores/toasts'

/**
 * SnippetBlock (redesign-plan.md section 3.4) is the one "here is text to
 * paste into a file" surface every ChangeHelper form and ConfigTree.vue
 * share — a `<pre>` plus a copy button (lib/clipboard.ts's copyText, same
 * Clipboard-API-with-selection-fallback convention CsvExportButton.vue's
 * download path and ModelChip.vue's own click-to-copy already use).
 */
const props = defineProps<{
  /** The exact text to display and copy — YAML (lib/snippets.ts) or a single-line JSON fragment, verbatim, never re-formatted by this component. */
  text: string
  /** A short caption above the block, e.g. "Paste into middleware.yaml" or "Add to the user's users.json line". */
  label?: string
}>()

const preRef = useTemplateRef<HTMLElement>('pre')
const state = ref<'idle' | 'copied' | 'selected' | 'failed'>('idle')
let resetTimer: ReturnType<typeof setTimeout> | undefined
const toasts = useToastsStore()

async function onCopy(): Promise<void> {
  state.value = await copyText(props.text, preRef.value ?? undefined)
  // Only a FAILURE also gets a toast (states-plan.md item 3) — see
  // ModelChip.vue's own identical doc comment: 'copied'/'selected'
  // already have clear inline feedback (the button's own icon/label
  // swap right below), so a success toast would double-notify.
  if (state.value === 'failed') toasts.push({ kind: 'error', message: 'Could not copy the snippet.' })
  clearTimeout(resetTimer)
  resetTimer = setTimeout(() => {
    state.value = 'idle'
  }, 1500)
}

const BUTTON_LABEL: Record<typeof state.value, string> = {
  idle: 'Copy',
  copied: 'Copied',
  selected: 'Selected — press ⌘/Ctrl+C',
  failed: 'Copy failed',
}

onUnmounted(() => clearTimeout(resetTimer))
</script>

<template>
  <div class="flex flex-col gap-1.5">
    <div class="flex items-center justify-between gap-2">
      <span v-if="label" class="text-xs font-medium text-muted-foreground">{{ label }}</span>
      <Button type="button" variant="outline" size="sm" class="ml-auto" @click="onCopy">
        <FontAwesomeIcon
          :icon="state === 'idle' ? faCopy : state === 'failed' ? faTriangleExclamation : faCheck"
          class="size-3.5"
          :class="state === 'failed' ? 'text-destructive' : undefined"
          aria-hidden="true"
        />
        {{ BUTTON_LABEL[state] }}
      </Button>
    </div>
    <pre ref="pre" class="overflow-x-auto rounded-md border bg-muted p-3 font-mono text-xs select-text whitespace-pre">{{ text }}</pre>
  </div>
</template>
