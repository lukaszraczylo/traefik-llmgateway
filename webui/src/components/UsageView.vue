<script setup lang="ts">
import type { SortingState } from '@tanstack/vue-table'
import { getCoreRowModel, getSortedRowModel, useVueTable } from '@tanstack/vue-table'
import { computed, reactive, ref, watch } from 'vue'

import SearchInput from '@/components/SearchInput.vue'
import SortHeaderButton from '@/components/SortHeaderButton.vue'
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from '@/components/ui/accordion'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { valueUpdater } from '@/components/ui/table'
import UsageTable from '@/components/UsageTable.vue'
import { useSearchQuery } from '@/composables/useSearchQuery'
import { formatCost, formatLimits } from '@/lib/format'
import { type ExpandState, clearExpandOverrides, computeExpandedItems, toggleItemExpand } from '@/lib/search-expand'
import { usageColumns } from '@/lib/usage-columns'
import { groupMatches, groupNameMatches, matchingMembersOfGroup, membersOfGroup, userMatches } from '@/lib/usage-search'
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

// --- user/group search filter (operator feature, mirrors
// OverviewView.vue's model search via the shared SearchInput component and
// useSearchQuery composable — same styling, same clear-button behavior.
// Matching logic lives in lib/usage-search.ts, which ChartsView.vue's own
// scope-picker filter calls directly too (groupMatches, userMatches) — no
// second copy of "does this user/group match?") ---
const { query: userQuery, normalized: normalizedQuery, hasQuery } = useSearchQuery()

/**
 * filteredGroups is every group matching the query — own name OR a
 * matching member (lib/usage-search.ts's groupMatches, binding semantics)
 * — or every group when there is no query. This feeds groupsTable's
 * `data()` below directly, so filtering happens upstream of sorting: the
 * Groups sort toolbar and the Accordion both operate on the same
 * (possibly narrowed) list, no separate filtering pass downstream of the
 * table.
 */
const filteredGroups = computed<AdminUsageEntryView[]>(() => {
  const all = usage.value?.groups ?? []
  if (!hasQuery.value) return all
  const users = usage.value?.users ?? []
  return all.filter((g) => groupMatches(g, users, normalizedQuery.value))
})

/**
 * memberOnlyMatchIds is filteredGroups narrowed to groups that matched
 * ONLY via a member, never via their own name — the auto-expand target
 * set (search-expand.ts's computeExpandedItems `matchingIds`).
 *
 * This is a DELIBERATE divergence from OverviewView.vue, not an
 * oversight, and NOT because Overview lacks a provider-name match —
 * it doesn't lack one: providerMatches there (OverviewView.vue) checks
 * each model's full ROUTABLE id, `${provider}/${model}`
 * (lib/format.ts's routableModelId), so a query like "openai" matches
 * every "openai/..." model id and the whole provider auto-expands with
 * every one of them showing. That is correct there — a provider's own
 * model list IS what the search is for, so revealing it is the point,
 * and EVERY provider match (name-flavored or not) auto-expands.
 *
 * Groups differ: a group's own name is already fully legible on its
 * COLLAPSED trigger row (the `<AccordionTrigger>` below) — no expansion
 * needed to read a name match. Auto-expanding every name-matched group too
 * would dump every one of their full member rosters onto the screen for a
 * query that only needed the trigger row to already be visible. A member-ONLY
 * match has no such shortcut — the member that matched stays invisible
 * until the group opens — so that case alone still auto-expands.
 * search-expand.ts itself has no opinion on any of this; it is purely
 * this view's own choice of matchingIds.
 */
const memberOnlyMatchIds = computed<string[]>(() => {
  if (!hasQuery.value) return []
  return filteredGroups.value.filter((g) => !groupNameMatches(g, normalizedQuery.value)).map((g) => g.id)
})

/**
 * visibleMembers implements the members-table distinction (binding
 * semantics): a group matched by its OWN name shows every member
 * (membersOfGroup); a group that matched only via a member shows only
 * the matching ones — the exported, tested matchingMembersOfGroup
 * helper (lib/usage-search.ts), not a re-filter of userMatches here. No
 * active query behaves like a name match — show everyone, today's
 * behavior unchanged. Replaces the former local membersOf helper.
 */
function visibleMembers(group: AdminUsageEntryView): AdminUsageEntryView[] {
  const users = usage.value?.users ?? []
  if (!hasQuery.value || groupNameMatches(group, normalizedQuery.value)) return membersOfGroup(group, users)
  return matchingMembersOfGroup(group, users, normalizedQuery.value)
}

/** groupsEmptyMessage mirrors OverviewView.vue's aliasEmptyMessage three-way pattern: distinguishes nothing configured from nothing matching the active query, so an empty Groups card never reads the same regardless of why it's empty. */
const groupsEmptyMessage = computed(() =>
  !usage.value?.groups.length ? 'none configured' : hasQuery.value ? `no groups match "${userQuery.value}"` : 'none',
)

/**
 * filteredUsers is every user matching the query directly, PLUS every
 * member of a group that matched by NAME (semantics item 5: "a
 * group-name match includes all of that group's member rows"). A group
 * that matched only via ONE member must not pull its other, non-matching
 * members into this flat table too — that narrower distinction is
 * visibleMembers' job, scoped to the nested table inside an expanded
 * group, not this one.
 */
