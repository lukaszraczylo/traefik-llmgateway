<script setup lang="ts">
import { inputVariants } from '@perfect-design/ui'

// pd's Input emits a Number for type="number", which rewrites what the user typed ("0.50" -> "0.5"); the helper forms validate the raw string, so this keeps a native number input.
defineProps<{ modelValue: string }>()
const emit = defineEmits<{ 'update:modelValue': [value: string] }>()
const classes = inputVariants()

function onInput(event: Event): void {
  emit('update:modelValue', (event.target as HTMLInputElement).value)
}
</script>

<template>
  <input type="number" :value="modelValue" :class="classes" @input="onInput" />
</template>
