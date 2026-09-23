<script setup lang="ts">
import { computed } from 'vue'

import DataTable from '@/components/DataTable.vue'
import EmptyState from '@/components/EmptyState.vue'
import ErrorState from '@/components/ErrorState.vue'
import SearchInput from '@/components/SearchInput.vue'
import SkeletonTable from '@/components/SkeletonTable.vue'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useSearchQuery } from '@/composables/useSearchQuery'
import { loadState } from '@/lib/load-state'
import { composeTargetId, targetColumns } from '@/lib/target-columns'
import type { TargetKind } from '@/lib/target-columns'
import { useDashboardStore } from '@/stores/dashboard'
import type { AdminTargetView } from '@/types/api'

// This component backs the "MCP & Agents" page (redesign-plan.md section
// 3.4, TargetsPage.vue) — GET /admin/api/targets (admin.go:
// adminTargetsResponse, Feature B v0.21).
const dashboard = useDashboardStore()
const targets = computed(() => dashboard.targets)

/**
 * select (redesign-plan.md section 3.4's "TargetCallers.vue expander")
 * fires with the composed "mcp/{name}"/"agent/{name}" id and the target's
 * own display name — TargetsPage.vue listens and renders TargetCallers.vue
 * for whichever target the reader last clicked. Two separate
 * targetColumns() instantiations below (MCP servers, Agents) close over
 * their own fixed `kind` so a click always composes the correct prefix,
 * without this component needing to carry a per-row kind field itself.
 */
const emit = defineEmits<{ select: [id: string, label: string] }>()

function onSelect(kind: TargetKind): (t: AdminTargetView) => void {
  return (t) => emit('select', composeTargetId(kind, t.name), t.name)
}

// Simple, single-field substring match on name — no separate lib/*-search
// module: ProviderHealthPanel.vue's own aliasMatches/providerMatches stay
// inline for the identical reason (one field, one comparison), reserving
// the extracted-and-tested lib/usage-search.ts treatment for
// ConsumerDirectory.vue's genuinely multi-entity (user-or-group-member)
// matching semantics.
const { query, normalized: normalizedQuery, hasQuery } = useSearchQuery()

function nameMatches(t: AdminTargetView): boolean {
  return t.name.toLowerCase().includes(normalizedQuery.value)
}

const filteredMCPServers = computed<AdminTargetView[]>(() => {
  const all = targets.value?.mcpServers ?? []
  return hasQuery.value ? all.filter(nameMatches) : all
})
const filteredAgents = computed<AdminTargetView[]>(() => {
  const all = targets.value?.agents ?? []
  return hasQuery.value ? all.filter(nameMatches) : all
})

/** emptyMessage mirrors ConsumerDirectory.vue's groupsEmptyMessage/usersEmptyMessage three-way pattern: distinguishes nothing configured from nothing matching the active query. */
function emptyMessage(configuredCount: number, filteredCount: number): string {
  if (!configuredCount) return 'none configured'
  if (hasQuery.value && filteredCount === 0) return `no targets match "${query.value}"`
  return 'none'
}

const mcpEmptyMessage = computed(() => emptyMessage(targets.value?.mcpServers.length ?? 0, filteredMCPServers.value.length))
const agentsEmptyMessage = computed(() => emptyMessage(targets.value?.agents.length ?? 0, filteredAgents.value.length))

/**
 * targetsLoadState (lib/load-state.ts) — GET /admin/api/targets used to
 * have NO loading/error handling at all here: `targets` mapped a
 * not-yet-loaded/errored dashboard.targets straight to `[]` (`?? []`
 * above), which DataTable then rendered as a confirmed, empty table —
 * exactly the same class of bug lib/access-matrix.ts's matrixColumnsState
 * fixed for the Access matrix (Task A). 'ready' once dashboard.targets
 * has loaded at least once, even if both lists are genuinely empty —
 * `noTargetsAtAll` below is the SEPARATE "loaded, but nothing configured"
 * check, same split as matrixColumnsState's own loaded-vs-empty
 * precedence.
 */
const targetsLoadState = computed(() => loadState({ loading: true, hasData: dashboard.targets !== null, error: dashboard.error }))

/**
 * noTargetsAtAll (states-plan.md item 2: "MCP & Agents: 'No MCP servers
 * or agents configured'") is true only once targets have genuinely
 * loaded with BOTH lists empty — read off the raw configured counts, not
 * the filtered ones, so an active search query never flips this true (a
 * query matching nothing is `mcpEmptyMessage`/`agentsEmptyMessage`'s own
 * "no targets match" case instead, a routine filter interaction, not a
 * fleet-configuration fact worth the same combined empty-state).
 */
const noTargetsAtAll = computed(
  () => targetsLoadState.value === 'ready' && (targets.value?.mcpServers.length ?? 0) === 0 && (targets.value?.agents.length ?? 0) === 0,
)

// Two separate column-def sets (lib/target-columns.ts) — MCP servers and
// Agents each close over their own fixed `kind` (see onSelect above) so a
// name click composes the right "mcp/…"/"agent/…" id. Stateless ColumnDef
// objects otherwise, safe to reuse across DataTable's two separate
// useVueTable instances (each owns its own sorting state).
const mcpColumns = targetColumns(onSelect('mcp'))
const agentColumns = targetColumns(onSelect('agent'))
</script>

<template>
  <div class="flex flex-col gap-6">
    <SearchInput v-model="query" placeholder="Filter MCP servers or agents" class="max-w-sm" />

    <Card v-if="noTargetsAtAll">
      <CardContent>
        <EmptyState
          title="No MCP servers or agents configured."
          description="Add an mcpServers or agents entry to the middleware config to see them here."
        />
      </CardContent>
    </Card>

    <template v-else>
      <Card>
        <CardHeader>
          <CardTitle>MCP servers</CardTitle>
          <CardDescription>
            Configured MCP proxy targets, the groups allowed to reach them, and their request counters. Click a name to see
            its callers.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <SkeletonTable v-if="targetsLoadState === 'skeleton'" :rows="3" :cols="mcpColumns.length" />
          <ErrorState v-else-if="targetsLoadState === 'error'" :message="dashboard.error" />
          <DataTable v-else :columns="mcpColumns" :data="filteredMCPServers" :empty-message="mcpEmptyMessage" />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Agents</CardTitle>
          <CardDescription>
            Configured A2A agent proxy targets, the groups allowed to reach them, and their request counters. Click a name to
            see its callers.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <SkeletonTable v-if="targetsLoadState === 'skeleton'" :rows="3" :cols="agentColumns.length" />
          <ErrorState v-else-if="targetsLoadState === 'error'" :message="dashboard.error" />
          <DataTable v-else :columns="agentColumns" :data="filteredAgents" :empty-message="agentsEmptyMessage" />
        </CardContent>
      </Card>
    </template>
  </div>
</template>
