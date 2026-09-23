<script setup lang="ts">
import { computed } from 'vue'

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { costAvoidedMicros, freeRows, pickReferenceModel } from '@/lib/cost-avoided'
import { formatCost } from '@/lib/format'
import type { AdminCatalogModel, AdminUsageModelEntry } from '@/types/api'

/**
 * CostAvoidedCard (redesign-plan.md section 3.4) reports what the fleet's
 * free-tier traffic in range WOULD have cost against a real, priced
 * reference model — lib/cost-avoided.ts's own doc comment has the unit
 * derivation. The reference model defaults to the most-requested priced
 * model actually served in range (pickReferenceModel) but is always
 * reader-overridable via the picker below; `ref` is a CONTROLLED prop
 * (this card owns no picker state itself), mirroring DataTable.vue's own
 * `sorting`/`update:sorting` convention — SpendPage.vue keeps the current
 * choice in the `ref` page param (redesign-plan.md section 3.1) so a
 * reload or shared link restores the same comparison.
 *
 * Cache savings (redesign-plan.md section 3.4: "cache savings (sum csave)
 * shown alongside when enabled") render as a second figure beside the
 * cost-avoided one, only when `cacheStatsEnabled` — a genuinely different
 * saving (a repeat request served from cache, never re-billed at all) that
 * would otherwise be easy to conflate with "served by a free model".
 *
 * N2 fix (verify-redesign-final.md): the figure is not always summable.
 * `cacheSavingsAvailable` (lib/cache-savings.ts's own doc comment has the
 * full backend grounding) is false for an hour-window range — the
 * backend never writes a per-hour cache-savings bucket at all, so
 * `m.cacheSavedMicroUsd` is undefined on every row and summing it would
 * silently render a fabricated "$0.00" rather than "no data for this
 * range". The card renders "n/a" instead whenever it is false.
 */
const props = defineProps<{
  /** The model ranking (GET /admin/api/usage/models, detail=1) — every free row feeds costAvoidedMicros; every catalog-priced row is a candidate reference model. */
  models: AdminUsageModelEntry[]
  catalog: Map<string, AdminCatalogModel>
  /**
   * The reader's explicit reference-model override (the `ref` page param),
   * or '' for "use the default". Named `refModel`, not `ref` — `ref` is a
   * reserved Vue template attribute (component template refs); a prop
   * literally named `ref` can never be set from a parent's template via
   * `:ref="..."`, since Vue always intercepts that binding for its own
   * template-ref mechanism instead of passing it through as a prop.
   */
  refModel: string
  /** AdminFeaturesView.cacheStats — gates the cache-savings figure. */
  cacheStatsEnabled: boolean
  /**
   * cacheSavingsAvailable (N2 fix, lib/cache-savings.ts's own
   * cacheSavingsStatus) — false for an hour-window range, where the
   * backend has no per-hour cache-savings bucket at all. The card
   * renders "n/a" instead of a figure when this is false, never a
   * fabricated "$0.00". SpendPage.vue computes this from the current
   * global range (it owns the filters store; this card stays a pure,
   * props-driven component).
   */
  cacheSavingsAvailable: boolean
  /**
   * cacheSavingsNote (N2 fix) — a caveat to show ALONGSIDE an available
   * figure (currently only the month-window "35-day retention" note,
   * lib/cache-savings.ts), or the reason text when
   * `cacheSavingsAvailable` is false. Null when the figure is available
   * and exact (day-window ranges).
   */
  cacheSavingsNote?: string | null
}>()

const emit = defineEmits<{ 'update:refModel': [value: string] }>()

/** priceableOptions is the picker's own list: every served model whose catalog entry is genuinely priced (excludes free/unpriced — comparing free traffic against another $0 model is meaningless, the same filter pickReferenceModel itself applies), sorted by descending requests so the most relevant choices sit at the top. */
const priceableOptions = computed<{ id: string; requests: number }[]>(() => {
  const out: { id: string; requests: number }[] = []
  for (const m of props.models) {
    const cat = props.catalog.get(m.id)
    if (!cat || cat.priceSource === 'free' || cat.priceSource === 'unpriced') continue
    out.push({ id: m.id, requests: m.requests ?? 0 })
  }
  return out.sort((a, b) => b.requests - a.requests)
})

const defaultReference = computed<string | null>(() => pickReferenceModel(props.models, props.catalog))
const effectiveReference = computed<string | null>(() => props.refModel || defaultReference.value)
const referenceCatalog = computed<AdminCatalogModel | undefined>(() =>
  effectiveReference.value ? props.catalog.get(effectiveReference.value) : undefined,
)

const avoidedMicros = computed<number | null>(() => {
  const cat = referenceCatalog.value
  if (!cat || cat.inputPerMTokUsd === undefined || cat.outputPerMTokUsd === undefined) return null
  return costAvoidedMicros(freeRows(props.models), cat.inputPerMTokUsd, cat.outputPerMTokUsd)
})

const cacheSavedMicros = computed<number>(() => {
  let total = 0
  for (const m of props.models) total += m.cacheSavedMicroUsd ?? 0
  return total
})

function onReferenceUpdate(value: unknown): void {
  if (typeof value === 'string') emit('update:refModel', value)
}
</script>

<template>
  <Card>
    <CardHeader class="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
      <div>
        <CardTitle>Cost avoided</CardTitle>
        <CardDescription>What free-tier traffic in range would have cost at a real model's own rates.</CardDescription>
      </div>
      <div v-if="priceableOptions.length > 0" class="flex flex-col gap-1">
        <label id="cost-avoided-ref-label" class="text-xs font-medium text-muted-foreground">Reference model</label>
        <Select :model-value="effectiveReference ?? undefined" @update:model-value="onReferenceUpdate">
          <SelectTrigger aria-labelledby="cost-avoided-ref-label" class="w-56">
            <SelectValue placeholder="Pick a reference model" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem v-for="opt in priceableOptions" :key="opt.id" :value="opt.id">{{ opt.id }}</SelectItem>
          </SelectContent>
        </Select>
      </div>
    </CardHeader>
    <CardContent class="grid grid-cols-2 gap-4 text-sm">
      <div>
        <p class="text-muted-foreground">Cost avoided (free-tier traffic)</p>
        <p v-if="avoidedMicros === null" class="text-sm text-muted-foreground">
          {{ priceableOptions.length === 0 ? 'No priced model served in range to compare against.' : 'Pick a reference model above.' }}
        </p>
        <p v-else class="text-lg font-semibold tabular-nums text-chart-requests">{{ formatCost(avoidedMicros) }}</p>
      </div>
      <div v-if="cacheStatsEnabled">
        <p class="text-muted-foreground">Cache savings</p>
        <p v-if="!cacheSavingsAvailable" class="text-lg font-semibold tabular-nums text-muted-foreground">n/a</p>
        <p v-else class="text-lg font-semibold tabular-nums text-chart-requests">{{ formatCost(cacheSavedMicros) }}</p>
        <p v-if="cacheSavingsNote" class="text-xs text-status-warn">{{ cacheSavingsNote }}</p>
      </div>
    </CardContent>
  </Card>
</template>
