<script setup lang="ts">
import { NativeSelect } from '@perfect-design/ui'
import { computed, useId } from 'vue'

export interface LabeledSelectOption {
  value: string
  label: string
}

/**
 * LabeledSelect is the one "label above a native select" block the panel's pickers share.
 * `allLabel`, when set, adds an "All X" option whose value is '' (modelValue === '');
 * omit it for a required-selection picker with no "All" concept.
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

const selectId = `labeled-select-${useId()}`

const allOptions = computed(() => (props.allLabel === undefined ? props.options : [{ value: '', label: props.allLabel }, ...props.options]))
</script>

<template>
  <div class="flex flex-col gap-1">
    <label :for="selectId" class="text-xs font-medium text-muted-foreground">{{ label }}</label>
    <NativeSelect
      :id="selectId"
      :model-value="modelValue"
      :options="allOptions"
      :placeholder="allLabel === undefined ? placeholder : undefined"
      :class="triggerClass"
      @update:model-value="emit('update:modelValue', $event)"
    />
  </div>
</template>
