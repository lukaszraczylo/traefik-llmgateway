<script setup lang="ts">
/**
 * StatItem (reuse-audit.md F6) is the ONE label-over-value stat cell every
 * "Total"/overview grid in this panel used to hand-roll: a muted label,
 * then either the value (default slot, rendered `text-lg font-semibold
 * tabular-nums`, optionally tinted by `tone`) or, when `hint` is set, a
 * small muted substitute line instead of a value — the "not enough data
 * yet"/"pick a reference model" case every month-projection or run-out
 * figure in this panel shows before it has enough data to report one.
 */
withDefaults(
  defineProps<{
    label: string
    tone?: 'default' | 'destructive' | 'accent' | 'muted'
    /** Shown as the value paragraph's `title` attribute — e.g. "Projected to exceed the configured cost/month limit." */
    title?: string
    /** A small muted line shown INSTEAD of the (default-slot) value — e.g. "not enough data yet". Omit to render the value normally. */
    hint?: string
  }>(),
  { tone: 'default', title: undefined, hint: undefined },
)

const TONE_CLASS: Record<'default' | 'destructive' | 'accent' | 'muted', string | undefined> = {
  default: undefined,
  destructive: 'text-destructive',
  accent: 'text-chart-requests',
  muted: 'text-muted-foreground',
}
</script>

<template>
  <div>
    <p class="text-muted-foreground">{{ label }}</p>
    <p v-if="hint" class="text-sm text-muted-foreground">{{ hint }}</p>
    <p v-else class="text-lg font-semibold tabular-nums" :class="TONE_CLASS[tone]" :title="title">
      <slot />
    </p>
  </div>
</template>
