<script setup lang="ts">
import { computed, ref } from 'vue'

import TargetPicker from '@/components/forms/TargetPicker.vue'
import SnippetBlock from '@/components/SnippetBlock.vue'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Input } from '@/components/ui/input'
import { useTargetPicker } from '@/composables/useTargetPicker'
import { groupGrantSnippet, parseList, userGrantJsonFragment, validatePersonalGrant } from '@/lib/snippets'

/**
 * GrantForm (ChangeHelper, redesign-plan.md section 3.4) builds a group's
 * access-list snippet (providers/models/mcpServers/agents, YAML) or a
 * user's personal grant (providers/models only — GroupConfig has no
 * per-user mcpServers/agents grant field, auth.go's own UserConfig shape)
 * as a bare JSON fragment for an existing users.json entry.
 *
 * Every list field is a comma-separated text Input rather than a proper
 * multi-select (no such shadcn-vue primitive is installed in this panel) —
 * parsed, trimmed, and de-duplicated below.
 */
const props = defineProps<{
  groupNames: string[]
  userNames: string[]
}>()

const { targetKind, targetName, targetNames, nameValid, snippetLabel } = useTargetPicker(
  () => props.groupNames,
  () => props.userNames,
)
const providersRaw = ref('')
const modelsRaw = ref('')
const mcpServersRaw = ref('')
const agentsRaw = ref('')

const providers = computed(() => parseList(providersRaw.value))
const models = computed(() => parseList(modelsRaw.value))
const mcpServers = computed(() => parseList(mcpServersRaw.value))
const agents = computed(() => parseList(agentsRaw.value))

// validatePersonalGrant is a USER-ONLY rule (lib/snippets.ts's own doc
// comment) — a group's own `providers: []` legitimately means "every
// provider" and Go applies it with no such check at group-construction
// time (auth.go). Gated on targetKind so a group grant with an empty
// providers list + a models list (a perfectly valid group config) no
// longer shows a rejection that does not apply to it (P1 item 13).
const grantError = computed(() => (targetKind.value === 'user' ? validatePersonalGrant(providers.value, models.value) : null))

const isEmpty = computed(
  () => providers.value.length === 0 && models.value.length === 0 && mcpServers.value.length === 0 && agents.value.length === 0,
)

const snippet = computed<string | null>(() => {
  if (!nameValid.value || grantError.value || isEmpty.value) return null
  if (targetKind.value === 'group') {
    return groupGrantSnippet(targetName.value.trim(), {
      providers: providers.value,
      models: models.value,
      mcpServers: mcpServers.value,
      agents: agents.value,
    })
  }
  const fragment = userGrantJsonFragment({ providers: providers.value, models: models.value })
  return fragment || null
})
</script>

<template>
  <div class="flex flex-col gap-4">
    <TargetPicker
      :target-kind="targetKind"
      :target-name="targetName"
      :target-names="targetNames"
      user-option-label="User (personal grant, existing users.json entry)"
      @update:target-kind="targetKind = $event"
      @update:target-name="targetName = $event"
    />

    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <div class="flex flex-col gap-1">
        <label for="grant-providers" class="text-xs font-medium text-muted-foreground">Providers (comma-separated, empty = all)</label>
        <Input id="grant-providers" v-model="providersRaw" placeholder="anthropic, openai" />
      </div>
      <div class="flex flex-col gap-1">
        <label for="grant-models" class="text-xs font-medium text-muted-foreground">Models (comma-separated, empty = all)</label>
        <Input id="grant-models" v-model="modelsRaw" placeholder="anthropic/claude-sonnet-5" />
      </div>
      <template v-if="targetKind === 'group'">
        <div class="flex flex-col gap-1">
          <label for="grant-mcp" class="text-xs font-medium text-muted-foreground">MCP servers (comma-separated, empty = all)</label>
          <Input id="grant-mcp" v-model="mcpServersRaw" placeholder="fetch, weather" />
        </div>
        <div class="flex flex-col gap-1">
          <label for="grant-agents" class="text-xs font-medium text-muted-foreground">Agents (comma-separated, empty = all)</label>
          <Input id="grant-agents" v-model="agentsRaw" placeholder="agentkit" />
        </div>
      </template>
    </div>

    <Alert v-if="grantError" variant="destructive">
      <AlertDescription>{{ grantError }}</AlertDescription>
    </Alert>

    <SnippetBlock v-if="snippet" :label="snippetLabel" :text="snippet" />
    <p v-else class="text-sm text-muted-foreground">Pick a target and at least one list to build a snippet.</p>
  </div>
</template>
