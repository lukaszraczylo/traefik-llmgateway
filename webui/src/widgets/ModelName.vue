<script setup lang="ts">
import { Badge } from '@perfect-design/ui'
import { computed } from 'vue'

import { splitModelId } from '@/lib/model-id'
import { useCatalogStore } from '@/stores/catalog'

const props = defineProps<{ id: string; bare?: boolean }>()
const parts = computed(() => splitModelId(props.id))
const catalog = useCatalogStore()
const target = computed(() => catalog.aliasTargets.get(props.id) ?? null)
</script>

<template>
  <span class="wb-model" :title="target ? `${id} → ${target}` : id">
    <span v-if="parts.provider && !bare" class="wb-model-provider">{{ parts.provider }}</span>
    <span class="wb-model-name">{{ parts.model }}</span>
    <Badge v-if="target" tone="neutral" variant="outline" size="sm" class="wb-model-alias ml-1 min-w-0 truncate font-mono">alias → {{ target }}</Badge>
  </span>
</template>
