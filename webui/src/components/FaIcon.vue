<script setup lang="ts">
import type { IconDefinition } from '@fortawesome/free-solid-svg-icons'
import { computed } from 'vue'

const props = defineProps<{ icon: IconDefinition }>()

const view = computed(() => {
  const [width, height, , , path] = props.icon.icon
  return { box: `0 0 ${width} ${height}`, path: Array.isArray(path) ? path.join('') : path }
})
</script>

<template>
  <svg
    :class="['svg-inline--fa', `fa-${icon.iconName}`]"
    :data-prefix="icon.prefix"
    :data-icon="icon.iconName"
    role="img"
    aria-hidden="true"
    focusable="false"
    :viewBox="view.box"
  >
    <path fill="currentColor" :d="view.path" />
  </svg>
</template>
