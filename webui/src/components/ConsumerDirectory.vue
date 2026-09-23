<script setup lang="ts">
import type { SortingState } from '@tanstack/vue-table'
import { getCoreRowModel, getSortedRowModel, useVueTable } from '@tanstack/vue-table'
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'

import CompactNumber from '@/components/CompactNumber.vue'
import CostForecastCard from '@/components/CostForecastCard.vue'
import CsvExportButton from '@/components/CsvExportButton.vue'
import EmptyState from '@/components/EmptyState.vue'
import ErrorState from '@/components/ErrorState.vue'
import ModelChip from '@/components/ModelChip.vue'
import SearchInput from '@/components/SearchInput.vue'
import SkeletonList from '@/components/SkeletonList.vue'
import SkeletonTable from '@/components/SkeletonTable.vue'
import SortHeaderButton from '@/components/SortHeaderButton.vue'
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from '@/components/ui/accordion'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { valueUpdater } from '@/components/ui/table'
import UsageBar from '@/components/UsageBar.vue'
import UsageTable from '@/components/UsageTable.vue'
import { useNow } from '@/composables/useNow'
import { useSearchQuery } from '@/composables/useSearchQuery'
import { headroom, monthProgress, projectMonthEnd } from '@/lib/forecast'
import { formatCost, formatExactInt, formatLimits } from '@/lib/format'
import { loadState } from '@/lib/load-state'
import { type ExpandState, clearExpandOverrides, computeExpandedItems, toggleItemExpand } from '@/lib/search-expand'
import { BUDGET_RATIO_LABEL, type BudgetRatio, budgetRatios } from '@/lib/usage-bars'
import { usageColumns } from '@/lib/usage-columns'
import { usageCsv } from '@/lib/usage-csv'
import { groupMatches, groupNameMatches, matchingMembersOfGroup, membersOfGroup, userMatches } from '@/lib/usage-search'
import { useConsumersStore } from '@/stores/consumers'
import { useDashboardStore } from '@/stores/dashboard'
import { useNavStore } from '@/stores/nav'
import type { AdminUsageEntryView } from '@/types/api'

/**
 * ConsumerDirectory (redesign-plan.md section 3.4) is the Consumers page's
 * default view: the Users/Groups usage-and-access directory that used to
 * be UsageView.vue's whole tab, moved here verbatim (the Groups accordion
 * + its access block included, per the coordinator's own instruction) with
 * three redesign-era changes: nav.usageQuery -> the page's own `q` hash
 * param (stores/nav.ts no longer special-cases a search string), the id
 * cell's ScopeLink -> EntityLink (usage-columns.ts, opens this row's own
 * UserDetail instead of jumping to the old flat Charts tab), and a new
 * Source column (inline/file, GET /admin/api/consumers) on the flat Users
 * table only.
 */
const dashboard = useDashboardStore()
const consumers = useConsumersStore()
const nav = useNavStore()
const usage = computed(() => dashboard.usage)

/**
 * dashboardLoadState (verify-ui-states.md #3) gates the Groups/Users
 * cards' own skeleton/error — `hasData` is "has GET /admin/api/usage EVER
 * landed", not "are there any groups/users": a fleet with genuinely zero
 * groups/users still reads 'ready' and falls through to the EXISTING
 * noGroupsConfigured/usersEmptyMessage copy below, unchanged. Mirrors
 * HomePage.vue's own identical kpiTilesState convention (verify-ui-states-2.
 * md #11: this comment named the old kpiTilesLoading identifier, since
 * renamed) — both read the same polled dashboard store. Previously
 * `noGroupsConfigured`/
 * `usersEmptyMessage` read `usage.value?.groups/users.length ?? 0 === 0`
 * directly, which was ALSO true before the first successful fetch (or
 * after a failed one) — "No groups/users configured" rendered as a false
 * fleet fact while the real state was "not loaded yet" or "failed".
 */
const dashboardLoadState = computed(() => loadState({ loading: !dashboard.lastUpdated, hasData: usage.value !== null, error: dashboard.error }))

onMounted(() => {
  void consumers.ensureConsumers()
})

