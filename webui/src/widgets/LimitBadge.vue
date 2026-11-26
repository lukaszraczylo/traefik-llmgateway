<script setup lang="ts">
import { Badge } from '@perfect-design/ui'
import { computed } from 'vue'

import { formatCompactCount } from '@/lib/format'

const props = defineProps<{ limit: number | null | undefined; unit?: string; hot?: boolean }>()

const has = computed(() => typeof props.limit === 'number' && Number.isFinite(props.limit) && props.limit > 0)
const text = computed(() => (has.value ? `${formatCompactCount(props.limit as number)}${props.unit ? `/${props.unit}` : ''}` : 'none'))
</script>

<template>
  <Badge class="wb-limit-badge font-mono" variant="outline" size="sm" :tone="hot ? 'danger' : 'neutral'" :data-limit="has ? 'set' : 'none'">
    {{ text }}
  </Badge>
</template>
