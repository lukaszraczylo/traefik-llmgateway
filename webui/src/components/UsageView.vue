<script setup lang="ts">
import { computed } from 'vue'

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import UsageTable from '@/components/UsageTable.vue'
import { formatCost } from '@/lib/format'
import { useDashboardStore } from '@/stores/dashboard'
import type { AdminUsageEntryView } from '@/types/api'

const dashboard = useDashboardStore()
const usage = computed(() => dashboard.usage)

/** memberCounts maps a group name to its member count — only the Overview response carries it. */
const memberCounts = computed<Record<string, number>>(() => {
  const out: Record<string, number> = {}
  for (const g of dashboard.overview?.groups ?? []) out[g.name] = g.memberCount
  return out
})

function groupNameOf(entry: AdminUsageEntryView): string {
  return entry.groupName ?? ''
}
function memberCountOf(entry: AdminUsageEntryView): string {
  const count = memberCounts.value[entry.id]
  return count === undefined ? '' : String(count)
}
</script>

<template>
  <div class="flex flex-col gap-6">
    <Card v-if="usage">
      <CardHeader>
        <CardTitle>Total</CardTitle>
        <CardDescription>Usage summed across every user and group combined.</CardDescription>
      </CardHeader>
      <CardContent class="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
        <div>
          <p class="text-muted-foreground">Requests/day</p>
          <p class="text-lg font-semibold tabular-nums">{{ usage.total.requestsPerDay }}</p>
        </div>
        <div>
          <p class="text-muted-foreground">Tokens in/day</p>
          <p class="text-lg font-semibold tabular-nums">{{ usage.total.tokensInPerDay }}</p>
        </div>
        <div>
          <p class="text-muted-foreground">Tokens out/day</p>
          <p class="text-lg font-semibold tabular-nums">{{ usage.total.tokensOutPerDay }}</p>
        </div>
        <div>
          <p class="text-muted-foreground">Cost/day</p>
          <p class="text-lg font-semibold tabular-nums">{{ formatCost(usage.total.costPerDayMicroUsd) }}</p>
        </div>
      </CardContent>
    </Card>

    <Card>
      <CardHeader>
        <CardTitle>Groups</CardTitle>
      </CardHeader>
      <CardContent>
        <UsageTable
          id-label="Name"
          secondary-column-label="Members"
          :entries="usage?.groups ?? []"
          :secondary-value="memberCountOf"
        />
      </CardContent>
    </Card>

    <Card>
      <CardHeader>
        <CardTitle>Users</CardTitle>
      </CardHeader>
      <CardContent>
        <UsageTable
          id-label="Name"
          secondary-column-label="Group"
          :entries="usage?.users ?? []"
          :secondary-value="groupNameOf"
        />
      </CardContent>
    </Card>
  </div>
</template>