// The one shared reactive clock every time-dependent read on this page uses
// (P1, composables/useNow.ts's own doc comment) — CostForecastCard's own
// elapsedFraction, usageColumns()'s `now` parameter (both the real Users/
// Members <table>s below and the headless Groups toolbar's groupColumns),
// and groupCostProjection all read THIS ref, never their own `new Date()`.
const { now, start: startClock, stop: stopClock } = useNow()
onMounted(startClock)
onUnmounted(stopClock)

/** memberCounts maps a group name to its member count — only the Overview response carries it. */
const memberCounts = computed<Record<string, number>>(() => {
  const out: Record<string, number> = {}
  for (const g of dashboard.overview?.groups ?? []) out[g.name] = g.memberCount
  return out
})

/** groupNameOf renders a user's FULL group membership (multi-group support — entry.groups ?? [entry.groupName], never groupName alone, see AdminUsageEntryView.groups' own doc comment), comma-joined for the Group column. */
function groupNameOf(entry: AdminUsageEntryView): string {
  return (entry.groups ?? (entry.groupName === undefined ? [] : [entry.groupName])).join(', ')
}
function memberCountOf(entry: AdminUsageEntryView): string {
  const count = memberCounts.value[entry.id]
  return count === undefined ? '' : String(count)
}

/** sourceOf (Consumers redesign) joins a usage entry's id against GET /admin/api/consumers' own users list — 'inline' or 'file' (admin.go: adminConsumerUser.Source), undefined until that on-demand fetch resolves or for an id it has no match for (a group row never has one). Passed to usage-columns.ts's usageColumns() as `sourceValue` — see that module's sourceColumn doc comment. */
function sourceOf(entry: AdminUsageEntryView): string | undefined {
  return consumers.data?.users.find((u) => u.name === entry.id)?.source
}

// --- user/group search filter (operator feature) ---
// Matching logic lives in lib/usage-search.ts. Bound to nav.params.q so
// the search text round-trips through the URL hash (#consumers?q=...,
// lib/hash-state.ts) — stores/nav.ts's own `params` is opaque, so this
// page owns interpreting/writing its own `q` key directly, rather than
// the old nav.usageQuery special case.
const usageQuery = computed<string>({
  get: () => nav.params.q ?? '',
  set: (value) => nav.goTo('consumers', { ...nav.params, q: value }),
})
const { query: userQuery, normalized: normalizedQuery, hasQuery } = useSearchQuery(usageQuery)

/** filteredGroups is every group matching the query — own name OR a matching member (lib/usage-search.ts's groupMatches) — or every group when there is no query. */
const filteredGroups = computed<AdminUsageEntryView[]>(() => {
  const all = usage.value?.groups ?? []
  if (!hasQuery.value) return all
  const users = usage.value?.users ?? []
  return all.filter((g) => groupMatches(g, users, normalizedQuery.value))
})

/** memberOnlyMatchIds is filteredGroups narrowed to groups that matched ONLY via a member, never via their own name — the auto-expand target set (search-expand.ts's computeExpandedItems `matchingIds`). A group's own name is already legible on its collapsed trigger row, so only a member-only match needs auto-expansion to reveal what matched. */
const memberOnlyMatchIds = computed<string[]>(() => {
  if (!hasQuery.value) return []
  return filteredGroups.value.filter((g) => !groupNameMatches(g, normalizedQuery.value)).map((g) => g.id)
})

/** visibleMembers: a group matched by its OWN name shows every member; a group that matched only via a member shows only the matching ones. */
function visibleMembers(group: AdminUsageEntryView): AdminUsageEntryView[] {
  const users = usage.value?.users ?? []
  if (!hasQuery.value || groupNameMatches(group, normalizedQuery.value)) return membersOfGroup(group, users)
  return matchingMembersOfGroup(group, users, normalizedQuery.value)
}

const groupsEmptyMessage = computed(() =>
  !usage.value?.groups.length ? 'none configured' : hasQuery.value ? `no groups match "${userQuery.value}"` : 'none',
)

