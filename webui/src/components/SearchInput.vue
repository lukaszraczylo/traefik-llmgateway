<script setup lang="ts">
import type { HTMLAttributes } from 'vue'
import { faMagnifyingGlass, faXmark } from '@fortawesome/free-solid-svg-icons'
import { computed } from 'vue'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { cn } from '@/lib/utils'

/**
 * SearchInput is the one search-box control every filter in this panel
 * uses — ProvidersView's model search, UsageView's user/group search, and
 * ChartsView's scope-picker search each grew an identical ~22-line
 * magnifying-glass-icon + Input + clear-button block; extracted into one
 * component (vue.md: "if you've written it twice, you owe an
 * abstraction"). A controlled component (modelValue/update:modelValue),
 * not v-model'd internally, since the incoming modelValue is a prop —
 * callers pair it with `useSearchQuery()` (composables/useSearchQuery.ts)
 * for the query/normalized/hasQuery/clear state, or v-model it against any
 * plain string ref directly.
 *
 * The clear button is a real shadcn Button (variant="ghost", size
 * "icon-xs") instead of a hand-rolled `<button>` — review fix: the
 * hand-rolled version had no focus-visible ring of its own and a smaller
 * than WCAG 2.2 SC 2.5.8 24x24 minimum hit target. Button's "icon-xs" size
 * is exactly size-6 (24x24px, ui/button/index.ts), and buttonVariants
 * already carries a focus-visible ring — fixing the a11y gap once here
 * instead of at every call site.
 */
const props = defineProps<{
  modelValue: string
  placeholder: string
  class?: HTMLAttributes['class']
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: string): void
}>()

const hasValue = computed(() => props.modelValue.length > 0)

function onInput(value: string | number): void {
  emit('update:modelValue', String(value))
}

function onClear(): void {
  emit('update:modelValue', '')
}
</script>

<template>
  <div :class="cn('relative', props.class)">
    <FontAwesomeIcon
      :icon="faMagnifyingGlass"
      class="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground"
      aria-hidden="true"
    />
    <Input
      :model-value="modelValue"
      type="text"
      :placeholder="placeholder"
      :aria-label="placeholder"
      class="pr-8 pl-8"
      @update:model-value="onInput"
    />
    <Button
      v-if="hasValue"
      type="button"
      variant="ghost"
      size="icon-xs"
      aria-label="Clear search"
      class="absolute top-1/2 right-1 -translate-y-1/2 text-muted-foreground hover:text-foreground"
      @click="onClear"
    >
      <FontAwesomeIcon :icon="faXmark" class="size-3.5" />
    </Button>
  </div>
</template>
