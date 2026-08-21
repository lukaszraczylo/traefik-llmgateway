<script setup lang="ts">
import type { ColumnDef } from '@tanstack/vue-table'
import {
  faCircleCheck,
  faCircleXmark,
  faTriangleExclamation,
} from '@fortawesome/free-solid-svg-icons'
import { computed, h, reactive, watch } from 'vue'

import DataTable from '@/components/DataTable.vue'
import ModelChip from '@/components/ModelChip.vue'
import SearchInput from '@/components/SearchInput.vue'
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from '@/components/ui/accordion'
import { Badge } from '@/components/ui/badge'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { useSearchQuery } from '@/composables/useSearchQuery'
import { formatAgo, formatTimestamp, routableModelId } from '@/lib/format'
import { type ExpandState, clearExpandOverrides, computeExpandedItems, toggleItemExpand } from '@/lib/search-expand'
import { useDashboardStore } from '@/stores/dashboard'
import type { AdminAliasView, AdminProviderView } from '@/types/api'

const dashboard = useDashboardStore()
const overview = computed(() => dashboard.overview)

// --- provider expand/collapse (shadcn-vue Accordion) ---
//
// Providers render as a real shadcn-vue Accordion (operator directive:
// use the library component, restructure the layout to fit it — the
// earlier hand-rolled table-row-toggle accordion is retired). The
// trigger is one provider's summary line; the content holds its detail
// (base URL, last refresh, last error) plus its model-id chips.
//
// Expand STATE itself is unchanged: lib/search-expand.ts's plain,
// Vue-free two-set module (manuallyExpanded/manuallyCollapsed, generalized
// from provider-expand.ts so UsageView.vue's Groups accordion shares the
// same tested logic) — the exact search-interaction bug fix a prior review
// flagged, covered by a real vitest spec (lib/search-expand.spec.ts) that
// exercises the module directly, without mounting Vue. Only the
// template-facing adapter changed: expandedProviderValues below is a
// writable computed translating that Set-based state into the string[]
// shape Accordion's `type="multiple"` v-model expects, and back —
// clicking a trigger fires the setter with the new array; diffing it
// against the previous effective set finds the one name that changed
// and replays it through the SAME tested toggleItemExpand used before, so
// Accordion never owns this state itself, only displays it.
const expandState: ExpandState = reactive({
  manuallyExpanded: new Set<string>(),
  manuallyCollapsed: new Set<string>(),
})

// --- model/alias search filter (operator feature) ---
const { query: modelQuery, normalized: normalizedQuery, hasQuery } = useSearchQuery()

/**
 * Matching is against each model's full ROUTABLE id
 * (routableModelId(p.name, m) — the same string ModelChip both displays
 * and copies), not the bare model id: copying a chip's text and pasting
 * it back into search must find it, and the routable form is strictly
 * more permissive (it contains the bare id as a substring), so this
 * never hides a match the bare-id form would have found.
 */
function modelMatches(providerName: string, modelId: string): boolean {
  return routableModelId(providerName, modelId).toLowerCase().includes(normalizedQuery.value)
}
function providerMatches(p: AdminProviderView): boolean {
  return p.models.some((m) => modelMatches(p.name, m))
}
function aliasMatches(a: AdminAliasView): boolean {
  return a.alias.toLowerCase().includes(normalizedQuery.value) || a.target.toLowerCase().includes(normalizedQuery.value)
}

const filteredProviders = computed<AdminProviderView[]>(() => {
  const all = overview.value?.providers ?? []
  return hasQuery.value ? all.filter(providerMatches) : all
})
const filteredAliases = computed<AdminAliasView[]>(() => {
  const all = overview.value?.aliases ?? []
  return hasQuery.value ? all.filter(aliasMatches) : all
})

/** expandedProviders is the EFFECTIVE (possibly auto-expanded-by-search) set — see lib/search-expand.ts's own doc comments for the manual/auto-expand/override semantics. */
const expandedProviders = computed<Set<string>>(() =>
  computeExpandedItems(
    expandState,
    hasQuery.value,
    filteredProviders.value.map((p) => p.name),
  ),
)

