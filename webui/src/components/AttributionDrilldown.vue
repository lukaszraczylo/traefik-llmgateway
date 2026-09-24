<script setup lang="ts">
import type { ColumnDef } from '@tanstack/vue-table'
import { faChevronRight } from '@fortawesome/free-solid-svg-icons'
import { FontAwesomeIcon } from '@fortawesome/vue-fontawesome'
import { computed, h, ref } from 'vue'

import ClampedRangeNotice from '@/components/ClampedRangeNotice.vue'
import CompactNumber from '@/components/CompactNumber.vue'
import DataTable from '@/components/DataTable.vue'
import EmptyState from '@/components/EmptyState.vue'
import EntityLink from '@/components/EntityLink.vue'
import ErrorState from '@/components/ErrorState.vue'
import SkeletonTable from '@/components/SkeletonTable.vue'
import StatDisabledAlert from '@/components/StatDisabledAlert.vue'
import TryLongerRangeButton from '@/components/TryLongerRangeButton.vue'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { formatCost } from '@/lib/format'
import { loadState } from '@/lib/load-state'
import { drilldownLevel, makeScope } from '@/lib/scope'
import { SPEND_METRIC_LABEL } from '@/stores/spend'
import type { AdminTotalsResponse, HistoryMetric, HistoryWindow } from '@/types/api'

/** DrilldownRow is one GET /admin/api/usage/totals row (types/api.ts's AdminTotalsResponse.rows entry). */
type DrilldownRow = AdminTotalsResponse['rows'][number]

/**
 * AttributionDrilldown (redesign-plan.md section 3.4) descends group ->
 * users (GET /admin/api/usage/totals?kind=user&group=) -> models (kind=
 * usermodel, only when AdminFeaturesView.userModelStats — the config's
 * admin.stats.userModel), breadcrumbs tracked in the `drill` page param
 * (section 3.1). This component owns no fetch itself — CONTROLLED, like
 * CostAvoidedCard.vue's own `ref` prop — stores/spend.ts's fetchDrilldown
 * reads `drill` and supplies `rows`/`metrics`/`disabled`; every click here
 * only emits `update:drill`, letting SpendPage.vue keep it in nav.params
 * (so a reload or shared link restores the same drilldown position).
 *
 * `userModelStatsEnabled` gates whether a USER row is clickable at all
 * (redesign-plan.md's own "only when features.userModelStats, else hint
 * naming the config key") — checked client-side from the already-polled
 * overview response, before ever firing a request that would only come
 * back 404. `disabled` (from the store, a REAL 404) is the fallback for
 * the deep-link case: a reader who lands directly on `drill=user:x` via a
 * restored hash before this prop has had a chance to gate the click.
 */
const props = defineProps<{
  rows: DrilldownRow[]
  metrics: string[]
  drill: string
  disabled: boolean
  userModelStatsEnabled: boolean
  /** The global filters store's current window/span (N3 fix) — ClampedRangeNotice's own props, forwarded so this stays a controlled, props-driven component rather than reaching into stores/filters.ts itself. */
  window: HistoryWindow
  span: number
  /** spend.drilldownLoading (verify-ui-states.md #3 fix) — this card had no loading/error input at all: `spend.drilldownRows` starts as [], so the very first Spend load, and any failed drilldown fetch, rendered the identical "No traffic in this range." a genuinely empty group/user/model list gets. */
  loading: boolean
  /**
   * spend.drilldownLoaded (verify-ui-states-2.md #2 fix) — "has a fetch
   * completed for the current window/span/drill selection", not
   * `rows.length > 0`: the 60s poll flips `loading` true on every tick, and
   * a genuinely empty level (e.g. a group with no users) used to flash a
   * skeleton back on for each tick. `noTrafficInRange` below still checks
   * `rows.length` directly for EmptyState vs. the real table.
   */
  loaded: boolean
  error: string
  /** Renders ErrorState's own "Retry" button — SpendPage.vue's own spend.fetchDrilldown. */
  onRetry?: () => void | Promise<void>
}>()

const emit = defineEmits<{ 'update:drill': [value: string] }>()

const level = computed(() => drilldownLevel(props.drill))

/**
 * parentGroupLabel is a BEST-EFFORT breadcrumb label for the group a
 * drilled-into user belongs to — the `drill` param itself only ever
 * encodes the current LEAF selection (`user:{id}`, redesign-plan.md
 * section 3.1's own `drill=group:x|user:x` shape has no room for a
 * compound path), so this is tracked as local component state, set at
 * the moment a user row is clicked FROM the user-list level. A reader who
 * lands directly on `drill=user:x` via a restored hash sees no middle
 * breadcrumb segment — an acceptable, honest degradation (nothing else
 * available in `drill` records it) rather than a fabricated group name.
 */
const parentGroupLabel = ref('')

