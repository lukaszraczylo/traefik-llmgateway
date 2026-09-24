<script setup lang="ts">
/**
 * DetailField (reuse-audit.md F6) is the ONE `dt`/`dd` detail-field pair
 * every accordion/expanded-row detail grid in this panel used to hand-
 * roll: a small muted `dt` label (`labelTitle` for a hint on the LABEL
 * itself, e.g. the glob-pattern hint on a group's Providers/Models/MCP
 * servers/Agents fields) and a `dd` for the value (default slot) —
 * `numeric` adds `tabular-nums` to the `dd` for a figure that should align
 * in a monospaced-digit grid. `inheritAttrs: false` + `v-bind="$attrs"` on
 * the `dd` (not the root `div`) forwards any other attribute a caller
 * passes straight onto the VALUE element — a conditional `class` (e.g. a
 * destructive rejected-count) or `title` (e.g. a "projected to exceed"
 * tooltip) lands on the same element it did before this extraction, never
 * on the label.
 */
defineOptions({ inheritAttrs: false })

withDefaults(
  defineProps<{
    label: string
    labelTitle?: string
    numeric?: boolean
  }>(),
  { labelTitle: undefined, numeric: false },
)
</script>

<template>
  <div>
    <dt class="text-xs text-muted-foreground" :title="labelTitle">{{ label }}</dt>
    <dd v-bind="$attrs" :class="numeric ? 'tabular-nums' : undefined">
      <slot />
    </dd>
  </div>
</template>