/** toggleProvider reads the CURRENT effective (visible) state for name before flipping it — see toggleItemExpand's own doc comment for exactly which bug this avoids. */
function toggleProvider(name: string): void {
  toggleItemExpand(expandState, name, expandedProviders.value.has(name))
}

/**
 * expandedProviderValues adapts expandedProviders (a Set) to Accordion's
 * `type="multiple"` v-model contract (a string[]): reads out as
 * Array.from(expandedProviders.value); writes replay each name whose
 * membership changed through toggleProvider — Accordion never mutates
 * expandState directly, it only tells this setter what changed.
 */
const expandedProviderValues = computed<string[]>({
  get: () => Array.from(expandedProviders.value),
  set: (newValues) => {
    const next = new Set(newValues)
    for (const name of expandedProviders.value) {
      if (!next.has(name)) toggleProvider(name)
    }
    for (const name of next) {
      if (!expandedProviders.value.has(name)) toggleProvider(name)
    }
  },
})

// A stale manuallyCollapsed suppression from one search must never
// silently carry into a later, unrelated one — see ExpandState's own doc
// comment. Watching modelQuery (not just the explicit clear button)
// covers backspacing to empty too.
watch(modelQuery, (value) => {
  if (value.trim() === '') clearExpandOverrides(expandState)
})

/** visibleModels is p's own model list, filtered to matches while a query is active — p only appears in filteredProviders at all because it has one, so this is never empty in that case; it exists so the accordion shows WHICH models matched instead of re-showing all 70+ with no distinction. */
function visibleModels(p: AdminProviderView): string[] {
  return hasQuery.value ? p.models.filter((m) => modelMatches(p.name, m)) : p.models
}

// --- model aliases (sortable DataTable, operator directive) ---
//
// The alias id gets the ModelChip copy treatment (an alias name IS the
// routable string a caller sends as `model`); the target column stays
// plain text.
const aliasColumns: ColumnDef<AdminAliasView, unknown>[] = [
  {
    id: 'alias',
    header: 'Alias',
    accessorFn: (a) => a.alias,
    cell: ({ row }) => h(ModelChip, { id: row.original.alias }),
  },
  {
    id: 'target',
    header: 'Target',
    accessorFn: (a) => a.target,
    cell: ({ row }) => h('span', { class: 'text-muted-foreground' }, row.original.target),
  },
]
const aliasEmptyMessage = computed(() =>
  !overview.value?.aliases.length ? 'none configured' : hasQuery.value ? `no aliases match "${modelQuery.value}"` : 'none',
)
</script>

