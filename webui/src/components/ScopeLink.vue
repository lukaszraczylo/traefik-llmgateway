<script setup lang="ts">
import { faChartLine } from '@fortawesome/free-solid-svg-icons'

import { useNavStore } from '@/stores/nav'

/**
 * ScopeLink (F6) is the one "jump to this scope's Charts view" control in
 * this panel: usage-columns.ts's id cell (Users/Groups tables and the
 * nested Members table), UsageView.vue's Groups accordion header, and
 * WP-B2's ProvidersView.vue per-model chips all render one of these
 * instead of each hand-rolling their own click-through-to-Charts wiring.
 *
 * A real `<button type="button">` (P2 review fix). This used to be a
 * `<span role="link" tabindex="0">`, justified at the time by the Groups
 * accordion trigger use site nesting it INSIDE shadcn-vue's
 * AccordionTrigger — itself a real `<button>` (reka-ui's own
 * AccordionTrigger primitive). That justification was wrong on its own
 * terms: the HTML button content model forbids ANY descendant carrying a
 * `tabindex` attribute, not just a nested button/anchor, and ARIA's
 * `button` role additionally declares "children presentational: true", so
 * assistive technology flattened the inner span and never exposed it as
 * its own link at all — axe flags this as `nested-interactive`. Reka's
 * AccordionTrigger also only listens for `click` (not `keydown`), so
 * `stopPropagation()` on click and Enter suppressed the accordion's own
 * toggle, but Space on the focused span did nothing and fell through to
 * page scroll instead of activating anything.
 *
 * The actual fix is structural, not a markup trick: every call site now
 * renders this component as a SIBLING of whatever it used to nest inside
 * (UsageView.vue's Groups header, ProvidersView.vue's provider-name
 * header — see both templates), never a descendant of another interactive
 * element, so a real `<button>` is valid there. No `role` override: a
 * plain button's implicit role is already correct here (activating it
 * performs an action — a programmatic Pinia-store navigation, not a
 * traditional `href` follow — which is exactly what a native `<button>`,
 * not `<a>`, is for; WAI-ARIA's first rule of ARIA use is not to override
 * a native element's semantics without a reason, and there is none left
 * here). `stopPropagation` on click stays — harmless everywhere (a table
 * cell, ProvidersView's per-model chips) and still correct defensively at
 * the Groups/Providers headers, where this button sits next to, not
 * inside, the accordion's own trigger.
 */
const props = defineProps<{
  /** The visible + accessible label, e.g. a user id, group id, or model id. */
  label: string
  /** The scope string useNavStore.goToCharts expects — "total" | "user:{id}" | "group:{id}" | "model:{id}" (history.ts's own convention). */
  scope: string
}>()

const nav = useNavStore()

function onClick(event: MouseEvent): void {
  event.stopPropagation()
  nav.goToCharts(props.scope)
}
</script>

<template>
  <button
    type="button"
    :aria-label="`View ${label} in Charts`"
    :title="`View ${label} in Charts`"
    class="inline-flex cursor-pointer items-center gap-1 rounded-sm text-inherit hover:text-primary hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
    @click="onClick"
  >
    <span>{{ label }}</span>
    <FontAwesomeIcon :icon="faChartLine" class="size-3" aria-hidden="true" />
  </button>
</template>
