<script setup lang="ts">
import { computed } from 'vue'

import GrantForm from '@/components/forms/GrantForm.vue'
import LimitsForm from '@/components/forms/LimitsForm.vue'
import ModelMetaForm from '@/components/forms/ModelMetaForm.vue'
import PricingForm from '@/components/forms/PricingForm.vue'
import UserForm from '@/components/forms/UserForm.vue'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useNavStore } from '@/stores/nav'

/** HELPER_IDS mirrors the Config page's own `helper` hash param (redesign-plan.md section 3.1: `helper=limits|grant|pricing|model|user`) — the single source of truth for which tab is selected and which form renders. */
const HELPER_IDS = ['limits', 'grant', 'pricing', 'model', 'user'] as const
type HelperId = (typeof HELPER_IDS)[number]

const HELPER_LABEL: Record<HelperId, string> = {
  limits: 'Limits',
  grant: 'Access grant',
  pricing: 'Pricing',
  model: 'Model metadata',
  user: 'New user',
}

/**
 * ChangeHelper (redesign-plan.md section 3.4) is the Config page's
 * snippet-building surface: five forms (LimitsForm, GrantForm,
 * PricingForm, ModelMetaForm, UserForm), each producing a paste-ready
 * YAML/users.json fragment via lib/snippets.ts, switched by the page's own
 * `helper` hash param so a shared link opens the same tab.
 */
const props = defineProps<{
  groupNames: string[]
  userNames: string[]
}>()

const nav = useNavStore()

const helper = computed<HelperId>(() => {
  const value = nav.params.helper
  return (HELPER_IDS as readonly string[]).includes(value ?? '') ? (value as HelperId) : 'limits'
})

function setHelper(next: unknown): void {
  if (typeof next !== 'string' || !(HELPER_IDS as readonly string[]).includes(next)) return
  nav.goTo('config', { ...nav.params, helper: next })
}
</script>

<template>
  <div class="flex flex-col gap-4">
    <Tabs :model-value="helper" @update:model-value="setHelper">
      <TabsList>
        <TabsTrigger v-for="id in HELPER_IDS" :key="id" :value="id">{{ HELPER_LABEL[id] }}</TabsTrigger>
      </TabsList>
    </Tabs>

    <LimitsForm v-if="helper === 'limits'" :group-names="props.groupNames" :user-names="props.userNames" />
    <GrantForm v-else-if="helper === 'grant'" :group-names="props.groupNames" :user-names="props.userNames" />
    <PricingForm v-else-if="helper === 'pricing'" />
    <ModelMetaForm v-else-if="helper === 'model'" />
    <UserForm v-else :group-names="props.groupNames" />
  </div>
</template>
