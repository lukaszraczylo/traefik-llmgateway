<script setup lang="ts">
import { faTriangleExclamation } from '@fortawesome/free-solid-svg-icons'
import { ref } from 'vue'

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'

/**
 * ConfigWarningsBanner surfaces GET /admin/api/overview's `warnings`/
 * `warningsDropped` (F10, dashboard-plan.md — promoted warnf calls
 * collected during newGateway's own construction window: metrics.path
 * collisions, a zero cache ttl, multi-provider registry ids, retry x
 * failover). Renders nothing at all for an empty `warnings` array — App.vue
 * only mounts this under the header when there is something to show, but
 * the component stays self-guarding too so it is safe to render
 * unconditionally.
 */
const props = defineProps<{
  warnings: string[]
  /** Warnings logged beyond the server's own cap (configWarningsCap, llmgateway.go) that were dropped, not kept — omitted (undefined) entirely when nothing was dropped. */
  warningsDropped?: number
}>()

/** Collapsed by default: the header/status line already reads cleanly, and a config warning is something an operator investigates once, not something that needs to stay open on every visit. */
const expanded = ref(false)

function toggleExpanded(): void {
  expanded.value = !expanded.value
}
</script>

<template>
  <Alert v-if="props.warnings.length > 0" variant="warn">
    <FontAwesomeIcon :icon="faTriangleExclamation" class="size-4" aria-hidden="true" />
    <AlertTitle class="flex items-center justify-between gap-2">
      <span>{{ props.warnings.length }} configuration {{ props.warnings.length === 1 ? 'warning' : 'warnings' }}</span>
      <!--
        P11 review fix (WCAG 2.5.3 Label in Name): the aria-label used to
        be the fixed text "Toggle configuration warning details", which
        shares NO words with the visible "Show"/"Hide" label a sighted
        reader sees — a speech-input user saying "click show" would not
        match this control's accessible name at all. The aria-label now
        STARTS WITH the same word the button visibly displays, so the
        accessible name always contains the visible label.
      -->
      <Button
        type="button"
        variant="ghost"
        size="xs"
        :aria-expanded="expanded"
        :aria-label="`${expanded ? 'Hide' : 'Show'} configuration warning details`"
        @click="toggleExpanded"
      >
        {{ expanded ? 'Hide' : 'Show' }}
      </Button>
    </AlertTitle>
    <AlertDescription v-if="expanded">
      <ul class="mt-1 list-disc space-y-0.5 pl-4 font-mono text-xs">
        <li v-for="(warning, i) in props.warnings" :key="i">{{ warning }}</li>
      </ul>
      <p v-if="props.warningsDropped" class="mt-1 text-xs">and {{ props.warningsDropped }} more were not kept</p>
    </AlertDescription>
  </Alert>
</template>
