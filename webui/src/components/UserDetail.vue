<script setup lang="ts">
import { faArrowLeft } from '@fortawesome/free-solid-svg-icons'
import { computed, onMounted, onUnmounted, watch } from 'vue'

import ClampedRangeNotice from '@/components/ClampedRangeNotice.vue'
import CompactNumber from '@/components/CompactNumber.vue'
import ModelChip from '@/components/ModelChip.vue'
import TimeSeriesChart from '@/components/TimeSeriesChart.vue'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import UsageBar from '@/components/UsageBar.vue'
import { useNow } from '@/composables/useNow'
import { runOutDate as computeRunOutDate } from '@/lib/burndown'
import { formatBucketLabel, formatCost, formatExactInt, formatLimits, formatTimestamp } from '@/lib/format'
import { BUDGET_RATIO_LABEL, budgetRatios } from '@/lib/usage-bars'
import { useConsumersStore } from '@/stores/consumers'
import { useDashboardStore } from '@/stores/dashboard'
import { useEventsStore } from '@/stores/events'
import { useFiltersStore } from '@/stores/filters'

/**
 * UserDetail (redesign-plan.md section 3.4) is the Consumers page's
 * `user=<name>` drill-down: a timeline of the user's own req/cost series,
 * their per-model breakdown (usermodel totals, when admin.stats.userModel
 * is on), recent errors filtered from the shared Events store, headroom
 * bars + an estimated run-out date, and their limits/grants.
 */
const props = defineProps<{ userId: string }>()
const emit = defineEmits<{ close: [] }>()

const dashboard = useDashboardStore()
const consumers = useConsumersStore()
const events = useEventsStore()
const filters = useFiltersStore()
const { now, start: startClock, stop: stopClock } = useNow()
onMounted(startClock)
onUnmounted(stopClock)

/** entry is this user's own row from the polled /admin/api/usage response — live counters, limits, lastSeen. undefined for a configured user who has never made a request (no usage scope created yet) or an id that does not exist at all. */
const entry = computed(() => dashboard.usage?.users.find((u) => u.id === props.userId))

/** consumerUser is this user's own row from GET /admin/api/consumers — source, admin flag, personal grant. */
const consumerUser = computed(() => consumers.data?.users.find((u) => u.name === props.userId))

const featuresUserModelStats = computed(() => dashboard.overview?.features.userModelStats ?? false)

function loadDetail(): void {
  void consumers.fetchUserDetail(props.userId, filters.window, filters.span, featuresUserModelStats.value)
}

onMounted(() => {
  loadDetail()
  void events.refresh()
})
// featuresUserModelStats is included alongside userId/range: on a deep
// link or reload (#consumers?user=x), this component mounts and fires
// loadDetail() BEFORE the polled overview (stores/dashboard.ts) has
// necessarily landed its first response, so the very first loadDetail()
// call can run with userModelStatsEnabled still false even though the
// feature really is on. Once the overview lands and that computed flips
// true, this watch refetches — otherwise "Models used" stayed
// permanently empty for the rest of the session even after the alert
// telling the reader to enable it disappeared (P2).
watch([() => props.userId, () => filters.range, featuresUserModelStats], loadDetail)

const detail = computed(() => consumers.detail[props.userId])

// Labels are built from the RESPONSE's own `window` (AdminSeriesResponse),
// not from the current filters.window directly — two fetchUserDetail
// calls for different ranges can land out of order, and reading the
// window off the live global filter would then mislabel a still-
// displayed stale response's buckets (P1 item 7). Falls back to
// filters.window only before any response has ever landed.
const reqLabels = computed(() =>
  (detail.value?.reqSeries?.buckets ?? []).map((b) => formatBucketLabel(b, detail.value?.reqSeries?.window ?? filters.window)),
)
const reqDatasets = computed(() => [
  { label: 'Requests', data: detail.value?.reqSeries?.series[0]?.points ?? [], color: '--chart-requests' },
])
const costLabels = computed(() =>
  (detail.value?.costSeries?.buckets ?? []).map((b) => formatBucketLabel(b, detail.value?.costSeries?.window ?? filters.window)),
)
const costDatasets = computed(() => [
  { label: 'Cost', data: (detail.value?.costSeries?.series[0]?.points ?? []).map((v) => v), color: '--chart-cost' },
])

