<script setup lang="ts">
import { faArrowRotateRight, faTriangleExclamation } from '@fortawesome/free-solid-svg-icons'
import { ref } from 'vue'

import { Button } from '@/components/ui/button'

/**
 * ErrorState is EmptyState.vue's destructive counterpart (states-plan.md
 * item 2) — a fetch for the CURRENT selection failed and there is no
 * data underneath it to fall back to (lib/load-state.ts's loadState:
 * 'error', not 'ready' — a background refresh failure while data is
 * ALREADY shown never reaches this component, it surfaces via a toast
 * instead, stores/toasts.ts). `message` is the server/network error
 * string already surfaced by lib/api.ts's AdminApiError, shown verbatim
 * — never re-worded, so what the reader sees matches what actually
 * failed. `onRetry`, when given, renders a "Retry" button calling the
 * store's own refresh/ensure action; omitted for a view with no single
 * refresh action to re-run (rare — most stores in this panel expose
 * one). `role="alert"` announces this assertively, unlike EmptyState's
 * polite `role="status"`.
 */
const props = defineProps<{
  message: string
  onRetry?: () => void | Promise<void>
}>()

/**
 * retrying (verify-ui-states.md #6 lows: "Retry shows progress (disabled +
 * spinner/text) while retrying") is LOCAL, not read off the caller's own
 * store loading flag — several call sites' `onRetry` (consumers.
 * fetchConsumers, events.refresh) do not clear their store's `error`
 * before re-fetching, so `loadState` would stay 'error' with no in-flight
 * signal at all if this depended on that. Tracking the retry promise here
 * instead works for every caller uniformly, and disabling the button while
 * it is in flight also closes the "repeated clicks fire parallel requests"
 * gap those same actions have no guard against.
 */
const retrying = ref(false)

async function onRetryClick(): Promise<void> {
  if (!props.onRetry || retrying.value) return
  retrying.value = true
  try {
    await props.onRetry()
  } finally {
    retrying.value = false
  }
}
</script>

<template>
  <div role="alert" class="flex flex-col items-center gap-2 px-4 py-8 text-center">
    <FontAwesomeIcon :icon="faTriangleExclamation" class="size-6 text-destructive" aria-hidden="true" />
    <p class="max-w-full break-words text-sm font-medium text-destructive">{{ message }}</p>
    <Button v-if="onRetry" type="button" variant="outline" size="sm" class="mt-1" :disabled="retrying" @click="onRetryClick">
      <FontAwesomeIcon
        :icon="faArrowRotateRight"
        class="size-3.5"
        :class="retrying ? 'animate-spin motion-reduce:animate-none' : undefined"
        aria-hidden="true"
      />
      {{ retrying ? 'Retrying…' : 'Retry' }}
    </Button>
  </div>
</template>
