<script setup lang="ts">
import type { ColumnDef } from '@tanstack/vue-table'
import { faChevronRight } from '@fortawesome/free-solid-svg-icons'
import { FontAwesomeIcon } from '@fortawesome/vue-fontawesome'
import { computed, h, ref } from 'vue'

import ClampedRangeNotice from '@/components/ClampedRangeNotice.vue'
import CompactNumber from '@/components/CompactNumber.vue'
import DataTable from '@/components/DataTable.vue'
import EntityLink from '@/components/EntityLink.vue'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { formatCost } from '@/lib/format'
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
}>()

const emit = defineEmits<{ 'update:drill': [value: string] }>()

type Level = { kind: 'group' } | { kind: 'user'; group: string } | { kind: 'usermodel'; user: string }

const level = computed<Level>(() => {
  if (props.drill === '') return { kind: 'group' }
  if (props.drill.startsWith('group:')) return { kind: 'user', group: props.drill.slice('group:'.length) }
  return { kind: 'usermodel', user: props.drill.slice('user:'.length) }
})

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
  if (parentGroupLabel.value) emit('update:drill', `group:${parentGroupLabel.value}`)
}
function onRowClick(id: string): void {
  const l = level.value
  if (l.kind === 'group') {
    emit('update:drill', `group:${id}`)
  } else if (l.kind === 'user' && props.userModelStatsEnabled) {
    parentGroupLabel.value = l.group
    emit('update:drill', `user:${id}`)
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
      <p v-if="showUserModelHint" class="text-sm text-muted-foreground">
        Per-model attribution for this user needs <code class="font-mono text-xs">admin.stats.userModel</code> enabled.
      </p>
      <DataTable
        v-else
        :columns="columns"
        :data="rows"
        :empty-message="level.kind === 'group' ? 'No group has spend in this range.' : 'Nothing in this range.'"
      />
    </CardContent>
  </Card>
</template>
