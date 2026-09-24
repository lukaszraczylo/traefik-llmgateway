<script setup lang="ts">
import { computed, useId } from 'vue'

import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'

export interface LabeledSelectOption {
  value: string
  label: string
}

/**
 * LabeledSelect (reuse-audit.md F11) is the ONE "label above a Select,
 * wired via aria-labelledby" block this panel used to hand-roll at every
 * filter/target picker (GlobalFilterBar's Range, CostAvoidedCard's
 * reference model, SpendPage's Metric, ReliabilityPage's Provider,
 * UserForm's Primary group). `allLabel`, when set, injects an "All X"
 * option and maps it to/from `modelValue === ''` internally — Radix/
 * reka-ui Select items cannot hold an empty-string value themselves
 * (EventsView.vue's/ReliabilityPage.vue's own identical ALL_KINDS/
 * ALL_PROVIDERS_OPTION doc comments), so every "All" filter used to
 * re-implement this same sentinel swap at its own template boundary.
 * Omit `allLabel` for a plain required-selection picker with no "All"
 * concept (UserForm's Primary group, CostAvoidedCard's reference model).
 */
const props = withDefaults(
  defineProps<{
    label: string
    modelValue: string
    options: LabeledSelectOption[]
    allLabel?: string
    placeholder?: string
    triggerClass?: string
  }>(),
  { allLabel: undefined, placeholder: undefined, triggerClass: undefined },
)

const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

/** ALL_SENTINEL is LabeledSelect's own internal Select-only stand-in for modelValue === '' — never exposed to a caller, which only ever sees '' itself via v-model. */
const ALL_SENTINEL = '__all__'

const labelId = `labeled-select-${useId()}`

const selectValue = computed(() => (props.modelValue === '' && props.allLabel !== undefined ? ALL_SENTINEL : props.modelValue))

function onUpdate(value: unknown): void {
  if (typeof value !== 'string') return
  emit('update:modelValue', value === ALL_SENTINEL ? '' : value)
}
</script>

<template>
  <div class="flex flex-col gap-1">
    <label :id="labelId" class="text-xs font-medium text-muted-foreground">{{ label }}</label>
    <Select :model-value="selectValue" @update:model-value="onUpdate">
      <SelectTrigger :aria-labelledby="labelId" :class="triggerClass">
        <SelectValue :placeholder="placeholder" />
      </SelectTrigger>
      <SelectContent>
        <SelectItem v-if="allLabel !== undefined" :value="ALL_SENTINEL">{{ allLabel }}</SelectItem>
        <SelectItem v-for="opt in options" :key="opt.value" :value="opt.value">{{ opt.label }}</SelectItem>
      </SelectContent>
    </Select>
  </div>
</template>
