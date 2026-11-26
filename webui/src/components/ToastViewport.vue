<script setup lang="ts">
import { ToastItem } from '@perfect-design/ui'
import { computed } from 'vue'

import type { Toast, ToastKind } from '@/stores/toasts'
import { useToastsStore } from '@/stores/toasts'

/**
 * ToastViewport (states-plan.md item 3) is the panel's one toast stack —
 * mounted ONCE in App.vue, reading stores/toasts.ts. Bottom-right, a
 * fixed width, on desktop; a full-width bottom sheet on mobile, inset by
 * the device's own safe area (the `pb-[calc(env(safe-area-inset-bottom)…
 * )]` arbitrary value below, not an inline style — this panel styles
 * with Tailwind only).
 *
 * TWO always-mounted live regions (verify-ui-states.md #5/#6 fix,
 * replacing the earlier per-toast `role="status"`/`role="alert"`
 * approach): a live region must exist in the DOM BEFORE its content
 * changes to be reliably announced — a `role="status"`/`role="alert"`
 * node inserted already filled with text is often missed entirely
 * (NVDA/JAWS, and VoiceOver in some cases). `politeRegion` below
 * (`aria-live="polite"`) is present from this component's first render
 * and holds success/info toasts; `assertiveRegion` (`aria-live="assertive"`)
 * is likewise always present and holds error toasts — toasts render AS
 * CHILDREN into whichever already-mounted region matches their kind,
 * so only their own text content changes, which every live-region
 * implementation reliably announces. `motion-reduce:transition-none` on
 * both TransitionGroups honors prefers-reduced-motion instead of
 * animating regardless.
 */
const toasts = useToastsStore()

const VARIANT: Record<ToastKind, 'success' | 'danger' | 'info'> = {
  success: 'success',
  error: 'danger',
  info: 'info',
}

/** politeToasts/assertiveToasts split the store's single ordered list by kind — success/info are politely announced, error assertively, matching each region's own aria-live level. */
const politeToasts = computed<Toast[]>(() => toasts.toasts.filter((t) => t.kind !== 'error'))
const assertiveToasts = computed<Toast[]>(() => toasts.toasts.filter((t) => t.kind === 'error'))

function onDismiss(toast: Toast): void {
  toasts.dismiss(toast.id)
}
</script>

<template>
  <div
    class="pointer-events-none fixed inset-x-0 bottom-0 z-(--z-toast) flex flex-col-reverse gap-2 p-4 pb-[calc(env(safe-area-inset-bottom)+1rem)] sm:inset-x-auto sm:right-4 sm:bottom-4 sm:w-96"
  >
    <div aria-live="assertive" class="flex flex-col-reverse gap-2">
      <TransitionGroup
        tag="div"
        class="flex flex-col-reverse gap-2"
        enter-active-class="transition duration-150 ease-out motion-reduce:transition-none"
        enter-from-class="translate-y-2 opacity-0"
        leave-active-class="transition duration-150 ease-in motion-reduce:transition-none"
        leave-to-class="opacity-0"
      >
        <div v-for="toast in assertiveToasts" :key="toast.id" class="pointer-events-auto">
          <ToastItem
            :title="toast.message"
            :variant="VARIANT[toast.kind]"
            :announce="false"
            :dismiss-label="`Dismiss: ${toast.message}`"
            @dismiss="onDismiss(toast)"
          />
        </div>
      </TransitionGroup>
    </div>
    <div aria-live="polite" class="flex flex-col-reverse gap-2">
      <TransitionGroup
        tag="div"
        class="flex flex-col-reverse gap-2"
        enter-active-class="transition duration-150 ease-out motion-reduce:transition-none"
        enter-from-class="translate-y-2 opacity-0"
        leave-active-class="transition duration-150 ease-in motion-reduce:transition-none"
        leave-to-class="opacity-0"
      >
        <div v-for="toast in politeToasts" :key="toast.id" class="pointer-events-auto">
          <ToastItem
            :title="toast.message"
            :variant="VARIANT[toast.kind]"
            :announce="false"
            :dismiss-label="`Dismiss: ${toast.message}`"
            @dismiss="onDismiss(toast)"
          />
        </div>
      </TransitionGroup>
    </div>
  </div>
</template>
