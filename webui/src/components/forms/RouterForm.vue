<script setup lang="ts">
import { Callout, Input } from '@perfect-design/ui'
import { computed, ref } from 'vue'

import NumberField from '@/components/NumberField.vue'
import SnippetBlock from '@/components/SnippetBlock.vue'
import { isValidName, parseList, routerSnippet, validateRouter } from '@/lib/snippets'
import type { RouterInput } from '@/lib/snippets'

/**
 * RouterForm (ChangeHelper) builds one `smartRouters` entry
 * (SmartRouterConfig, smart_router.go): up to three tiers, cheapest first,
 * and one optional keyword rule. A tier row with an empty name is ignored.
 * Validation lives in lib/snippets.ts's validateRouter, which mirrors the
 * gateway's own startup checks.
 */
const TIER_ROWS = 3

interface TierRow {
  name: string
  minScore: string
  models: string
}

const routerName = ref('')
const tierRows = ref<TierRow[]>(
  Array.from({ length: TIER_ROWS }, (_, i) => ({ name: '', minScore: i === 0 ? '0' : '', models: '' })),
)
const ruleTier = ref('')
const ruleKeywords = ref('')
const selection = ref<'ordered' | 'hash'>('ordered')
const maxAttemptsRaw = ref('')
const contextEscalation = ref(true)

const nameValid = computed(() => isValidName(routerName.value.trim()))

const router = computed<RouterInput>(() => ({
  tiers: tierRows.value
    .filter((row) => row.name.trim() !== '')
    .map((row) => ({
      name: row.name.trim(),
      minScore: row.minScore.trim() === '' ? Number.NaN : Number(row.minScore),
      models: parseList(row.models),
    })),
  keywordRules: ruleTier.value.trim() === '' && ruleKeywords.value.trim() === '' ? [] : [{ tier: ruleTier.value.trim(), keywords: parseList(ruleKeywords.value) }],
  selection: selection.value,
  maxAttempts: maxAttemptsRaw.value.trim() === '' ? 0 : Number(maxAttemptsRaw.value),
  contextEscalation: contextEscalation.value,
}))

const errors = computed(() => {
  const out: string[] = []
  if (routerName.value.trim() !== '' && !nameValid.value) out.push('Router name may only use letters, digits, dot, underscore and hyphen.')
  if (router.value.tiers.length > 0) out.push(...validateRouter(router.value))
  return out
})

const snippet = computed<string | null>(() => {
  if (!nameValid.value || router.value.tiers.length === 0 || errors.value.length > 0) return null
  return routerSnippet(routerName.value.trim(), router.value)
})
</script>

<template>
  <div class="flex flex-col gap-4">
    <div class="flex flex-col gap-1">
      <label for="router-name" class="text-xs font-medium text-muted-foreground">Router name (the model clients request)</label>
      <Input id="router-name" v-model="routerName" placeholder="auto" class="font-mono" />
    </div>

    <div v-for="(row, index) in tierRows" :key="index" class="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_8rem_2fr]">
      <div class="flex flex-col gap-1">
        <label :for="`router-tier-name-${index}`" class="text-xs font-medium text-muted-foreground">Tier {{ index + 1 }} name</label>
        <Input :id="`router-tier-name-${index}`" v-model="row.name" :placeholder="['small', 'medium', 'large'][index]" />
      </div>
      <div class="flex flex-col gap-1">
        <label :for="`router-tier-score-${index}`" class="text-xs font-medium text-muted-foreground">Min score (0-1000)</label>
        <NumberField :id="`router-tier-score-${index}`" v-model="row.minScore" min="0" max="1000" step="1" />
      </div>
      <div class="flex flex-col gap-1">
        <label :for="`router-tier-models-${index}`" class="text-xs font-medium text-muted-foreground">Models (comma-separated)</label>
        <Input :id="`router-tier-models-${index}`" v-model="row.models" placeholder="openai/gpt-5-mini, gx10/current" class="font-mono" />
      </div>
    </div>

    <div class="grid grid-cols-1 gap-3 sm:grid-cols-[1fr_2fr]">
      <div class="flex flex-col gap-1">
        <label for="router-rule-tier" class="text-xs font-medium text-muted-foreground">Keyword rule tier (optional)</label>
        <Input id="router-rule-tier" v-model="ruleTier" placeholder="large" />
      </div>
      <div class="flex flex-col gap-1">
        <label for="router-rule-keywords" class="text-xs font-medium text-muted-foreground">Keywords (comma-separated)</label>
        <Input id="router-rule-keywords" v-model="ruleKeywords" placeholder="prove, architecture review" />
      </div>
    </div>

    <div class="grid grid-cols-1 gap-3 sm:grid-cols-3 sm:items-end">
      <div class="flex flex-col gap-1">
        <label for="router-selection" class="text-xs font-medium text-muted-foreground">Selection within a tier</label>
        <select id="router-selection" v-model="selection" class="h-9 rounded-md border border-input bg-transparent px-3 text-sm">
          <option value="ordered">ordered</option>
          <option value="hash">hash</option>
        </select>
      </div>
      <div class="flex flex-col gap-1">
        <label for="router-max-attempts" class="text-xs font-medium text-muted-foreground">Max attempts (blank = 4)</label>
        <NumberField id="router-max-attempts" v-model="maxAttemptsRaw" min="0" step="1" placeholder="4" />
      </div>
      <div class="flex items-center gap-2">
        <input id="router-context-escalation" v-model="contextEscalation" type="checkbox" class="size-4 rounded border-input" />
        <label for="router-context-escalation" class="text-xs font-medium text-muted-foreground">Skip models with too small a context window</label>
      </div>
    </div>

    <Callout v-if="errors.length" id="router-errors" type="danger" role="alert" size="sm">
        <p v-for="(error, index) in errors" :key="index">{{ error }}</p>
      </Callout>

    <SnippetBlock v-if="snippet" label="Paste into middleware.yaml" :text="snippet" />
    <p v-else class="text-sm text-muted-foreground">Enter a router name and at least one tier with models to build a snippet.</p>
  </div>
</template>