<template>
  <div class="flex flex-col gap-6">
    <div class="grid gap-4 sm:grid-cols-3">
      <Card>
        <CardHeader>
          <CardTitle class="flex items-center gap-2 text-sm font-medium text-muted-foreground">
            <FontAwesomeIcon
              :icon="overview?.redis.configured ? faCircleCheck : faCircleXmark"
              :class="overview?.redis.configured ? 'text-chart-requests' : 'text-muted-foreground'"
              class="size-3.5"
            />
            Redis
          </CardTitle>
        </CardHeader>
        <CardContent class="flex flex-col gap-1 text-sm">
          <p class="font-medium">{{ overview?.redis.configured ? 'Configured' : 'Not configured' }}</p>
          <p v-if="overview?.redis.lastErr" class="flex items-start gap-1.5 text-destructive">
            <FontAwesomeIcon :icon="faTriangleExclamation" class="mt-0.5 size-3.5 shrink-0" />
            <span>{{ overview.redis.lastErr }}{{ formatAgo(overview.redis.lastErrAt) }}</span>
          </p>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle class="flex items-center gap-2 text-sm font-medium text-muted-foreground">
            <FontAwesomeIcon
              :icon="overview?.cache.enabled ? faCircleCheck : faCircleXmark"
              :class="overview?.cache.enabled ? 'text-chart-requests' : 'text-muted-foreground'"
              class="size-3.5"
            />
            Response cache
          </CardTitle>
        </CardHeader>
        <CardContent class="text-sm">
          <p class="font-medium">{{ overview?.cache.enabled ? 'Enabled' : 'Disabled' }}</p>
          <p v-if="overview?.cache.enabled" class="text-muted-foreground">ttl {{ overview.cache.ttl }}</p>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle class="flex items-center gap-2 text-sm font-medium text-muted-foreground">
            <FontAwesomeIcon
              :icon="overview?.retry.enabled ? faCircleCheck : faCircleXmark"
              :class="overview?.retry.enabled ? 'text-chart-requests' : 'text-muted-foreground'"
              class="size-3.5"
            />
            Retry
          </CardTitle>
        </CardHeader>
        <CardContent class="text-sm">
          <p class="font-medium">{{ overview?.retry.enabled ? 'Enabled' : 'Disabled' }}</p>
          <p v-if="overview?.retry.enabled" class="text-muted-foreground">
            attempts {{ overview.retry.attempts }}, backoff {{ overview.retry.backoff }}
          </p>
        </CardContent>
      </Card>
    </div>

    <Card>
      <CardHeader>
        <CardTitle>Providers</CardTitle>
        <CardDescription>
          Every configured upstream and its last discovery refresh. Search filters providers and aliases by
          model id.
        </CardDescription>
        <SearchInput v-model="modelQuery" placeholder="Search models or aliases..." class="mt-2 max-w-sm" />
      </CardHeader>
      <CardContent>
        <p v-if="!overview?.providers.length" class="py-6 text-center text-sm text-muted-foreground">none</p>
        <p
          v-else-if="hasQuery && filteredProviders.length === 0"
          class="py-6 text-center text-sm text-muted-foreground"
        >
          no providers match &quot;{{ modelQuery }}&quot;
        </p>
        <Accordion v-else v-model="expandedProviderValues" type="multiple" class="rounded-md border px-3">
          <AccordionItem v-for="p in filteredProviders" :key="p.name" :value="p.name">
            <AccordionTrigger>
              <span class="flex flex-1 flex-wrap items-center gap-x-3 gap-y-1 pr-2 text-left">
                <span class="font-medium">{{ p.name }}</span>
                <Badge as="span" variant="secondary" class="font-normal">{{ p.type }}</Badge>
                <span class="text-xs text-muted-foreground tabular-nums">{{ p.modelCount }} models</span>
                <FontAwesomeIcon
                  v-if="p.lastErr"
                  :icon="faTriangleExclamation"
                  class="size-3.5 shrink-0 text-destructive"
                  aria-hidden="true"
                />
                <span class="text-xs text-muted-foreground">refreshed {{ formatAgo(p.lastRefresh) || 'never' }}</span>
              </span>
            </AccordionTrigger>
            <AccordionContent>
              <dl class="mb-3 grid grid-cols-2 gap-x-6 gap-y-1.5 text-sm sm:grid-cols-3">
                <div>
                  <dt class="text-xs text-muted-foreground">Base URL</dt>
                  <dd class="break-all">{{ p.baseUrl }}</dd>
                </div>
                <div>
                  <dt class="text-xs text-muted-foreground">Last refresh</dt>
                  <dd>{{ formatTimestamp(p.lastRefresh) }}</dd>
                </div>
                <div v-if="p.lastErr">
                  <dt class="text-xs text-muted-foreground">Last error</dt>
                  <dd class="text-destructive">{{ p.lastErr }}</dd>
                </div>
              </dl>
              <div v-if="visibleModels(p).length" class="flex flex-wrap gap-1.5">
                <ModelChip v-for="m in visibleModels(p)" :key="m" :id="routableModelId(p.name, m)" />
              </div>
              <p v-else class="text-sm text-muted-foreground">no models known yet</p>
            </AccordionContent>
          </AccordionItem>
        </Accordion>
      </CardContent>
    </Card>

    <Card>
      <CardHeader>
        <CardTitle>Model aliases</CardTitle>
        <CardDescription>Operator-defined alias &rarr; target model mappings.</CardDescription>
      </CardHeader>
      <CardContent>
        <DataTable :columns="aliasColumns" :data="filteredAliases" :empty-message="aliasEmptyMessage" />
      </CardContent>
    </Card>

    <p v-if="overview?.version" class="text-xs text-muted-foreground">Version: {{ overview.version }}</p>
  </div>
</template>
