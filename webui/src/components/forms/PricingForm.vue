<script setup lang="ts">
import { computed, ref } from 'vue'

import SnippetBlock from '@/components/SnippetBlock.vue'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Input } from '@/components/ui/input'
import { isValidNonNegative, pricingSnippet } from '@/lib/snippets'

/**
 * PricingForm (ChangeHelper, redesign-plan.md section 3.4) builds a
 * model's per-token pricing override — inputPerM/outputPerM, the two
 * ModelPricing fields (llmgateway.go) that live in the top-level
 * Config.Pricing map (`spec.plugin.llmgateway.pricing`) and OVERRIDE the
 * built-in/LiteLLM-derived price for one model id (pricing.go's
 * billingPriceSource: `override` always wins). This is a DIFFERENT config
 * section from ModelMetaForm.vue's own modelMeta entry — ModelMeta's own
 * cost fields drive metadata exposure only, never billing (see
 * lib/snippets.ts's own ModelMeta doc comment). Both fields are required
 * together — a one-sided price makes no billing sense — unlike
 * ModelMetaForm.vue's independent contextTokens/free fields.
 */
const modelId = ref('')
const inputRaw = ref('')
const outputRaw = ref('')

const modelIdValid = computed(() => modelId.value.trim() !== '')
const inputValue = computed(() => Number(inputRaw.value))
const outputValue = computed(() => Number(outputRaw.value))
const inputValid = computed(() => inputRaw.value.trim() !== '' && isValidNonNegative(inputValue.value))
const outputValid = computed(() => outputRaw.value.trim() !== '' && isValidNonNegative(outputValue.value))

const errors = computed(() => {
  const out: string[] = []
  if (inputRaw.value.trim() !== '' && !inputValid.value) out.push('Input price/MTok must be a non-negative number.')
  if (outputRaw.value.trim() !== '' && !outputValid.value) out.push('Output price/MTok must be a non-negative number.')
  if ((inputValid.value && !outputValid.value) || (!inputValid.value && outputValid.value)) {
    out.push('Set both input and output price/MTok — a one-sided override has no billing meaning.')
  }
  return out
})

const snippet = computed<string | null>(() => {
  if (!modelIdValid.value || !inputValid.value || !outputValid.value) return null
  return pricingSnippet(modelId.value.trim(), { inputPerM: inputValue.value, outputPerM: outputValue.value })
})
</script>

<template>
  <div class="flex flex-col gap-4">
    <div class="flex flex-col gap-1">
      <label for="pricing-model-id" class="text-xs font-medium text-muted-foreground">Model id (provider/model)</label>
      <Input id="pricing-model-id" v-model="modelId" placeholder="openai/gpt-4" class="font-mono" />
    </div>
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <div class="flex flex-col gap-1">
        <label for="pricing-input" class="text-xs font-medium text-muted-foreground">Input price / MTok (USD)</label>
        <Input id="pricing-input" v-model="inputRaw" type="number" min="0" step="any" placeholder="0.50" />
      </div>
      <div class="flex flex-col gap-1">
        <label for="pricing-output" class="text-xs font-medium text-muted-foreground">Output price / MTok (USD)</label>
        <Input id="pricing-output" v-model="outputRaw" type="number" min="0" step="any" placeholder="1.50" />
      </div>
    </div>

    <Alert v-if="errors.length" variant="destructive">
      <AlertDescription>
        <p v-for="(error, index) in errors" :key="index">{{ error }}</p>
      </AlertDescription>
    </Alert>

    <SnippetBlock v-if="snippet" label="Paste into middleware.yaml" :text="snippet" />
    <p v-else class="text-sm text-muted-foreground">Enter a model id and both prices to build a snippet.</p>
  </div>
</template>
