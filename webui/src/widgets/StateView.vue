<script setup lang="ts">
import { EmptyState, ErrorState, LoadingState, StaleNotice } from '@perfect-design/ui'

export type ViewState = 'loading' | 'empty' | 'error' | 'ready'

withDefaults(
  defineProps<{
    state: ViewState
    /** Fetch time of the data on screen; a stale one shows the notice above the content. */
    updatedAt?: number | string | Date | null
    emptyTitle?: string
    emptyDescription?: string
    errorTitle?: string
    errorMessage?: string
    retryable?: boolean
    rows?: number
  }>(),
  { emptyTitle: 'Nothing here yet', errorTitle: 'Could not load data', retryable: true, rows: 5 },
)
const emit = defineEmits<{ retry: [] }>()
</script>

<template>
  <LoadingState v-if="state === 'loading'" variant="table" :rows="rows" label="Loading" />
  <ErrorState
    v-else-if="state === 'error'"
    :title="errorTitle"
    :message="errorMessage"
    :retryable="retryable"
    @retry="emit('retry')"
  />
  <EmptyState v-else-if="state === 'empty'" :title="emptyTitle" :description="emptyDescription">
    <template v-if="$slots.action" #action><slot name="action" /></template>
  </EmptyState>
  <template v-else>
    <StaleNotice v-if="updatedAt" variant="inline" :timestamp="updatedAt" hide-when-fresh />
    <slot />
  </template>
</template>
