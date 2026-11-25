<script setup lang="ts">
import { Button, Callout } from '@perfect-design/ui'
import { ref } from 'vue'

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
  <Callout
    v-if="props.warnings.length > 0"
    type="warning"
    :title="`${props.warnings.length} configuration ${props.warnings.length === 1 ? 'warning' : 'warnings'}`"
  >
    <template v-if="expanded">
      <ul class="mt-1 list-disc space-y-0.5 pl-4 font-mono text-xs">
        <li v-for="(warning, i) in props.warnings" :key="i">{{ warning }}</li>
      </ul>
      <p v-if="props.warningsDropped" class="mt-1 text-xs">and {{ props.warningsDropped }} more were not kept</p>
    </template>
    <template #actions>
      <!-- The aria-label starts with the visible word (WCAG 2.5.3 Label in Name). -->
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
    </template>
  </Callout>
</template>