function goToGroups(): void {
  emit('update:drill', '')
}
function goToParentGroup(): void {
  if (parentGroupLabel.value) emit('update:drill', makeScope('group', parentGroupLabel.value))
}
function onRowClick(id: string): void {
  const l = level.value
  if (l.kind === 'group') {
    emit('update:drill', makeScope('group', id))
  } else if (l.kind === 'user' && props.userModelStatsEnabled) {
    parentGroupLabel.value = l.group
    emit('update:drill', makeScope('user', id))
  }
}

const idColumnLabel = computed<string>(() => (level.value.kind === 'group' ? 'Group' : level.value.kind === 'user' ? 'User' : 'Model'))

/** Row id is clickable to drill further at the group level always, and at the user level only when userModelStatsEnabled — the leaf (usermodel) level renders the model id as an EntityLink to the Models page instead, since there is nowhere further to drill. */
function idCell(row: DrilldownRow) {
  const l = level.value
  if (l.kind === 'usermodel') return h(EntityLink, { label: row.id, kind: 'model', id: row.id })
  const clickable = l.kind === 'group' || props.userModelStatsEnabled
  if (!clickable) return h('span', {}, row.id)
  return h(
    'button',
    {
      type: 'button',
      class:
        'inline-flex cursor-pointer items-center gap-1 rounded-sm hover:text-primary hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50',
      onClick: () => onRowClick(row.id),
    },
    [row.id, h(FontAwesomeIcon, { icon: faChevronRight, class: 'size-3', 'aria-hidden': 'true' })],
  )
}

const columns = computed<ColumnDef<DrilldownRow, unknown>[]>(() => [
  { id: 'id', header: idColumnLabel.value, accessorFn: (r) => r.id, cell: ({ row }) => idCell(row.original) },
  ...props.metrics.map(
    (metric): ColumnDef<DrilldownRow, unknown> => ({
      id: metric,
      header: SPEND_METRIC_LABEL[metric as HistoryMetric] ?? metric,
      meta: { align: 'right' },
      accessorFn: (r) => r.values[metric] ?? 0,
      cell: ({ row }) => {
        const value = row.original.values[metric] ?? 0
        return metric === 'cost' ? formatCost(value) : h(CompactNumber, { value })
      },
    }),
  ),
])

const showUserModelHint = computed<boolean>(() => level.value.kind === 'user' && (!props.userModelStatsEnabled || props.disabled))

const state = computed(() => loadState({ loading: props.loading, hasData: props.loaded, error: props.error }))

/**
 * noTrafficInRange (states-plan.md item 2: "Spend/Models/Home lists: 'No
 * traffic in this range'") is the genuine "nothing to show" case — gated
 * directly on `rows.length === 0` (verify-ui-states-2.md #2 fix; `state`
 * no longer distinguishes empty from non-empty once `loaded` is true, see
 * this component's own `loaded` prop doc comment), the same way
 * UserDetail.vue's own "Models used" table checks `!modelRows.length`. The
 * template below still checks `state === 'skeleton'`/`'error'` FIRST, so a
 * first/failed fetch never reaches this branch. Never true alongside
 * showUserModelHint, which takes precedence in the template below (that
 * state has its own, more specific copy).
 */
const noTrafficInRange = computed<boolean>(() => !showUserModelHint.value && props.rows.length === 0)
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>Attribution</CardTitle>
      <CardDescription>Drill from groups into users into the models they used.</CardDescription>
      <nav aria-label="Breadcrumb" class="flex flex-wrap items-center gap-1.5 text-sm text-muted-foreground">
        <button type="button" class="hover:text-foreground hover:underline" :disabled="level.kind === 'group'" @click="goToGroups">
          All groups
        </button>
        <template v-if="level.kind === 'user'">
          <FontAwesomeIcon :icon="faChevronRight" class="size-3" aria-hidden="true" />
          <span class="font-medium text-foreground">{{ level.group }}</span>
        </template>
        <template v-if="level.kind === 'usermodel'">
          <FontAwesomeIcon :icon="faChevronRight" class="size-3" aria-hidden="true" />
          <button v-if="parentGroupLabel" type="button" class="hover:text-foreground hover:underline" @click="goToParentGroup">
            {{ parentGroupLabel }}
          </button>
          <FontAwesomeIcon :icon="faChevronRight" class="size-3" aria-hidden="true" />
          <span class="font-medium text-foreground">{{ level.user }}</span>
        </template>
      </nav>
    </CardHeader>
    <CardContent class="flex flex-col gap-3">
      <ClampedRangeNotice v-if="level.kind === 'usermodel'" :window="window" :span="span" />
      <StatDisabledAlert
        v-if="showUserModelHint"
        title="Per-user-model statistics are off"
        config-key="admin.stats.userModel"
        purpose="to see per-model attribution for this user."
      />
      <SkeletonTable v-else-if="state === 'skeleton'" :rows="5" :cols="metrics.length + 1" />
      <ErrorState v-else-if="state === 'error'" :message="error" :on-retry="onRetry" />
      <EmptyState v-else-if="noTrafficInRange" title="No traffic in this range.">
        <TryLongerRangeButton />
      </EmptyState>
      <DataTable v-else :columns="columns" :data="rows" empty-message="none" />
    </CardContent>
  </Card>
</template>