/** showUserModelHint mirrors AttributionDrilldown.vue's own convention (Spend page): the hint shows when the client's own feature flag reads off, OR the usermodel call itself came back 404 (stores/consumers.ts's modelTotalsUnavailable) — the second case catches a stale/not-yet-loaded overview on a deep link or reload, where featuresUserModelStats can read true for a moment while the server still says otherwise. */
const showUserModelHint = computed(() => !featuresUserModelStats.value || detail.value?.modelTotalsUnavailable === true)

/** modelRows ranks usermodel totals by cost desc (falling back to req when cost is absent from the requested metrics) — a small, page-local sort since this table has no other consumer that would justify a shared column/sort helper. */
const modelRows = computed(() => {
  const rows = detail.value?.modelTotals?.rows ?? []
  return [...rows].sort((a, b) => (b.values.cost ?? b.values.req ?? 0) - (a.values.cost ?? a.values.req ?? 0))
})

/** recentErrors filters the shared Events store (WP-F, read-only import) down to this user — the newest 10, matching the store's own newest-first order. */
const recentErrors = computed(() => events.events.filter((e) => e.user === props.userId).slice(0, 10))

const ratios = computed(() => (entry.value ? budgetRatios(entry.value) : []))

function budgetValueText(id: string, used: number, limit: number): string {
  if (id.startsWith('cost')) return `${formatCost(used)} / ${formatCost(limit)}`
  return `${formatExactInt(used)} / ${formatExactInt(limit)}`
}

/**
 * runOutDate reuses lib/burndown.ts's own runOutDate — the SAME function
 * BurnDownChart.vue projects the Spend page's burn-down line with — rather
 * than a second, disagreeing implementation (P2 item 14: the two used to
 * differ on an already-exceeded budget and on day 1 of the month).
 */
const runOutDate = computed(() => {
  const limitUsd = entry.value?.limits?.costPerMonthUSD
  if (!entry.value || !limitUsd) return null
  return computeRunOutDate(entry.value.costPerMonthMicroUsd, Math.round(limitUsd * 1_000_000), now.value)
})
</script>

