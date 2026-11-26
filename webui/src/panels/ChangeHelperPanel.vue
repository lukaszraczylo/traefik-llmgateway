<script setup lang="ts">
import { Button, Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@perfect-design/ui'

import GrantForm from '@/components/forms/GrantForm.vue'
import LimitsForm from '@/components/forms/LimitsForm.vue'
import ModelMetaForm from '@/components/forms/ModelMetaForm.vue'
import PricingForm from '@/components/forms/PricingForm.vue'
import RouterForm from '@/components/forms/RouterForm.vue'
import UserForm from '@/components/forms/UserForm.vue'
import { HELPER_IDS, HELPER_LABEL } from '@/lib/helper-forms'
import type { HelperId } from '@/lib/helper-forms'

defineProps<{ form: HelperId; groupNames: string[]; userNames: string[] }>()
const emit = defineEmits<{ close: []; select: [id: HelperId] }>()

function onOpen(open: boolean): void {
  if (!open) emit('close')
}
</script>

<template>
  <Sheet :open="true" @update:open="onOpen">
    <SheetContent side="right" size="lg" data-panel-kind="helper" :data-panel-id="form">
      <SheetHeader>
        <SheetTitle>Change helper</SheetTitle>
        <SheetDescription>Build a paste-ready snippet for middleware.yaml or users.json. Nothing here changes the running config.</SheetDescription>
      </SheetHeader>
      <div class="min-w-0 flex-1 overflow-y-auto" data-panel-scroll>
        <div class="flex flex-col gap-4 px-1 pb-4">
          <div class="flex flex-wrap gap-1" role="group" aria-label="Snippet form">
            <Button
              v-for="id in HELPER_IDS"
              :key="id"
              type="button"
              size="sm"
              :variant="id === form ? 'default' : 'outline'"
              :aria-pressed="id === form"
              @click="emit('select', id)"
            >
              {{ HELPER_LABEL[id] }}
            </Button>
          </div>
          <LimitsForm v-if="form === 'limits'" :group-names="groupNames" :user-names="userNames" />
          <GrantForm v-else-if="form === 'grant'" :group-names="groupNames" :user-names="userNames" />
          <PricingForm v-else-if="form === 'pricing'" />
          <ModelMetaForm v-else-if="form === 'model'" />
          <RouterForm v-else-if="form === 'router'" />
          <UserForm v-else :group-names="groupNames" />
        </div>
      </div>
    </SheetContent>
  </Sheet>
</template>
