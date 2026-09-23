<script setup lang="ts">
import { computed, ref } from 'vue'

import SnippetBlock from '@/components/SnippetBlock.vue'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Input } from '@/components/ui/input'
import { isValidNonNegativeInt, modelMetaSnippet } from '@/lib/snippets'

/**
 * ModelMetaForm (ChangeHelper, redesign-plan.md section 3.4) builds a
 * model's contextTokens/free modelMeta entry — independent fields (unlike
 * PricingForm.vue's paired input/output price), either or both may be
 * set: contextTokens alone documents a window the upstream cannot report;
 * free alone marks a self-hosted model billed as 0 regardless of any
 * discovered/override price (pricing.go: modelMetaFree short-circuits
 * before override/builtin/LiteLLM resolution).
 */
const modelId = ref('')
const contextRaw = ref('')
const free = ref(false)

const modelIdValid = computed(() => modelId.value.trim() !== '')
const contextValue = computed(() => Number(contextRaw.value))
const contextEntered = computed(() => contextRaw.value.trim() !== '')
// ModelMetaConfig.ContextTokens is Go `int` (llmgateway.go) — a fraction
// like "128000.5" fails json.Unmarshal outright, so isValidNonNegativeInt
// (not isValidNonNegative) is the right domain check here (P2 item 12).
const contextValid = computed(() => !contextEntered.value || (isValidNonNegativeInt(contextValue.value) && contextValue.value > 0))

const snippet = computed<string | null>(() => {
  if (!modelIdValid.value || !contextValid.value) return null
  if (!contextEntered.value && !free.value) return null
  return modelMetaSnippet(modelId.value.trim(), {
    contextTokens: contextEntered.value ? contextValue.value : undefined,
    free: free.value || undefined,
  })
})
</script>

<template>
  <div class="flex flex-col gap-4">
    <div class="flex flex-col gap-1">
      <label for="modelmeta-model-id" class="text-xs font-medium text-muted-foreground">Model id (provider/model)</label>
      <Input id="modelmeta-model-id" v-model="modelId" placeholder="gx10/current" class="font-mono" />
    </div>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 sm:items-end">
      <div class="flex flex-col gap-1">
        <label for="modelmeta-context" class="text-xs font-medium text-muted-foreground">Context window (tokens)</label>
        <Input id="modelmeta-context" v-model="contextRaw" type="number" min="1" step="1" placeholder="131072" />
      </div>
      <label class="flex items-center gap-2 pb-1.5 text-sm">
        <input id="modelmeta-free" v-model="free" type="checkbox" class="size-4 rounded border-input" />
        Free (billed as $0 regardless of any resolved price)
      </label>
    </div>

    <Alert v-if="contextEntered && !contextValid" variant="destructive">
      <AlertDescription>Context window must be a positive whole number of tokens.</AlertDescription>
    </Alert>

    <SnippetBlock v-if="snippet" label="Paste into middleware.yaml" :text="snippet" />
    <p v-else class="text-sm text-muted-foreground">Enter a model id and set a context window and/or free to build a snippet.</p>
  </div>
</template>
