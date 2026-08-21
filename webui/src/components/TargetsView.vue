<script setup lang="ts">
import { computed } from 'vue'

import DataTable from '@/components/DataTable.vue'
import SearchInput from '@/components/SearchInput.vue'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useSearchQuery } from '@/composables/useSearchQuery'
import { targetColumns } from '@/lib/target-columns'
import { useDashboardStore } from '@/stores/dashboard'
import type { AdminTargetView } from '@/types/api'

// This component backs the "MCP & Agents" tab (App.vue) — GET
// /admin/api/targets (admin.go: adminTargetsResponse, Feature B v0.21).
const dashboard = useDashboardStore()
const targets = computed(() => dashboard.targets)

// Simple, single-field substring match on name — no separate lib/*-search
// module: ProvidersView.vue's own aliasMatches/providerMatches stay inline
// for the identical reason (one field, one comparison), reserving the
// extracted-and-tested lib/usage-search.ts treatment for UsageView.vue's
// genuinely multi-entity (user-or-group-member) matching semantics.
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

/** emptyMessage mirrors UsageView.vue's groupsEmptyMessage/usersEmptyMessage three-way pattern: distinguishes nothing configured from nothing matching the active query. */
function emptyMessage(configuredCount: number, filteredCount: number): string {
  if (!configuredCount) return 'none configured'
  if (hasQuery.value && filteredCount === 0) return `no targets match "${query.value}"`
  return 'none'
}

const mcpEmptyMessage = computed(() => emptyMessage(targets.value?.mcpServers.length ?? 0, filteredMCPServers.value.length))
const agentsEmptyMessage = computed(() => emptyMessage(targets.value?.agents.length ?? 0, filteredAgents.value.length))

// One shared column-def set (lib/target-columns.ts) for both tables below —
// stateless ColumnDef objects, safe to reuse across DataTable's two
// separate useVueTable instances (each owns its own sorting state).
const columns = targetColumns()
</script>

<template>
  <div class="flex flex-col gap-6">
    <SearchInput v-model="query" placeholder="Filter MCP servers or agents" class="max-w-sm" />

    <Card>
      <CardHeader>
        <CardTitle>MCP servers</CardTitle>
        <CardDescription>Configured MCP proxy targets, the groups allowed to reach them, and their request counters.</CardDescription>
      </CardHeader>
      <CardContent>
        <DataTable :columns="columns" :data="filteredMCPServers" :empty-message="mcpEmptyMessage" />
      </CardContent>
    </Card>

    <Card>
      <CardHeader>
        <CardTitle>Agents</CardTitle>
        <CardDescription>Configured A2A agent proxy targets, the groups allowed to reach them, and their request counters.</CardDescription>
      </CardHeader>
      <CardContent>
        <DataTable :columns="columns" :data="filteredAgents" :empty-message="agentsEmptyMessage" />
      </CardContent>
    </Card>
  </div>
</template>
