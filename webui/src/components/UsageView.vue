<script setup lang="ts">
import type { SortingState } from '@tanstack/vue-table'
import { getCoreRowModel, getSortedRowModel, useVueTable } from '@tanstack/vue-table'
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { storeToRefs } from 'pinia'

import CompactNumber from '@/components/CompactNumber.vue'
import CostForecastCard from '@/components/CostForecastCard.vue'
import CsvExportButton from '@/components/CsvExportButton.vue'
import ModelChip from '@/components/ModelChip.vue'
import ScopeLink from '@/components/ScopeLink.vue'
import SearchInput from '@/components/SearchInput.vue'
import SortHeaderButton from '@/components/SortHeaderButton.vue'
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from '@/components/ui/accordion'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { valueUpdater } from '@/components/ui/table'
import UsageBar from '@/components/UsageBar.vue'
import UsageTable from '@/components/UsageTable.vue'
import { useNow } from '@/composables/useNow'
import { useSearchQuery } from '@/composables/useSearchQuery'
import { useNavStore } from '@/stores/nav'
import { headroom, monthProgress, projectMonthEnd } from '@/lib/forecast'
import { formatCost, formatExactInt, formatLimits } from '@/lib/format'
import { type ExpandState, clearExpandOverrides, computeExpandedItems, toggleItemExpand } from '@/lib/search-expand'
import { BUDGET_RATIO_LABEL, type BudgetRatio, budgetRatios } from '@/lib/usage-bars'
import { usageColumns } from '@/lib/usage-columns'
import { usageCsv } from '@/lib/usage-csv'
import { groupMatches, groupNameMatches, matchingMembersOfGroup, membersOfGroup, userMatches } from '@/lib/usage-search'
import { useDashboardStore } from '@/stores/dashboard'
import type { AdminUsageEntryView } from '@/types/api'

const dashboard = useDashboardStore()
const usage = computed(() => dashboard.usage)

// The one shared reactive clock every time-dependent read on this tab uses
// (P1, composables/useNow.ts's own doc comment) — CostForecastCard's own
// elapsedFraction, usageColumns()'s `now` parameter (both the real Users/
// Members <table>s below and the headless Groups toolbar's groupColumns),
// and groupCostProjection all read THIS ref, never their own `new Date()`,
// so a long-open tab's cost projections advance together instead of
// freezing at whatever instant each one happened to first run.
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

// --- user/group search filter (operator feature, mirrors
// ProvidersView.vue's model search via the shared SearchInput component and
// useSearchQuery composable — same styling, same clear-button behavior.
// Matching logic lives in lib/usage-search.ts, which ChartsView.vue's own
// scope-picker filter calls directly too (groupMatches, userMatches) — no
// second copy of "does this user/group match?") ---
// Bound to nav.usageQuery so the search text round-trips through the URL hash (#usage?q=…, lib/hash-state.ts).
const { usageQuery } = storeToRefs(useNavStore())
const { query: userQuery, normalized: normalizedQuery, hasQuery } = useSearchQuery(usageQuery)

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
 * This is a DELIBERATE divergence from ProvidersView.vue, not an
 * oversight, and NOT because Providers lacks a provider-name match —
 * it doesn't lack one: providerMatches there (ProvidersView.vue) checks
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

/** groupsEmptyMessage mirrors ProvidersView.vue's aliasEmptyMessage three-way pattern: distinguishes nothing configured from nothing matching the active query, so an empty Groups card never reads the same regardless of why it's empty. */
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
    (u) =>
      userMatches(u, normalizedQuery.value) ||
      (u.groups ?? [u.groupName]).some((g) => g !== undefined && nameMatchedGroupIds.has(g)),
  )
})
/** usersEmptyMessage mirrors groupsEmptyMessage's three-way pattern (see its own doc comment). */
const usersEmptyMessage = computed(() =>
  !usage.value?.users.length ? 'none configured' : hasQuery.value ? `no users match "${userQuery.value}"` : 'none',
)

