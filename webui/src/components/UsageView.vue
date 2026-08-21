<script setup lang="ts">
import type { SortingState } from '@tanstack/vue-table'
import { faSort, faSortDown, faSortUp } from '@fortawesome/free-solid-svg-icons'
import { FlexRender, getCoreRowModel, getSortedRowModel, useVueTable } from '@tanstack/vue-table'
import { computed, ref } from 'vue'

import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from '@/components/ui/accordion'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { valueUpdater } from '@/components/ui/table'
import UsageTable from '@/components/UsageTable.vue'
import { formatCost, formatLimits } from '@/lib/format'
import { usageColumns } from '@/lib/usage-columns'
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

/**
 * membersOf returns one group's own member users' usage rows — a user's
 * row already carries its own group name (admin.go's
 * adminUsageEntryView.GroupName, set in buildAdminUsage), so this is a
 * plain client-side join against the SAME GET /admin/api/usage response
 * already in the store, no extra fetch. Verified this field already
 * exists and is already asserted by a backend test
 * (admin_test.go's TestAdminUsage_MathAgainstSeededCounters checks
 * alice.GroupName == "agroup") — no backend change was needed for this.
 */
function membersOf(group: AdminUsageEntryView): AdminUsageEntryView[] {
  return (usage.value?.users ?? []).filter((u) => u.groupName === group.id)
}

// --- Groups accordion (operator directive) ---
//
// Groups render as a shadcn-vue Accordion, same component and rationale
// as Providers (OverviewView.vue) — expanding a group shows its member
// users' own usage rows below it. A group's row cannot ALSO be a literal
// <table> row for the identical reason Providers' old expandable row was
// retired: AccordionItem wraps its trigger+content in a <div>, illegal
// directly inside a <tbody>/<tr>.
//
// "Sortable groups usage" (operator directive) is therefore implemented
// headlessly: the SAME usageColumns() defs used by the real <table>-based
// UsageTable (Users, and each group's own nested member list) drive a
// second, unrendered useVueTable instance here purely for its
// getSortedRowModel() — sortedGroupRows below. The small button toolbar
// under the Groups heading toggles that sort exactly like DataTable.vue's
// own sortable header buttons (same icons, same toggleSorting call,
// same aria-sort), just laid out as a toolbar instead of <th>s, because
// there is no <table> here to put a <th> in.
const groupColumns = usageColumns('Name', 'Members', memberCountOf)
const groupSorting = ref<SortingState>([])
const groupsTable = useVueTable({
  get data() {
    return usage.value?.groups ?? []
  },
  get columns() {
    return groupColumns
  },
  getCoreRowModel: getCoreRowModel(),
  getSortedRowModel: getSortedRowModel(),
  onSortingChange: (updater) => valueUpdater(updater, groupSorting),
  state: {
    get sorting() {
      return groupSorting.value
    },
  },
})
const sortedGroupRows = computed(() => groupsTable.getRowModel().rows)

/** ariaSort mirrors DataTable.vue's own helper — kept local since this toolbar isn't a <table> (aria-sort still applies to the toolbar buttons' own semantics, not a <th>, so it's set as a data attribute for styling/testing rather than the real ARIA property here). */
function isSorted(id: string): false | 'asc' | 'desc' {
  return groupsTable.getColumn(id)?.getIsSorted() ?? false
}

/** Which groups are open — plain array state (no search-driven auto-expand exists for this view, unlike Providers, so the two-set provider-expand.ts module is not needed here). */
const expandedGroups = ref<string[]>([])
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
        <CardDescription>Expand a group to see its member users' own usage.</CardDescription>
      </CardHeader>
      <CardContent>
        <div
          v-if="sortedGroupRows.length"
          class="mb-3 flex flex-wrap gap-x-4 gap-y-1 text-xs font-medium text-muted-foreground"
        >
          <button
            v-for="header in groupsTable.getHeaderGroups()[0].headers"
            :key="header.id"
            type="button"
            class="inline-flex items-center gap-1 rounded px-1 py-0.5 hover:text-foreground focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:outline-1 focus-visible:outline-ring"
            :data-sort="isSorted(header.column.id) || 'none'"
            @click="header.column.toggleSorting(isSorted(header.column.id) === 'asc')"
          >
            <FlexRender :render="header.column.columnDef.header" :props="header.getContext()" />
            <FontAwesomeIcon
              :icon="isSorted(header.column.id) === 'asc' ? faSortUp : isSorted(header.column.id) === 'desc' ? faSortDown : faSort"
              class="size-3 shrink-0"
              :class="isSorted(header.column.id) ? 'text-foreground' : 'text-muted-foreground/50'"
              aria-hidden="true"
            />
          </button>
        </div>

        <Accordion v-if="sortedGroupRows.length" v-model="expandedGroups" type="multiple" class="rounded-md border px-3">
          <AccordionItem v-for="row in sortedGroupRows" :key="row.original.id" :value="row.original.id">
            <AccordionTrigger>
              <div class="flex flex-1 flex-wrap items-center gap-x-4 gap-y-1 pr-2 text-left">
                <span class="font-medium">{{ row.original.id }}</span>
                <span class="text-xs text-muted-foreground tabular-nums">{{ memberCountOf(row.original) || '0' }} members</span>
                <span class="text-xs text-muted-foreground tabular-nums">
                  {{ row.original.storeDown ? '?' : row.original.requestsPerDay }} req/day
                </span>
                <span class="text-xs text-muted-foreground tabular-nums">
                  {{ row.original.storeDown ? '?' : formatCost(row.original.costPerDayMicroUsd) }}/day
                </span>
              </div>
            </AccordionTrigger>
            <AccordionContent>
              <dl class="mb-3 grid grid-cols-2 gap-x-6 gap-y-1.5 text-sm sm:grid-cols-4">
                <div>
                  <dt class="text-xs text-muted-foreground">Limits</dt>
                  <dd>{{ formatLimits(row.original.limits) }}</dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">req/min</dt>
                  <dd class="tabular-nums">{{ row.original.storeDown ? '?' : row.original.requestsPerMinute }}</dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">tokIn/day</dt>
                  <dd class="tabular-nums">{{ row.original.storeDown ? '?' : row.original.tokensInPerDay }}</dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">tokOut/day</dt>
                  <dd class="tabular-nums">{{ row.original.storeDown ? '?' : row.original.tokensOutPerDay }}</dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">tokIn/month</dt>
                  <dd class="tabular-nums">{{ row.original.storeDown ? '?' : row.original.tokensInPerMonth }}</dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">tokOut/month</dt>
                  <dd class="tabular-nums">{{ row.original.storeDown ? '?' : row.original.tokensOutPerMonth }}</dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">cost/month</dt>
                  <dd class="tabular-nums">{{ row.original.storeDown ? '?' : formatCost(row.original.costPerMonthMicroUsd) }}</dd>
                </div>
              </dl>
              <p class="mb-1.5 text-xs font-medium text-muted-foreground">Members</p>
              <UsageTable
                id-label="Name"
                secondary-column-label="Group"
                :entries="membersOf(row.original)"
                :secondary-value="groupNameOf"
              />
            </AccordionContent>
          </AccordionItem>
        </Accordion>
        <p v-else class="py-6 text-center text-sm text-muted-foreground">none</p>
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
