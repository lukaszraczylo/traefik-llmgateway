<script setup lang="ts">
import { faInbox } from '@fortawesome/free-solid-svg-icons'
import type { IconDefinition } from '@fortawesome/free-solid-svg-icons'

/**
 * EmptyState is the ONE shared "nothing to show, and that's expected"
 * placeholder every table/list/card in this panel uses instead of an
 * ad-hoc "no X configured"/"no traffic" text line (states-plan.md item
 * 2) — an icon, a short title, a one-line description, and an optional
 * action (a button/link via the default slot, e.g. "Try a longer range"
 * or a link to the Config page's change helper). `role="status"` — this
 * is informational, not an error, so it is announced politely
 * (aria-live="polite" semantics), matching ErrorState.vue's own
 * `role="alert"` counterpart for the destructive case.
 */
withDefaults(
  defineProps<{
    icon?: IconDefinition
    title: string
    description?: string
  }>(),
  { icon: () => faInbox },
)
</script>

<template>
  <div role="status" class="flex flex-col items-center gap-2 px-4 py-8 text-center">
    <FontAwesomeIcon :icon="icon" class="size-6 text-muted-foreground/60" aria-hidden="true" />
    <p class="text-sm font-medium text-foreground">{{ title }}</p>
    <p v-if="description" class="max-w-sm text-sm text-muted-foreground">{{ description }}</p>
    <div v-if="$slots.default" class="mt-1">
      <slot />
    </div>
  </div>
</template>
