<script setup lang="ts">
import { computed, reactive } from 'vue'

import TargetPicker from '@/components/forms/TargetPicker.vue'
import SnippetBlock from '@/components/SnippetBlock.vue'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Input } from '@/components/ui/input'
import { useTargetPicker } from '@/composables/useTargetPicker'
import { groupLimitsSnippet, isValidNonNegative, isValidNonNegativeInt, userLimitsJsonFragment } from '@/lib/snippets'
import type { LimitsConfig } from '@/types/api'

/**
 * LimitsForm (ChangeHelper, redesign-plan.md section 3.4) builds a
 * group's or a user's limits snippet — a group's is a full YAML block
 * (lib/snippets.ts's groupLimitsSnippet); a user's own limits live inside
 * their users.json line, so it is a bare JSON field fragment
 * (userLimitsJsonFragment) meant to be merged into that user's existing
 * entry, not a whole new line (UserForm.vue owns building a whole new
 * line).
 */
const props = defineProps<{
  groupNames: string[]
  userNames: string[]
}>()

const { targetKind, targetName, targetNames, nameValid, snippetLabel } = useTargetPicker(
  () => props.groupNames,
  () => props.userNames,
)

/** INTEGER_FIELDS is every LimitsConfig field Go decodes as `int64` (llmgateway.go) — requestsPerMinute/requestsPerDay/tokensPerDay/tokensPerMonth reject a fractional value like "1.5" at json.Unmarshal outright (P2 item 12), unlike costPerDayUSD/costPerMonthUSD, which are `float64`. */
const INTEGER_FIELDS = new Set<keyof LimitsConfig>(['requestsPerMinute', 'requestsPerDay', 'tokensPerDay', 'tokensPerMonth'])

/** fieldValid applies isValidNonNegativeInt to the four int64 fields and isValidNonNegative (fractions allowed) to the two float64 cost fields — the ONE gate both invalidFields and limits below share, so they can never disagree on which fields are int-only. */
function fieldValid(key: keyof LimitsConfig, value: number): boolean {
  return INTEGER_FIELDS.has(key) ? isValidNonNegativeInt(value) : isValidNonNegative(value)
}

const FIELDS: { key: keyof LimitsConfig; label: string }[] = [
  { key: 'requestsPerMinute', label: 'Requests/minute' },
  { key: 'requestsPerDay', label: 'Requests/day' },
  { key: 'tokensPerDay', label: 'Tokens/day (in+out)' },
  { key: 'tokensPerMonth', label: 'Tokens/month (in+out)' },
  { key: 'costPerDayUSD', label: 'Cost/day (USD)' },
  { key: 'costPerMonthUSD', label: 'Cost/month (USD)' },
]

const raw = reactive<Record<keyof LimitsConfig, string>>({
  requestsPerMinute: '',
  requestsPerDay: '',
  tokensPerDay: '',
  tokensPerMonth: '',
  costPerDayUSD: '',
  costPerMonthUSD: '',
})

/** invalidFields lists every field whose entered text is non-empty but fails its own domain check (fieldValid above) — surfaced individually rather than one generic "invalid input" message, so the operator can see exactly which field to fix. Split by INTEGER_FIELDS so the error text is accurate for both: a fraction is invalid for requestsPerMinute etc. but perfectly valid for costPerDayUSD/costPerMonthUSD. */
const invalidFields = computed(() =>
  FIELDS.filter(({ key }) => raw[key].trim() !== '' && !fieldValid(key, Number(raw[key]))).map((f) => f.label),
)
const invalidIntegerFields = computed(() =>
  FIELDS.filter(({ key }) => INTEGER_FIELDS.has(key) && raw[key].trim() !== '' && !fieldValid(key, Number(raw[key]))).map((f) => f.label),
)
const invalidDecimalFields = computed(() =>
  FIELDS.filter(({ key }) => !INTEGER_FIELDS.has(key) && raw[key].trim() !== '' && !fieldValid(key, Number(raw[key]))).map((f) => f.label),
)

const limits = computed<LimitsConfig>(() => {
  const out: LimitsConfig = {}
  for (const { key } of FIELDS) {
    const value = Number(raw[key])
    if (raw[key].trim() !== '' && fieldValid(key, value) && value > 0) out[key] = value
  }
  return out
})

const snippet = computed<string | null>(() => {
  if (!nameValid.value || invalidFields.value.length > 0) return null
  if (Object.keys(limits.value).length === 0) return null
  return targetKind.value === 'group'
    ? groupLimitsSnippet(targetName.value.trim(), limits.value)
    : userLimitsJsonFragment(limits.value)
})
</script>

<template>
  <div class="flex flex-col gap-4">
    <TargetPicker
      :target-kind="targetKind"
      :target-name="targetName"
      :target-names="targetNames"
      user-option-label="User (existing users.json entry)"
      @update:target-kind="targetKind = $event"
      @update:target-name="targetName = $event"
    />

    <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
      <div v-for="field in FIELDS" :key="field.key" class="flex flex-col gap-1">
        <label :for="`limits-${field.key}`" class="text-xs font-medium text-muted-foreground">{{ field.label }}</label>
        <Input
          :id="`limits-${field.key}`"
          v-model="raw[field.key]"
          type="number"
          min="0"
          :step="INTEGER_FIELDS.has(field.key) ? '1' : 'any'"
          placeholder="unset"
        />
      </div>
    </div>

    <Alert v-if="invalidFields.length" variant="destructive">
      <AlertDescription>
        <p v-if="invalidIntegerFields.length">Enter a non-negative whole number for: {{ invalidIntegerFields.join(', ') }}.</p>
        <p v-if="invalidDecimalFields.length">Enter a non-negative number for: {{ invalidDecimalFields.join(', ') }}.</p>
      </AlertDescription>
    </Alert>

    <SnippetBlock v-if="snippet" :label="snippetLabel" :text="snippet" />
    <p v-else class="text-sm text-muted-foreground">Pick a target and at least one limit to build a snippet.</p>
  </div>
</template>
