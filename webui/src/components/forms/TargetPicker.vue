<script setup lang="ts">
import { useId } from 'vue'

import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import type { GrantTargetKind } from '@/composables/useTargetPicker'

/**
 * TargetPicker (reuse-audit.md F12) is the "Target kind, then Group or
 * User name" two-Select block GrantForm.vue and LimitsForm.vue each
 * hand-rolled — pairs with the composables/useTargetPicker.ts composable,
 * which owns the state/validation this component only renders.
 * `userOptionLabel` stays a prop rather than a hardcoded string: GrantForm
 * ("User (personal grant, existing users.json entry)") and LimitsForm
 * ("User (existing users.json entry)") word the "user" option differently,
 * and unifying that wording is outside this refactor's scope.
 */
const props = defineProps<{
  targetKind: GrantTargetKind
  targetName: string
  targetNames: string[]
  userOptionLabel: string
}>()

const emit = defineEmits<{
  'update:targetKind': [value: GrantTargetKind]
  'update:targetName': [value: string]
}>()

const kindLabelId = `target-picker-kind-${useId()}`
const nameLabelId = `target-picker-name-${useId()}`

function onKindUpdate(value: unknown): void {
  if (value === 'group' || value === 'user') emit('update:targetKind', value)
}
function onNameUpdate(value: unknown): void {
  emit('update:targetName', String(value))
}
</script>

<template>
  <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
    <div class="flex flex-col gap-1">
      <label :id="kindLabelId" class="text-xs font-medium text-muted-foreground">Target</label>
      <Select :model-value="targetKind" @update:model-value="onKindUpdate">
        <SelectTrigger :aria-labelledby="kindLabelId"><SelectValue /></SelectTrigger>
        <SelectContent>
          <SelectItem value="group">Group</SelectItem>
          <SelectItem value="user">{{ props.userOptionLabel }}</SelectItem>
        </SelectContent>
      </Select>
    </div>
    <div class="flex flex-col gap-1">
      <label :id="nameLabelId" class="text-xs font-medium text-muted-foreground">{{ targetKind === 'group' ? 'Group name' : 'User name' }}</label>
      <Select :model-value="targetName" @update:model-value="onNameUpdate">
        <SelectTrigger :aria-labelledby="nameLabelId"><SelectValue placeholder="Select…" /></SelectTrigger>
        <SelectContent>
          <SelectItem v-for="name in targetNames" :key="name" :value="name">{{ name }}</SelectItem>
        </SelectContent>
      </Select>
    </div>
  </div>
</template>
