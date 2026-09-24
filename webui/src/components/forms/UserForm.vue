<script setup lang="ts">
import { computed, ref } from 'vue'

import LabeledSelect from '@/components/LabeledSelect.vue'
import SnippetBlock from '@/components/SnippetBlock.vue'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { API_KEY_PLACEHOLDER, generateApiKey, isValidUserName, parseList, userJsonLine, validatePersonalGrant } from '@/lib/snippets'
import { useToastsStore } from '@/stores/toasts'

/**
 * UserForm (ChangeHelper, redesign-plan.md section 3.4) builds a whole new
 * users.json line — name, apiKey (placeholder by default, Q10 DECISION;
 * "Generate" produces a real client-side random key, never sent anywhere),
 * a primary group (production's own single-`group` convention) plus an
 * optional comma list of ADDITIONAL groups that switches the emitted field
 * to plural `groups` (lib/snippets.ts's userJsonLine), an optional
 * personal grant (validated the same way GrantForm.vue's user-target path
 * is), and an admin checkbox.
 */
const props = defineProps<{ groupNames: string[] }>()

const groupOptions = computed(() => props.groupNames.map((g) => ({ value: g, label: g })))

const name = ref('')
const apiKey = ref(API_KEY_PLACEHOLDER)
const primaryGroup = ref('')
const extraGroupsRaw = ref('')
const providersRaw = ref('')
const modelsRaw = ref('')
const admin = ref(false)

const toasts = useToastsStore()

/**
 * generate (states-plan.md item 3) confirms a fresh key via toast — unlike
 * the copy-to-clipboard actions elsewhere in this panel, nothing else on
 * screen visibly draws the eye to the API-key input changing (it is one
 * field among many, above the fold but easy to miss), so this one gets a
 * success toast, not just the failure-only treatment ModelChip/
 * SnippetBlock/CsvExportButton use. generateApiKey (lib/snippets.ts) reads
 * crypto.getRandomValues, which can theoretically throw in a context with
 * no Web Crypto — caught defensively rather than left to bubble into an
 * unhandled click-handler rejection.
 */
function generate(): void {
  try {
    apiKey.value = generateApiKey()
    toasts.push({ kind: 'success', message: 'Generated a new API key.' })
  } catch (err) {
    toasts.push({ kind: 'error', message: err instanceof Error ? `Could not generate an API key: ${err.message}` : 'Could not generate an API key.' })
  }
}

const extraGroups = computed(() => parseList(extraGroupsRaw.value))
const providers = computed(() => parseList(providersRaw.value))
const models = computed(() => parseList(modelsRaw.value))

const nameValid = computed(() => isValidUserName(name.value))
const groupValid = computed(() => primaryGroup.value !== '')
const apiKeyValid = computed(() => apiKey.value.trim() !== '' && apiKey.value !== API_KEY_PLACEHOLDER)
const grantError = computed(() => validatePersonalGrant(providers.value, models.value))

const snippet = computed<string | null>(() => {
  if (!nameValid.value || !groupValid.value || !apiKeyValid.value || grantError.value) return null
  const allGroups = extraGroups.value.length > 0 ? [primaryGroup.value, ...extraGroups.value] : null
  return userJsonLine({
    name: name.value.trim(),
    apiKey: apiKey.value.trim(),
    group: allGroups ? undefined : primaryGroup.value,
    groups: allGroups ?? undefined,
    providers: providers.value,
    models: models.value,
    admin: admin.value,
  })
})
</script>

<template>
  <div class="flex flex-col gap-4">
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <div class="flex flex-col gap-1">
        <label for="user-name" class="text-xs font-medium text-muted-foreground">Name</label>
        <Input id="user-name" v-model="name" placeholder="alice" />
      </div>
      <LabeledSelect
        label="Primary group"
        :model-value="primaryGroup"
        :options="groupOptions"
        placeholder="Select…"
        @update:model-value="primaryGroup = $event"
      />
    </div>

    <div class="flex flex-col gap-1">
      <label for="user-api-key" class="text-xs font-medium text-muted-foreground">API key</label>
      <div class="flex gap-2">
        <Input id="user-api-key" v-model="apiKey" class="font-mono" />
        <Button type="button" variant="outline" size="sm" @click="generate">Generate</Button>
      </div>
      <p v-if="apiKey === API_KEY_PLACEHOLDER" class="text-xs text-muted-foreground">Still the placeholder — click Generate for a real key.</p>
    </div>

    <div class="flex flex-col gap-1">
      <label for="user-extra-groups" class="text-xs font-medium text-muted-foreground">Additional groups (comma-separated, optional)</label>
      <Input id="user-extra-groups" v-model="extraGroupsRaw" placeholder="friends" />
    </div>

    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <div class="flex flex-col gap-1">
        <label for="user-providers" class="text-xs font-medium text-muted-foreground">Personal grant: providers (optional)</label>
        <Input id="user-providers" v-model="providersRaw" placeholder="gx10" />
      </div>
      <div class="flex flex-col gap-1">
        <label for="user-models" class="text-xs font-medium text-muted-foreground">Personal grant: models (optional)</label>
        <Input id="user-models" v-model="modelsRaw" placeholder="gx10/current" />
      </div>
    </div>

    <label class="flex items-center gap-2 text-sm">
      <input v-model="admin" type="checkbox" class="size-4 rounded border-input" />
      Admin (full access to /admin/api/*)
    </label>

    <Alert v-if="grantError" variant="destructive">
      <AlertDescription>{{ grantError }}</AlertDescription>
    </Alert>

    <SnippetBlock v-if="snippet" label="Add as a new line in users.json" :text="snippet" />
    <p v-else class="text-sm text-muted-foreground">Fill in a name, group, and a real (generated) API key to build a snippet.</p>
  </div>
</template>