<template>
  <div class="flex flex-col gap-4">
    <div class="flex items-center gap-2">
      <Button type="button" variant="ghost" size="sm" @click="emit('close')">
        <FontAwesomeIcon :icon="faArrowLeft" class="size-3.5" aria-hidden="true" />
        Back to directory
      </Button>
    </div>

    <Card>
      <CardHeader>
        <div class="flex flex-wrap items-center gap-2">
          <CardTitle class="font-mono">{{ userId }}</CardTitle>
          <Badge v-if="consumerUser?.admin" variant="destructive">admin</Badge>
          <Badge v-if="consumerUser?.source" variant="secondary">{{ consumerUser.source }}</Badge>
        </div>
        <CardDescription v-if="entry?.groups?.length || entry?.groupName">
          Member of {{ (entry?.groups ?? [entry?.groupName]).filter(Boolean).join(', ') }}
        </CardDescription>
      </CardHeader>
      <CardContent v-if="!entry" class="text-sm text-muted-foreground">
        No usage scope for this user yet — it appears after their first request.
      </CardContent>
      <CardContent v-else class="grid grid-cols-2 gap-4 text-sm sm:grid-cols-4">
        <div>
          <p class="text-muted-foreground">Requests/day</p>
          <p class="text-lg font-semibold tabular-nums"><CompactNumber :value="entry.requestsPerDay" /></p>
        </div>
        <div>
          <p class="text-muted-foreground">Cost/day</p>
          <p class="text-lg font-semibold tabular-nums">{{ formatCost(entry.costPerDayMicroUsd) }}</p>
        </div>
        <div>
          <p class="text-muted-foreground">Cost/month</p>
          <p class="text-lg font-semibold tabular-nums">{{ formatCost(entry.costPerMonthMicroUsd) }}</p>
        </div>
        <div>
          <p class="text-muted-foreground">Limits</p>
          <p class="text-sm">{{ formatLimits(entry.limits) }}</p>
        </div>
      </CardContent>
    </Card>

    <Card v-if="entry && ratios.length">
      <CardHeader>
        <CardTitle>Headroom</CardTitle>
        <CardDescription v-if="runOutDate">At the current pace, cost/month runs out around {{ formatTimestamp(runOutDate.toISOString()) }}.</CardDescription>
      </CardHeader>
      <CardContent class="flex flex-wrap items-center gap-x-4 gap-y-1.5">
        <span v-for="ratio in ratios" :key="ratio.id" class="inline-flex items-center gap-1.5 text-xs text-muted-foreground">
          {{ BUDGET_RATIO_LABEL[ratio.id] }}
          <UsageBar
            :ratio="ratio.ratio"
            :label="BUDGET_RATIO_LABEL[ratio.id]"
            :value-text="budgetValueText(ratio.id, ratio.used, ratio.limit)"
          />
        </span>
      </CardContent>
    </Card>

    <Card>
      <CardHeader>
        <CardTitle>Requests over time</CardTitle>
      </CardHeader>
      <CardContent>
        <p v-if="detail?.loading" class="text-sm text-muted-foreground">loading…</p>
        <p v-else-if="detail?.error" class="text-sm text-destructive">{{ detail.error }}</p>
        <TimeSeriesChart v-else :labels="reqLabels" :datasets="reqDatasets" :ariaLabel="`Requests over time for ${userId}`" />
      </CardContent>
    </Card>

    <Card>
      <CardHeader>
        <CardTitle>Cost over time</CardTitle>
      </CardHeader>
      <CardContent>
        <p v-if="detail?.loading" class="text-sm text-muted-foreground">loading…</p>
        <p v-else-if="detail?.error" class="text-sm text-destructive">{{ detail.error }}</p>
        <TimeSeriesChart v-else :labels="costLabels" :datasets="costDatasets" :value-formatter="formatCost" :ariaLabel="`Cost over time for ${userId}`" />
      </CardContent>
    </Card>

    <Card>
      <CardHeader>
        <CardTitle>Models used</CardTitle>
      </CardHeader>
      <CardContent class="flex flex-col gap-3">
        <ClampedRangeNotice :window="filters.window" :span="filters.span" />
        <Alert v-if="showUserModelHint" variant="warn">
          <AlertDescription>Enable admin.stats.userModel in the plugin config to see a per-model breakdown for this user.</AlertDescription>
        </Alert>
        <Table v-else-if="modelRows.length">
          <TableHeader>
            <TableRow>
              <TableHead>Model</TableHead>
              <TableHead class="text-right">Requests</TableHead>
              <TableHead class="text-right">Cost</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            <TableRow v-for="row in modelRows" :key="row.id">
              <TableCell><ModelChip :id="row.id" /></TableCell>
              <TableCell class="text-right tabular-nums"><CompactNumber :value="row.values.req ?? 0" /></TableCell>
              <TableCell class="text-right tabular-nums">{{ formatCost(row.values.cost ?? 0) }}</TableCell>
            </TableRow>
          </TableBody>
        </Table>
        <p v-else class="py-6 text-center text-sm text-muted-foreground">no model usage in this range</p>
      </CardContent>
    </Card>

    <Card>
      <CardHeader>
        <CardTitle>Recent errors</CardTitle>
      </CardHeader>
      <CardContent>
        <ul v-if="recentErrors.length" class="flex flex-col gap-2 text-sm">
          <li v-for="(event, index) in recentErrors" :key="index" class="border-b pb-2 last:border-0 last:pb-0">
            <p class="flex flex-wrap items-center gap-2">
              <Badge variant="secondary">{{ event.kind }}</Badge>
              <span class="text-muted-foreground">{{ formatTimestamp(event.time) }}</span>
            </p>
            <p class="text-muted-foreground">{{ event.message }}</p>
          </li>
        </ul>
        <p v-else class="py-6 text-center text-sm text-muted-foreground">none in the current event window</p>
      </CardContent>
    </Card>

    <Card v-if="consumerUser?.providers?.length || consumerUser?.models?.length">
      <CardHeader>
        <CardTitle>Personal grant</CardTitle>
        <CardDescription>Access this user has beyond their group's own.</CardDescription>
      </CardHeader>
      <CardContent class="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div>
          <p class="mb-1.5 text-xs text-muted-foreground">Providers</p>
          <div class="flex flex-wrap gap-1.5">
            <Badge v-for="name in consumerUser?.providers ?? []" :key="name" as="span" variant="secondary" class="font-mono font-normal">
              {{ name }}
            </Badge>
          </div>
        </div>
        <div>
          <p class="mb-1.5 text-xs text-muted-foreground">Models</p>
          <div class="flex flex-wrap gap-1.5">
            <ModelChip v-for="id in consumerUser?.models ?? []" :key="id" :id="id" />
          </div>
        </div>
      </CardContent>
    </Card>
  </div>
</template>
