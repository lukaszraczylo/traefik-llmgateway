<script setup lang="ts">
import { computed, ref, watch } from 'vue'

import ClampedRangeNotice from '@/components/ClampedRangeNotice.vue'
import DataTable from '@/components/DataTable.vue'
import ErrorState from '@/components/ErrorState.vue'
import SkeletonTable from '@/components/SkeletonTable.vue'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { adminFetch } from '@/lib/api'
import { loadState } from '@/lib/load-state'
import { dayOrMonthWindow, totalsUrl } from '@/lib/range'
import { targetCallerColumns } from '@/lib/target-columns'
import { useFiltersStore } from '@/stores/filters'
import type { AdminTotalsResponse } from '@/types/api'

/**
 * TargetCallers (redesign-plan.md section 3.4, MCP & Agents page) reads
 * GET /admin/api/usage/totals?kind=targetcaller — per-caller request
 * counts, ranked descending, dropped to zero rows server-side. Used in two
 * modes from TargetsPage.vue, sharing this one component (vue.md: "if
 * you've written it twice, you owe an abstraction") rather than a
 * near-duplicate for each:
 *
 * - Per-target ("TargetCallers.vue expander"): `target` is set to the
 *   composed "mcp/{name}"/"agent/{name}" id (lib/target-columns.ts's
 *   composeTargetId) of whichever row the reader clicked in TargetsView.vue
 *   — shows only that target's own callers, no Target column (the panel's
 *   own heading already names it).
 * - Fleet-wide ("top callers" card): `target` omitted — every configured
 *   target's callers, ranked together, WITH a Target column so a caller
 *   who reaches more than one target is not ambiguous.
 *
 * `kind=targetcaller` only supports day/month windows (redesign-plan.md
 * section 1.3.v) — lib/range.ts's dayOrMonthWindow clamps the global
 * filters store's current (possibly hour-window) range down to a window
 * this endpoint actually accepts, rather than firing an invalid request
 * whenever the reader has an hour-resolution range (24h/48h) selected.
 */
const props = withDefaults(
  defineProps<{
    /** Composed "mcp/{name}" / "agent/{name}" id — omitted means fleet-wide, every target. */
    target?: string
    title: string
    limit?: number
  }>(),
  { limit: 10 },
)

const filters = useFiltersStore()

const rows = ref<AdminTotalsResponse['rows']>([])
const loading = ref(false)
/** loadedOk is true only right after a SUCCESSFUL fetch for the CURRENT selection (even with zero rows — a genuine "no callers" answer is still data) — reset synchronously at the top of every load() call, not just on props.target changing, so a range change gets the identical "skeleton while this NEW selection's own fetch is in flight" treatment (states-plan.md item 1: a skeleton only for the current selection's own missing data, never stale data from the previous one). */
const loadedOk = ref(false)
const error = ref('')
/** reqId guards against an out-of-order response overwriting a newer one (latest-request-wins, stores/catalog.ts's own convention) — a quick target switch, or a range change mid-flight, no longer risks showing a STALE target's callers under the new heading. */
let reqId = 0

async function load(): Promise<void> {
  const requestId = ++reqId
  loading.value = true
  rows.value = []
  loadedOk.value = false
  error.value = ''
  try {
    const { window, span } = dayOrMonthWindow(filters.window, filters.span)
    const res = await adminFetch<AdminTotalsResponse>(
      totalsUrl({ kind: 'targetcaller', window, span, metrics: ['req'], limit: props.limit, target: props.target }),
    )
    if (requestId !== reqId) return
    rows.value = res.rows
    loadedOk.value = true
  } catch (err) {
    if (requestId !== reqId) return
    error.value = err instanceof Error ? err.message : String(err)
  } finally {
    if (requestId === reqId) loading.value = false
  }
}

watch(
  [() => props.target, () => filters.window, () => filters.span],
  () => void load(),
  { immediate: true },
)

const columns = computed(() => targetCallerColumns(!props.target))

/** callersLoadState (lib/load-state.ts) replaces the old "loading…" text this view used to render inline as a fake DataTable empty-row — it no longer distinguished a still-loading fetch from one that had already FAILED (an initial-fetch failure rendered the identical "loading…" text forever, the same class of bug EventsView.vue's own P9 fix addressed). */
const callersLoadState = computed(() => loadState({ loading: loading.value, hasData: loadedOk.value, error: error.value }))
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>{{ title }}</CardTitle>
      <CardDescription>Ranked by requests, most recent day/month window available for the current range.</CardDescription>
    </CardHeader>
    <CardContent class="flex flex-col gap-3">
      <ClampedRangeNotice :window="filters.window" :span="filters.span" />
      <SkeletonTable v-if="callersLoadState === 'skeleton'" :rows="5" :cols="columns.length" />
      <ErrorState v-else-if="callersLoadState === 'error'" :message="error" :on-retry="load" />
      <DataTable v-else :columns="columns" :data="rows" empty-message="no callers recorded in this window" />
    </CardContent>
  </Card>
</template>