const filteredUsers = computed<AdminUsageEntryView[]>(() => {
  const all = usage.value?.users ?? []
  if (!hasQuery.value) return all
  const groups = usage.value?.groups ?? []
  const nameMatchedGroupIds = new Set(
    groups.filter((g) => groupNameMatches(g, normalizedQuery.value)).map((g) => g.id),
  )
  return all.filter(
    (u) => userMatches(u, normalizedQuery.value) || (u.groupName !== undefined && nameMatchedGroupIds.has(u.groupName)),
  )
})
/** usersEmptyMessage mirrors groupsEmptyMessage's three-way pattern (see its own doc comment). */
const usersEmptyMessage = computed(() =>
  !usage.value?.users.length ? 'none configured' : hasQuery.value ? `no users match "${userQuery.value}"` : 'none',
)

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
// under the Groups heading toggles that sort using the identical
// SortHeaderButton DataTable.vue's real <th>s use (same icons, same
// toggleSorting call, same accessible name), just laid out as a toolbar
// instead of <th>s, because there is no <table> here to put a <th> in.
const groupColumns = usageColumns('Name', 'Members', memberCountOf)
const groupSorting = ref<SortingState>([])
const groupsTable = useVueTable({
  get data() {
    return filteredGroups.value
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

/**
 * TOOLBAR_COLUMN_IDS restricts the toolbar to exactly the fields each
 * trigger row displays (Name, Members, req/day, cost/day) — review fix:
 * exposing all 11 usageColumns() fields as sortable when only 4 are
 * visible per row let the accordion's order silently diverge from what
 * the toolbar visibly claims to control. The full stat set stays
 * available inside each expanded item's own detail grid, just not as a
 * sort key here.
 */
const TOOLBAR_COLUMN_IDS = new Set(['id', 'secondary', 'reqDay', 'costDay'])
const toolbarHeaders = computed(() =>
  groupsTable.getHeaderGroups()[0].headers.filter((header) => TOOLBAR_COLUMN_IDS.has(header.column.id)),
)

/**
 * Which groups are open — driven by the SAME tested lib/search-expand.ts
 * state OverviewView.vue uses for providers (generalized from
 * provider-expand.ts specifically so this view could reuse it, rather
 * than fork a second copy — see that module's own doc comment). The
 * adapter shape is the identical get/set computed pattern
 * expandedProviderValues uses there, translating the Set-based effective
 * state into the string[] Accordion's `type="multiple"` v-model expects.
 */
const groupExpandState: ExpandState = reactive({
  manuallyExpanded: new Set<string>(),
  manuallyCollapsed: new Set<string>(),
})
const expandedGroups = computed<Set<string>>(() =>
  computeExpandedItems(groupExpandState, hasQuery.value, memberOnlyMatchIds.value),
)
function toggleGroup(id: string): void {
  toggleItemExpand(groupExpandState, id, expandedGroups.value.has(id))
}
const expandedGroupValues = computed<string[]>({
  get: () => Array.from(expandedGroups.value),
  set: (newValues) => {
    const next = new Set(newValues)
    for (const id of expandedGroups.value) {
      if (!next.has(id)) toggleGroup(id)
    }
    for (const id of next) {
      if (!expandedGroups.value.has(id)) toggleGroup(id)
    }
  },
})

// A stale manuallyCollapsed suppression from one search must never
// silently carry into a later, unrelated one — see ExpandState's own doc
// comment. Watching userQuery (not just the clear button) covers
// backspacing to empty too — same convention as OverviewView.vue.
watch(userQuery, (value) => {
  if (value.trim() === '') clearExpandOverrides(groupExpandState)
})
</script>

<template>
  <div class="flex flex-col gap-6">
    <SearchInput v-model="userQuery" placeholder="Filter users or groups" class="max-w-sm" />

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
          <SortHeaderButton v-for="header in toolbarHeaders" :key="header.id" :header="header" />
        </div>

        <Accordion v-if="sortedGroupRows.length" v-model="expandedGroupValues" type="multiple" class="rounded-md border px-3">
          <AccordionItem v-for="row in sortedGroupRows" :key="row.original.id" :value="row.original.id">
            <AccordionTrigger>
              <span class="flex flex-1 flex-wrap items-center gap-x-4 gap-y-1 pr-2 text-left">
                <span class="font-medium">{{ row.original.id }}</span>
                <span class="text-xs text-muted-foreground tabular-nums">{{ memberCountOf(row.original) || '0' }} members</span>
                <span class="text-xs text-muted-foreground tabular-nums">
                  {{ row.original.storeDown ? '?' : row.original.requestsPerDay }} req/day
                </span>
                <span class="text-xs text-muted-foreground tabular-nums">
                  {{ row.original.storeDown ? '?' : formatCost(row.original.costPerDayMicroUsd) }}/day
                </span>
              </span>
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
                :entries="visibleMembers(row.original)"
                :secondary-value="groupNameOf"
              />
            </AccordionContent>
          </AccordionItem>
        </Accordion>
        <p v-else class="py-6 text-center text-sm text-muted-foreground">{{ groupsEmptyMessage }}</p>
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
          :entries="filteredUsers"
          :secondary-value="groupNameOf"
          :empty-message="usersEmptyMessage"
        />
      </CardContent>
    </Card>
  </div>
</template>
