<script setup lang="ts">
import { NativeSelect } from '@perfect-design/ui'
import { computed, useId } from 'vue'

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

const kindId = `target-picker-kind-${useId()}`
const nameId = `target-picker-name-${useId()}`

const kindOptions = computed(() => [
  { value: 'group', label: 'Group' },
  { value: 'user', label: props.userOptionLabel },
])
const nameOptions = computed(() => props.targetNames.map((name) => ({ value: name, label: name })))

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
      <label :for="kindId" class="text-xs font-medium text-muted-foreground">Target</label>
      <NativeSelect :id="kindId" :model-value="targetKind" :options="kindOptions" @update:model-value="onKindUpdate" />
    </div>
    <div class="flex flex-col gap-1">
      <label :for="nameId" class="text-xs font-medium text-muted-foreground">{{ targetKind === 'group' ? 'Group name' : 'User name' }}</label>
      <NativeSelect :id="nameId" :model-value="targetName" :options="nameOptions" placeholder="Select…" @update:model-value="onNameUpdate" />
      <p v-if="targetNames.length === 0" class="text-xs text-muted-foreground">No {{ targetKind === 'group' ? 'groups' : 'users' }} loaded yet.</p>
    </div>
  </div>
</template>
