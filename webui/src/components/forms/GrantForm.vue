<script setup lang="ts">
import { computed, ref } from 'vue'

import SnippetBlock from '@/components/SnippetBlock.vue'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Input } from '@/components/ui/input'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { groupGrantSnippet, isValidName, isValidUserName, userGrantJsonFragment, validatePersonalGrant } from '@/lib/snippets'

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

const targetKind = ref<'group' | 'user'>('group')
const targetName = ref('')
const providersRaw = ref('')
const modelsRaw = ref('')
const mcpServersRaw = ref('')
const agentsRaw = ref('')

/** parseList splits a comma-separated field into trimmed, non-empty, de-duplicated entries — the one shared parser every list field below uses. */
function parseList(raw: string): string[] {
  const seen = new Set<string>()
  for (const part of raw.split(',')) {
    const trimmed = part.trim()
    if (trimmed) seen.add(trimmed)
  }
  return Array.from(seen)
}

const providers = computed(() => parseList(providersRaw.value))
const models = computed(() => parseList(modelsRaw.value))
const mcpServers = computed(() => parseList(mcpServersRaw.value))
const agents = computed(() => parseList(agentsRaw.value))

const targetNames = computed(() => (targetKind.value === 'group' ? props.groupNames : props.userNames))
// A group name follows NAME_PATTERN (auth.go's configNamePattern — a
// route-path-segment safety rule); a user name only needs to be
// non-empty (auth.go's buildEntry, no character restriction at all —
// P3 item 27). isValidUserName, not isValidName, for targetKind === 'user'.
const nameValid = computed(() => (targetKind.value === 'group' ? isValidName(targetName.value.trim()) : isValidUserName(targetName.value)))

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
    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
      <div class="flex flex-col gap-1">
        <label id="grant-target-kind-label" class="text-xs font-medium text-muted-foreground">Target</label>
        <Select :model-value="targetKind" @update:model-value="(v) => (targetKind = v as 'group' | 'user')">
          <SelectTrigger aria-labelledby="grant-target-kind-label"><SelectValue /></SelectTrigger>
          <SelectContent>
            <SelectItem value="group">Group</SelectItem>
            <SelectItem value="user">User (personal grant, existing users.json entry)</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <div class="flex flex-col gap-1">
        <label id="grant-target-name-label" class="text-xs font-medium text-muted-foreground">{{ targetKind === 'group' ? 'Group name' : 'User name' }}</label>
        <Select :model-value="targetName" @update:model-value="(v) => (targetName = String(v))">
          <SelectTrigger aria-labelledby="grant-target-name-label"><SelectValue placeholder="Select…" /></SelectTrigger>
          <SelectContent>
            <SelectItem v-for="name in targetNames" :key="name" :value="name">{{ name }}</SelectItem>
          </SelectContent>
        </Select>
      </div>
    </div>

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

    <SnippetBlock
      v-if="snippet"
      :label="targetKind === 'group' ? 'Paste into middleware.yaml' : `Merge into ${targetName}’s users.json line`"
      :text="snippet"
    />
    <p v-else class="text-sm text-muted-foreground">Pick a target and at least one list to build a snippet.</p>
  </div>
</template>
