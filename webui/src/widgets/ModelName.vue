<script setup lang="ts">
import { computed } from 'vue'

import { splitModelId } from '@/lib/model-id'

const props = defineProps<{ id: string; bare?: boolean }>()
const parts = computed(() => splitModelId(props.id))
</script>

<template>
  <span class="wb-model" :title="id">
    <span v-if="parts.provider && !bare" class="wb-model-provider">{{ parts.provider }}</span>
    <span class="wb-model-name">{{ parts.model }}</span>
  </span>
</template>