// --- Groups accordion (operator directive) ---
//
// Groups render as a shadcn-vue Accordion, same component and rationale
// as Providers (ProvidersView.vue) — expanding a group shows its member
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
 * state ProvidersView.vue uses for providers (generalized from
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
// backspacing to empty too — same convention as ProvidersView.vue.
watch(userQuery, (value) => {
  if (value.trim() === '') clearExpandOverrides(groupExpandState)
})

// --- Access block (group-access-display task, review round) ---
//
// GLOB_HINT is the title attribute on every Access label (Providers,
// Models, MCP servers, Agents): group.allowsX (auth.go) matches every one
// of these four configured lists via matchesGlob/path.Match semantics, not
// literal equality — a heading that just says "Models" leaves that
// invisible. One constant, not four copies of the same string (vue.md:
// "if you've written it twice, you owe an abstraction").
const GLOB_HINT = 'Glob pattern — matches like a shell wildcard (*, ?, [...])'

/**
 * isGlobPattern reports whether one configured models entry is a glob
 * pattern rather than a literal model id — the same special characters
 * path.Match recognizes (auth.go's matchesGlob, which group.allowsModel
 * calls). Only Models needs this distinction rendered differently: a
 * literal id gets the copyable ModelChip treatment (it IS a routable id a
 * caller can paste into a request's `model` field); a pattern like
 * "gpt-4*" is not a valid id and must not invite copying it as one, so it
 * renders as a plain, non-copyable Badge instead (see the template below).
 * Providers/MCPServers/Agents render every entry as a plain Badge either
 * way, so they need no such split.
 */
function isGlobPattern(entry: string): boolean {
  return /[*?[]/.test(entry)
}

// --- F1 usage-vs-limit bars (group detail grid) ---
//
// usage-columns.ts's own numericColumn already renders a bar beside each
// TABLE cell that has a configured limit (Users table, and — reused — the
// Members table inside an expanded group). A group's own accordion detail
// grid (the dl/dd block below, distinct from its nested Members table) has
// no per-cell numeric columns at all, so it renders budgetRatios directly
// as one compact row of labeled bars instead — the coordinator brief's own
// phrasing ("Group detail grid ... renders bars from budgetRatios").

// BUDGET_RATIO_LABEL (P10/P11 review fix): imported from lib/usage-bars.ts
// — this used to be a second, independently-declared copy here (BUDGET_LABEL)
// with slightly different text ("tok/day" here vs usage-columns.ts's own
// bar label), the exact DRY drift the coordinator's review flagged. The one
// shared definition also spells out "tokens (in+out)/day" (P10) for the
// tokDay/tokMonth ids — accurate for a screen-reader user, who has no OTHER
// column beside this compact grid's own labeled bar to infer the combined-
// total measurement from.

/** budgetValueText renders one BudgetRatio's "used / limit" detail text (UsageBar's aria-valuetext/title) — cost ids in dollars (formatCost, the micro-USD unit budgetRatios already computed them in), every other id as a plain thousands-separated count. */
function budgetValueText(ratio: BudgetRatio): string {
  if (ratio.id.startsWith('cost')) return `${formatCost(ratio.used)} / ${formatCost(ratio.limit)}`
  return `${formatExactInt(ratio.used)} / ${formatExactInt(ratio.limit)}`
}

/**
 * groupCostProjection (F7) mirrors usage-columns.ts's own costMonthProjColumn
 * math for one group row — the coordinator brief's own phrasing ("also in
 * group grid"): the group detail dl/dd block is not a DataTable column, so
 * it cannot reuse that column's ColumnDef cell renderer directly, but the
 * underlying lib/forecast.ts math (monthProgress/projectMonthEnd/headroom)
 * is identical. `now` is the shared clock (useNow, above) — the SAME
 * instant every other time-dependent read on this tab uses — not a fresh
 * `new Date()` read per call, so this grid's own projection never disagrees
 * with CostForecastCard's or a column's mid-render.
 */
function groupCostProjection(entry: AdminUsageEntryView, now: Date): { text: string; willExceed: boolean } {
  const elapsedFraction = monthProgress(now)
  const projected = projectMonthEnd(entry.costPerMonthMicroUsd, elapsedFraction)
  if (projected === null) return { text: 'not enough data yet', willExceed: false }
  return { text: formatCost(projected), willExceed: headroom(projected, entry.limits?.costPerMonthUSD).willExceed }
}

// --- F8 CSV export ---
//
// Each button exports whatever rows are CURRENTLY visible: Groups export
// in the Groups toolbar's own sort order (sortedGroupRows, already
// computed below for the headless sort toolbar); Users export in the
// Users <table>'s own CURRENT column sort (P6 review fix — this used to
// export in filtered order alone, a self-declared deviation from the plan's
// "exports filtered+sorted rows"). The Users DataTable's sort state is
// LIFTED UP here via DataTable.vue's `v-model:sorting` (that component's
// own doc comment) — usersSorting below is bound two ways: down into the
// rendered UsageTable (template) so a header click writes back here, and
// into a second, headless useVueTable instance (usersTable) that applies
// the identical sort to filteredUsers so usersCsv can read it back in the
// same order the reader is actually looking at — the same "one sort state,
// two consumers" shape sortedGroupRows/groupsTable already used for Groups.
const usersColumns = computed(() => usageColumns('Name', 'Group', groupNameOf, now.value))
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
          <!-- F4: fleet-wide requests denied by a limit check today (types/api.ts's own doc comment) — destructive tint once positive, mirroring usage-columns.ts's own rejDay column convention. -->
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

    <!-- F7: fleet-wide cost forecast, directly under the Total card (coordinator brief). -->
    <CostForecastCard v-if="usage" :mtd-micros="usage.total.costPerMonthMicroUsd" :limits="usage.total.limits" :now="now" />

    <Card>
      <CardHeader>
        <div class="flex items-start justify-between gap-2">
          <div>
            <CardTitle>Groups</CardTitle>
            <CardDescription>Expand a group to see its member users' own usage.</CardDescription>
          </div>
          <CsvExportButton filename="usage-groups.csv" :build="groupsCsv" />
        </div>
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
            <!--
              P2 review fix: ScopeLink moved OUTSIDE AccordionTrigger — a
              real interactive element can never be a valid descendant of
              another interactive element (AccordionTrigger's own <button>,
              reka-ui), so it is now a sibling in this header row instead
              of nested inside the trigger's own slot. `py-2.5` matches
              AccordionTrigger.vue's own vertical padding so the two line
              up, keeping the same visual row the nested version had.
              `header-class="flex-1"` fills the AccordionHeader (<h3>) that
              wraps the trigger button, since AccordionTrigger's own
              `class` prop lands on the inner button, not the header — see
              AccordionTrigger.vue's doc comment. Without it the header
              shrinks to content width, so the chevron drifts left of the
              row's right edge and the empty space to its right stops
              being clickable. The sr-only span gives the toggle button an
              accessible name that names the group, since the visible text
              beside it is just counts.
            -->
            <div class="flex items-center gap-2">
              <ScopeLink class="shrink-0 py-2.5 font-medium" :label="row.original.id" :scope="`group:${row.original.id}`" />
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
              <!--
                Access block (group-access-display task): the group's
                configured provider/model/MCP-server/agent access,
                display-only — not part of the user/group search filter
                above (row.original.providers/models/mcpServers/agents are
                plain display data, never touched by groupMatches/
                userMatches in lib/usage-search.ts). Same dl/dt/dd
                convention as the stat grid immediately below — one
                convention per panel.
              -->
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
                        class="font-mono font-normal"
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
                        <Badge v-else as="span" variant="secondary" class="font-mono font-normal">{{ modelId }}</Badge>
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
                        class="font-mono font-normal"
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
                        class="font-mono font-normal"
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
                  <!-- F7: projected month-end cost, extrapolated from this group's own month-to-date spend (mirrors usage-columns.ts's costMonthProj column). -->
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
              <!-- F1: usage-vs-limit bars, one per configured limit — budgetRatios (lib/usage-bars.ts) returns nothing for an unlimited group or a storeDown one, so this whole row is simply absent then. -->
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
              />
            </AccordionContent>
          </AccordionItem>
        </Accordion>
        <p v-else class="py-6 text-center text-sm text-muted-foreground">{{ groupsEmptyMessage }}</p>
      </CardContent>
    </Card>

    <Card>
      <CardHeader>
        <div class="flex items-start justify-between gap-2">
          <CardTitle>Users</CardTitle>
          <CsvExportButton filename="usage-users.csv" :build="usersCsv" />
        </div>
      </CardHeader>
      <CardContent>
        <UsageTable
          id-label="Name"
          secondary-column-label="Group"
          :entries="filteredUsers"
          :secondary-value="groupNameOf"
          :empty-message="usersEmptyMessage"
          :now="now"
          v-model:sorting="usersSorting"
        />
      </CardContent>
    </Card>
  </div>
</template>