/** noGroupsConfigured (states-plan.md item 2: "Consumers: 'No groups configured' + hint to the Config page change helper") — mirrors AccessMatrix.vue's identical own noGroupsConfigured, the Access matrix view's own copy of this same "Consumers" page fact. Only true when genuinely nothing is configured, never while a search query merely matches none (groupsEmptyMessage's own "no groups match" case keeps its plain inline text — a filter producing zero rows is a routine interaction, not a fleet-configuration fact worth the same visual weight). */
const noGroupsConfigured = computed(() => (usage.value?.groups.length ?? 0) === 0 && !hasQuery.value)

/** filteredUsers is every user matching the query directly, PLUS every member of a group that matched by NAME. */
const filteredUsers = computed<AdminUsageEntryView[]>(() => {
  const all = usage.value?.users ?? []
  if (!hasQuery.value) return all
  const groups = usage.value?.groups ?? []
  const nameMatchedGroupIds = new Set(groups.filter((g) => groupNameMatches(g, normalizedQuery.value)).map((g) => g.id))
  return all.filter(
    (u) => userMatches(u, normalizedQuery.value) || (u.groups ?? [u.groupName]).some((g) => g !== undefined && nameMatchedGroupIds.has(g)),
  )
})
const usersEmptyMessage = computed(() =>
  !usage.value?.users.length ? 'none configured' : hasQuery.value ? `no users match "${userQuery.value}"` : 'none',
)

