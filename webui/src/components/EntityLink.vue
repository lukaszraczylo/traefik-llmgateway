<script setup lang="ts">
import { faArrowUpRightFromSquare } from '@fortawesome/free-solid-svg-icons'

import { useNavStore } from '@/stores/nav'

/**
 * EntityLink (redesign-plan.md section 3.5) is the redesigned shell's own
 * navigation link for an entity id: a user id opens Consumers pre-filtered
 * to that user (lib/usage-columns.ts's id cell, UserDetail.vue), a model
 * id opens Models pre-filtered to that model (lib/model-table-columns.ts's
 * id cell) — nav.goTo rather than a bare href, so it stays inside the
 * SPA's own hash-routed shell. It replaced ScopeLink.vue (deleted, section
 * 3.5) once every one of that component's own call sites had switched
 * over.
 *
 * Structural note carried over from ScopeLink.vue: a real
 * `<button type="button">`, always rendered as a SIBLING of whatever
 * interactive element it sits beside (a table cell, an accordion header),
 * never nested inside one — the HTML button content model forbids a
 * focusable descendant, and nesting one breaks both keyboard operation
 * and how assistive technology exposes it.
 */
const props = defineProps<{
  /** The visible + accessible label, e.g. a user id or model id. */
  label: string
  /** Which page this id opens — 'user' -> Consumers, 'model' -> Models. */
  kind: 'user' | 'model'
  /** The raw entity id (never pre-formatted as "kind:id" — this component builds nav.goTo's own params itself). */
  id: string
}>()

const nav = useNavStore()

const TARGET: Record<'user' | 'model', { page: 'consumers' | 'models'; paramKey: 'user' | 'model'; noun: string }> = {
  user: { page: 'consumers', paramKey: 'user', noun: 'Consumers' },
  model: { page: 'models', paramKey: 'model', noun: 'Models' },
}

function onClick(event: MouseEvent): void {
  event.stopPropagation()
  const target = TARGET[props.kind]
  nav.goTo(target.page, { [target.paramKey]: props.id })
}
</script>

<template>
  <button
    type="button"
    :aria-label="`View ${label} in ${TARGET[kind].noun}`"
    :title="`View ${label} in ${TARGET[kind].noun}`"
    class="inline-flex cursor-pointer items-center gap-1 rounded-sm text-inherit hover:text-primary hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
    @click="onClick"
  >
    <span>{{ label }}</span>
    <FontAwesomeIcon :icon="faArrowUpRightFromSquare" class="size-3" aria-hidden="true" />
  </button>
</template>
