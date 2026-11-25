<script setup lang="ts">
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@perfect-design/ui'

import type { PanelKind } from '@/lib/panel-selection'

defineProps<{ kind: PanelKind; id: string; title: string }>()
const emit = defineEmits<{ close: [] }>()

function onOpen(open: boolean): void {
  if (!open) emit('close')
}
</script>

<template>
  <Sheet :open="true" @update:open="onOpen">
    <SheetContent side="right" size="lg" :data-panel-kind="kind" :data-panel-id="id">
      <SheetHeader>
        <SheetTitle class="truncate">{{ title }}</SheetTitle>
        <SheetDescription class="sr-only">{{ kind }} details for {{ title }}</SheetDescription>
      </SheetHeader>
      <div class="min-w-0 flex-1 overflow-y-auto">
        <slot />
      </div>
    </SheetContent>
  </Sheet>
</template>