// --- Groups accordion ---
//
// Groups render as a shadcn-vue Accordion — expanding a group shows its
// member users' own usage rows below it. "Sortable groups usage" is
// implemented headlessly: the SAME usageColumns() defs the real <table>-
// based UsageTable uses drive a second, unrendered useVueTable instance
// here purely for its getSortedRowModel() — sortedGroupRows below. The
// small button toolbar under the Groups heading toggles that sort using
// the identical SortHeaderButton DataTable.vue's real <th>s use.
const groupColumns = computed(() => usageColumns('Name', 'Members', memberCountOf, now.value))
const groupSorting = ref<SortingState>([])
const groupsTable = useVueTable({
  get data() {
    return filteredGroups.value
  },
  get columns() {
    return groupColumns.value
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

/** TOOLBAR_COLUMN_IDS restricts the toolbar to exactly the fields each trigger row displays (Name, Members, req/day, cost/day) — the full stat set stays available inside each expanded item's own detail grid, just not as a sort key here. */
const TOOLBAR_COLUMN_IDS = new Set(['id', 'secondary', 'reqDay', 'costDay'])
const toolbarHeaders = computed(() => groupsTable.getHeaderGroups()[0].headers.filter((header) => TOOLBAR_COLUMN_IDS.has(header.column.id)))

/** Which groups are open — driven by the shared tested lib/search-expand.ts state. */
const groupExpandState: ExpandState = reactive({
  manuallyExpanded: new Set<string>(),
  manuallyCollapsed: new Set<string>(),
})
const expandedGroups = computed<Set<string>>(() => computeExpandedItems(groupExpandState, hasQuery.value, memberOnlyMatchIds.value))
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
// comment.
watch(userQuery, (value) => {
  if (value.trim() === '') clearExpandOverrides(groupExpandState)
})

// --- Access block (moved from UsageView.vue, unchanged) ---
const GLOB_HINT = 'Glob pattern — matches like a shell wildcard (*, ?, [...])'

/** isGlobPattern reports whether one configured models entry is a glob pattern rather than a literal model id — the same special characters path.Match recognizes (auth.go's matchesGlob). */
function isGlobPattern(entry: string): boolean {
  return /[*?[]/.test(entry)
}

/** budgetValueText renders one BudgetRatio's "used / limit" detail text (UsageBar's aria-valuetext/title). */
function budgetValueText(ratio: BudgetRatio): string {
  if (ratio.id.startsWith('cost')) return `${formatCost(ratio.used)} / ${formatCost(ratio.limit)}`
  return `${formatExactInt(ratio.used)} / ${formatExactInt(ratio.limit)}`
}

/** groupCostProjection mirrors usage-columns.ts's own costMonthProjColumn math for one group row. */
function groupCostProjection(entry: AdminUsageEntryView, now: Date): { text: string; willExceed: boolean } {
  const elapsedFraction = monthProgress(now)
  const projected = projectMonthEnd(entry.costPerMonthMicroUsd, elapsedFraction)
  if (projected === null) return { text: 'not enough data yet', willExceed: false }
  return { text: formatCost(projected), willExceed: headroom(projected, entry.limits?.costPerMonthUSD).willExceed }
}

// --- CSV export ---
const usersColumns = computed(() => usageColumns('Name', 'Group', groupNameOf, now.value, sourceOf))
const usersSorting = ref<SortingState>([])
const usersTable = useVueTable({
  get data() {
    return filteredUsers.value
  },
  get columns() {
    return usersColumns.value
  },
  getCoreRowModel: getCoreRowModel(),
  getSortedRowModel: getSortedRowModel(),
  onSortingChange: (updater) => valueUpdater(updater, usersSorting),
  state: {
    get sorting() {
      return usersSorting.value
    },
  },
})

function usersCsv(): string {
  return usageCsv(
    usersTable.getRowModel().rows.map((row) => row.original),
    'users',
    groupNameOf,
    now.value,
  )
}
function groupsCsv(): string {
  return usageCsv(
    sortedGroupRows.value.map((row) => row.original),
    'groups',
    memberCountOf,
    now.value,
  )
}
</script>

<template>
  <div class="flex flex-col gap-6">
    <SearchInput v-model="userQuery" placeholder="Filter users or groups" class="max-w-sm" />

    <Card v-if="usage">
      <CardHeader>
        <CardTitle>Total</CardTitle>
        <CardDescription>Usage summed across every user and group combined.</CardDescription>
      </CardHeader>
      <CardContent class="grid grid-cols-2 gap-4 text-sm sm:grid-cols-5">
        <div>
          <p class="text-muted-foreground">Requests/day</p>
          <p class="text-lg font-semibold tabular-nums"><CompactNumber :value="usage.total.requestsPerDay" /></p>
        </div>
        <div>
          <p class="text-muted-foreground">Rejected/day</p>
          <p class="text-lg font-semibold tabular-nums" :class="usage.total.rejectionsPerDay > 0 ? 'text-destructive' : undefined">
            <CompactNumber :value="usage.total.rejectionsPerDay" />
          </p>
        </div>
        <div>
          <p class="text-muted-foreground">Tokens in/day</p>
          <p class="text-lg font-semibold tabular-nums"><CompactNumber :value="usage.total.tokensInPerDay" /></p>
        </div>
        <div>
          <p class="text-muted-foreground">Tokens out/day</p>
          <p class="text-lg font-semibold tabular-nums"><CompactNumber :value="usage.total.tokensOutPerDay" /></p>
        </div>
        <div>
          <p class="text-muted-foreground">Cost/day</p>
          <p class="text-lg font-semibold tabular-nums">{{ formatCost(usage.total.costPerDayMicroUsd) }}</p>
        </div>
      </CardContent>
    </Card>

    <CostForecastCard v-if="usage" :mtd-micros="usage.total.costPerMonthMicroUsd" :limits="usage.total.limits" :now="now" />

    <Card>
      <CardHeader>
        <div class="flex items-start justify-between gap-2">
          <div>
            <CardTitle>Groups</CardTitle>
            <CardDescription>Expand a group to see its member users' own usage.</CardDescription>
          </div>
          <CsvExportButton filename="consumers-groups.csv" :build="groupsCsv" />
        </div>
      </CardHeader>
      <CardContent>
        <SkeletonList v-if="dashboardLoadState === 'skeleton'" :rows="3" />
        <ErrorState v-else-if="dashboardLoadState === 'error'" :message="dashboard.error" :on-retry="dashboard.refresh" />
        <template v-else>
        <div v-if="sortedGroupRows.length" class="mb-3 flex flex-wrap gap-x-4 gap-y-1 text-xs font-medium text-muted-foreground">
          <SortHeaderButton v-for="header in toolbarHeaders" :key="header.id" :header="header" />
        </div>

        <Accordion v-if="sortedGroupRows.length" v-model="expandedGroupValues" type="multiple" class="rounded-md border px-3">
          <AccordionItem v-for="row in sortedGroupRows" :key="row.original.id" :value="row.original.id">
            <div class="flex items-center gap-2">
              <span class="shrink-0 py-2.5 font-medium">{{ row.original.id }}</span>
              <AccordionTrigger header-class="flex-1">
                <span class="sr-only">Group {{ row.original.id }} details</span>
                <span class="flex flex-1 flex-wrap items-center gap-x-4 gap-y-1 pr-2 text-left">
                  <span class="text-xs text-muted-foreground tabular-nums">{{ memberCountOf(row.original) || '0' }} members</span>
                  <span class="text-xs text-muted-foreground tabular-nums">
                    <template v-if="row.original.storeDown">?</template>
                    <CompactNumber v-else :value="row.original.requestsPerDay" /> req/day
                  </span>
                  <span class="text-xs text-muted-foreground tabular-nums">
                    {{ row.original.storeDown ? '?' : formatCost(row.original.costPerDayMicroUsd) }}/day
                  </span>
                </span>
              </AccordionTrigger>
            </div>
            <AccordionContent>
              <dl class="mb-3 grid grid-cols-2 gap-x-6 gap-y-1.5 text-sm sm:grid-cols-4">
                <div>
                  <dt class="text-xs text-muted-foreground" :title="GLOB_HINT">Providers</dt>
                  <dd>
                    <div v-if="row.original.providers?.length" class="flex flex-wrap gap-1.5">
                      <Badge
                        v-for="providerName in row.original.providers"
                        :key="providerName"
                        as="span"
                        variant="secondary"
                        class="max-w-full truncate font-mono font-normal"
                        :title="providerName"
                      >
                        {{ providerName }}
                      </Badge>
                    </div>
                    <span v-else class="text-muted-foreground">All providers</span>
                  </dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground" :title="GLOB_HINT">Models</dt>
                  <dd>
                    <div v-if="row.original.models?.length" class="flex flex-wrap gap-1.5">
                      <template v-for="modelId in row.original.models" :key="modelId">
                        <ModelChip v-if="!isGlobPattern(modelId)" :id="modelId" />
                        <Badge v-else as="span" variant="secondary" class="max-w-full truncate font-mono font-normal" :title="modelId">{{ modelId }}</Badge>
                      </template>
                    </div>
                    <span v-else class="text-muted-foreground">All models</span>
                  </dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground" :title="GLOB_HINT">MCP servers</dt>
                  <dd>
                    <div v-if="row.original.mcpServers?.length" class="flex flex-wrap gap-1.5">
                      <Badge
                        v-for="serverName in row.original.mcpServers"
                        :key="serverName"
                        as="span"
                        variant="secondary"
                        class="max-w-full truncate font-mono font-normal"
                        :title="serverName"
                      >
                        {{ serverName }}
                      </Badge>
                    </div>
                    <span v-else class="text-muted-foreground">All MCP servers</span>
                  </dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground" :title="GLOB_HINT">Agents</dt>
                  <dd>
                    <div v-if="row.original.agents?.length" class="flex flex-wrap gap-1.5">
                      <Badge
                        v-for="agentName in row.original.agents"
                        :key="agentName"
                        as="span"
                        variant="secondary"
                        class="max-w-full truncate font-mono font-normal"
                        :title="agentName"
                      >
                        {{ agentName }}
                      </Badge>
                    </div>
                    <span v-else class="text-muted-foreground">All agents</span>
                  </dd>
                </div>
              </dl>
              <dl class="mb-3 grid grid-cols-2 gap-x-6 gap-y-1.5 text-sm sm:grid-cols-4">
                <div>
                  <dt class="text-xs text-muted-foreground">Limits</dt>
                  <dd>{{ formatLimits(row.original.limits) }}</dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">req/min</dt>
                  <dd class="tabular-nums">
                    <template v-if="row.original.storeDown">?</template>
                    <CompactNumber v-else :value="row.original.requestsPerMinute" />
                  </dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">rejected/day</dt>
                  <dd class="tabular-nums" :class="!row.original.storeDown && row.original.rejectionsPerDay > 0 ? 'text-destructive' : undefined">
                    <template v-if="row.original.storeDown">?</template>
                    <CompactNumber v-else :value="row.original.rejectionsPerDay" />
                  </dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">tokIn/day</dt>
                  <dd class="tabular-nums">
                    <template v-if="row.original.storeDown">?</template>
                    <CompactNumber v-else :value="row.original.tokensInPerDay" />
                  </dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">tokOut/day</dt>
                  <dd class="tabular-nums">
                    <template v-if="row.original.storeDown">?</template>
                    <CompactNumber v-else :value="row.original.tokensOutPerDay" />
                  </dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">tokIn/month</dt>
                  <dd class="tabular-nums">
                    <template v-if="row.original.storeDown">?</template>
                    <CompactNumber v-else :value="row.original.tokensInPerMonth" />
                  </dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">tokOut/month</dt>
                  <dd class="tabular-nums">
                    <template v-if="row.original.storeDown">?</template>
                    <CompactNumber v-else :value="row.original.tokensOutPerMonth" />
                  </dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">cost/month</dt>
                  <dd class="tabular-nums">{{ row.original.storeDown ? '?' : formatCost(row.original.costPerMonthMicroUsd) }}</dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">cost/month (proj.)</dt>
                  <dd
                    class="tabular-nums"
                    :class="!row.original.storeDown && groupCostProjection(row.original, now).willExceed ? 'text-destructive' : undefined"
                    :title="!row.original.storeDown && groupCostProjection(row.original, now).willExceed ? 'Projected to exceed the configured cost/month limit' : undefined"
                  >
                    {{ row.original.storeDown ? '?' : groupCostProjection(row.original, now).text }}
                  </dd>
                </div>
              </dl>
              <div v-if="budgetRatios(row.original).length" class="mb-3 flex flex-wrap items-center gap-x-4 gap-y-1.5">
                <span
                  v-for="ratio in budgetRatios(row.original)"
                  :key="ratio.id"
                  class="inline-flex items-center gap-1.5 text-xs text-muted-foreground"
                >
                  {{ BUDGET_RATIO_LABEL[ratio.id] }}
                  <UsageBar :ratio="ratio.ratio" :label="BUDGET_RATIO_LABEL[ratio.id]" :value-text="budgetValueText(ratio)" />
                </span>
              </div>
              <p class="mb-1.5 text-xs font-medium text-muted-foreground">Members</p>
              <UsageTable
                id-label="Name"
                secondary-column-label="Group"
                :entries="visibleMembers(row.original)"
                :secondary-value="groupNameOf"
                :now="now"
                :source-value="sourceOf"
              />
            </AccordionContent>
          </AccordionItem>
        </Accordion>
        <EmptyState v-else-if="noGroupsConfigured" title="No groups configured." description="Grant a group access from the Config page's change helper.">
          <Button type="button" variant="outline" size="sm" @click="nav.goTo('config', { helper: 'grant' })">Open change helper</Button>
        </EmptyState>
        <p v-else class="py-6 text-center text-sm text-muted-foreground">{{ groupsEmptyMessage }}</p>
        </template>
      </CardContent>
    </Card>

    <Card>
      <CardHeader>
        <div class="flex items-start justify-between gap-2">
          <div>
            <CardTitle>Users</CardTitle>
            <CardDescription v-if="consumers.data?.usersFile">File-backed users load from {{ consumers.data.usersFile }}.</CardDescription>
          </div>
          <CsvExportButton filename="consumers-users.csv" :build="usersCsv" />
        </div>
      </CardHeader>
      <CardContent>
        <SkeletonTable v-if="dashboardLoadState === 'skeleton'" :rows="5" :cols="5" />
        <ErrorState v-else-if="dashboardLoadState === 'error'" :message="dashboard.error" :on-retry="dashboard.refresh" />
        <UsageTable
          v-else
          id-label="Name"
          secondary-column-label="Group"
          :entries="filteredUsers"
          :secondary-value="groupNameOf"
          :empty-message="usersEmptyMessage"
          :now="now"
          :source-value="sourceOf"
          v-model:sorting="usersSorting"
        />
      </CardContent>
    </Card>
  </div>
</template>
