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
import ProviderRateBadge from '@/components/ProviderRateBadge.vue'
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
import { formatAgo, formatContextWindow, formatLatencyMs, formatUntil, refreshDetailLabel, refreshLabel, routableModelId } from '@/lib/format'
import { isModelDegraded } from '@/lib/provider-rate'
import { type ExpandState, clearExpandOverrides, computeExpandedItems, toggleItemExpand } from '@/lib/search-expand'
import { useDashboardStore } from '@/stores/dashboard'
import type { AdminAliasView, AdminLatencyView, AdminModelMetaView, AdminModelRateView, AdminProviderView } from '@/types/api'

// This component backs the "Providers" tab (App.vue) — a UI-label rename
// only. The data it renders still comes from GET /admin/api/overview
// (admin.go's adminOverviewResponse) and the store field below keeps that
// same "overview" name, since the backend route/type names are unchanged
// API surface.
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

/** ZERO_MODEL_RATE is the fallback ProviderRateBadge reads for a model p.modelRates has no entry for — should not happen (admin.go's buildAdminOverview populates one entry per Models id, unconditionally), but a defensive fallback keeps a stale/mismatched client build from throwing rather than just under-reporting. */
const ZERO_MODEL_RATE: AdminModelRateView = { attemptsDay: 0, failuresDay: 0 }

/**
 * modelRateFor looks up one model's counters within p.modelRates (Feature
 * A, v0.22), falling back to ZERO_MODEL_RATE. The optional-chain on
 * modelRates itself (folded review minor, v0.22 review round), not just
 * on the lookup, matches ZERO_MODEL_RATE's own doc comment: a stale
 * client talking to an older server build (before this field existed at
 * all) would otherwise throw on `.modelRates[model]` rather than fall
 * back — the type says modelRates is always present, but a real response
 * across a version skew is the one case that type cannot guarantee.
 */
function modelRateFor(p: AdminProviderView, model: string): AdminModelRateView {
  return p.modelRates?.[model] ?? ZERO_MODEL_RATE
}

/** modelIsDegraded gates the per-model rate badge (spec: "ONLY when that model is degraded") so a healthy or no-traffic model's ModelChip renders with no badge beside it at all. */
function modelIsDegraded(p: AdminProviderView, model: string): boolean {
  const r = modelRateFor(p, model)
  return isModelDegraded(r.attemptsDay, r.failuresDay)
}

/** ZERO_MODEL_META is modelMetaFor's fallback, mirroring ZERO_MODEL_RATE's own doc comment above — every sub-field undefined reads as "nothing known", not a thrown error, across a version-skewed client/server pair. */
const ZERO_MODEL_META: AdminModelMetaView = {}

/** modelMetaFor looks up one model's resolved metadata within p.modelMeta (feature v0.23), falling back to ZERO_MODEL_META. */
function modelMetaFor(p: AdminProviderView, model: string): AdminModelMetaView {
  return p.modelMeta?.[model] ?? ZERO_MODEL_META
}

// --- discovery circuit breaker (feat/provider-health) ---
//
// Badge shown ONLY for 'open'/'half-open' — mirroring modelIsDegraded's
// own "no badge when healthy" convention above: a 'closed' provider (the
// common case) renders no breaker badge at all, exactly like a
// non-degraded model gets no ProviderRateBadge.

/** healthBadgeVariant maps healthState to the shadcn-vue Badge variant: destructive (backing off) for 'open', secondary (in progress) for 'half-open', undefined (no badge) for 'closed'. */
function healthBadgeVariant(state: AdminProviderView['healthState']): 'destructive' | 'secondary' | undefined {
  switch (state) {
    case 'open':
      return 'destructive'
    case 'half-open':
      return 'secondary'
    default:
      return undefined
  }
}

/** healthBadgeLabel renders the breaker badge's text: "open (in Ns)" while backing off (formatUntil omits the parenthetical once openUntil has passed or is unset), or "half-open" while probing. */
function healthBadgeLabel(p: AdminProviderView): string {
  const until = formatUntil(p.openUntil)
  return until ? `${p.healthState} (${until})` : p.healthState
}

/** healthDetailClass matches the accordion-content detail row's text color to healthBadgeVariant's own trigger-row Badge color, rather than hardcoding text-destructive for every non-closed state: destructive (red) for 'open', muted (grey, the same neutral tone secondary conveys on the Badge) for 'half-open'. */
function healthDetailClass(state: AdminProviderView['healthState']): string {
  return state === 'open' ? 'text-destructive' : 'text-muted-foreground'
}

// --- upstream latency (feat: instrument upstream latency) ---
//
// Beside the discovery-health badge above: no badge at all for a stream
// state with no observations yet (p.latency undefined, or missing that
// key) — mirrors healthBadgeVariant/modelIsDegraded's own "nothing to
// show reads as no badge" convention, so absent data reads as absent,
// never as a fabricated zero.
//
// The two stream states are deliberately labeled and worded differently,
// never merged into one figure (task brief's own correctness rule):
// streaming shows avgTtfbMs, the load-sensitive reading, labeled "ttfb";
// non-streaming shows avgDurationMs labeled "avg" — NEVER "latency" or
// "ttfb" — because a non-streaming provider buffers its whole completion
// before sending anything, so its own avgTtfbMs is approximately equal
// to avgDurationMs and carries the identical output-length contamination
// a raw total-duration figure always has (a 4000-token answer legitimately
// takes longer than a 50-token one on an equally healthy provider).

/** streamingLatency/nonStreamingLatency pull p.latency's two known stream-state keys (admin.go's buildAdminLatencyViews emits exactly these two strings) — undefined when that stream state has no observations yet. */
function streamingLatency(p: AdminProviderView): AdminLatencyView | undefined {
  return p.latency?.streaming
}
function nonStreamingLatency(p: AdminProviderView): AdminLatencyView | undefined {
  return p.latency?.['non-streaming']
}

/** latencyObservationCount renders v.count (omitted, per admin.go, when 0) as a singular/plural detail phrase for a badge title, falling back to "no observations counted" for the defensive case of a present view with an absent count (version skew). */
function latencyObservationCount(v: AdminLatencyView): string {
  if (v.count === undefined) return 'no observations counted'
  return `${v.count} ${v.count === 1 ? 'observation' : 'observations'} on this replica`
}

/**
 * PER_REPLICA_CAVEAT is appended to every latency badge's title (task
 * brief's correctness rule 1): this figure comes from the same
 * in-process accumulator the rate-limit rejection counter reads, never
 * Redis, so it reflects only whichever of this deployment's several
 * Traefik replicas answered the current poll — never a fleet-wide
 * average, and a freshly restarted pod shows a short window.
 */
const PER_REPLICA_CAVEAT = 'Per-replica, in-process only — not a fleet-wide average across this deployment’s replicas.'

/** streamingLatencyTitle is the streaming ttfb badge's hover/title detail. */
function streamingLatencyTitle(v: AdminLatencyView): string {
  return `Average time to first byte, the load-sensitive reading for streaming traffic. ${latencyObservationCount(v)}. ${PER_REPLICA_CAVEAT}`
}

/** nonStreamingLatencyTitle is the non-streaming avg-duration badge's hover/title detail — explicit that this is NOT a latency/ttfb reading (correctness rule 2). */
function nonStreamingLatencyTitle(v: AdminLatencyView): string {
  return `Average total response time, including generation — not a load-sensitive signal like streaming ttfb, since this provider buffers the whole completion before sending anything. ${latencyObservationCount(v)}. ${PER_REPLICA_CAVEAT}`
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
    // Optional-chained (review fix, folded minor) the same way
    // modelMetaFor guards the provider path below: AdminAliasView.
    // modelMeta is typed as always-present, but a real response from a
    // server build older than this field (version skew) would otherwise
    // throw on `.modelMeta.contextTokens` here instead of just omitting
    // the hover detail.
    cell: ({ row }) =>
      h(ModelChip, {
        id: row.original.alias,
        contextTokens: row.original.modelMeta?.contextTokens,
        inputPerMTokUsd: row.original.modelMeta?.inputPerMTokUsd,
        outputPerMTokUsd: row.original.modelMeta?.outputPerMTokUsd,
      }),
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
                <ProviderRateBadge
                  :attempts-day="p.attemptsDay"
                  :failures-day="p.failuresDay"
                  :attempts-minute="p.attemptsMinute"
                  :failures-minute="p.failuresMinute"
                />
                <!--
                  Discovery circuit breaker (feat/provider-health): no
                  badge at all while 'closed' (the healthy, common case),
                  mirroring modelIsDegraded's own convention for the
                  per-model rate badge above.
                -->
                <Badge
                  v-if="p.healthState !== 'closed'"
                  as="span"
                  :variant="healthBadgeVariant(p.healthState)"
                  class="font-normal"
                  :title="p.healthState === 'open' ? 'discovery is backing off after repeated failures' : 'a discovery probe is deciding whether to recover'"
                >
                  {{ healthBadgeLabel(p) }}
                </Badge>
                <!--
                  Upstream latency (feat: instrument upstream latency):
                  streaming's ttfb is the load-sensitive figure; the
                  non-streaming badge deliberately says "avg", never
                  "ttfb"/"latency" — see nonStreamingLatencyTitle's own
                  doc comment for why that number is not a load signal.
                  No badge at all for a stream state with no observations
                  yet, mirroring the health badge's own convention above.
                -->
                <Badge
                  v-if="streamingLatency(p)?.avgTtfbMs !== undefined"
                  as="span"
                  variant="outline"
                  class="font-normal tabular-nums"
                  :title="streamingLatencyTitle(streamingLatency(p) as AdminLatencyView)"
                >
                  streaming ttfb {{ formatLatencyMs(streamingLatency(p)!.avgTtfbMs as number) }}
                </Badge>
                <Badge
                  v-if="nonStreamingLatency(p)?.avgDurationMs !== undefined"
                  as="span"
                  variant="outline"
                  class="font-normal tabular-nums"
                  :title="nonStreamingLatencyTitle(nonStreamingLatency(p) as AdminLatencyView)"
                >
                  non-streaming avg {{ formatLatencyMs(nonStreamingLatency(p)!.avgDurationMs as number) }}
                </Badge>
                <FontAwesomeIcon
                  v-if="p.lastErr"
                  :icon="faTriangleExclamation"
                  class="size-3.5 shrink-0 text-destructive"
                  aria-hidden="true"
                />
                <span class="text-xs text-muted-foreground">{{ refreshLabel(p.discoveryEnabled, p.lastRefresh) }}</span>
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
                  <dd>{{ refreshDetailLabel(p.discoveryEnabled, p.lastRefresh) }}</dd>
                </div>
                <div v-if="p.healthState !== 'closed'">
                  <dt class="text-xs text-muted-foreground">Discovery health</dt>
                  <dd :class="healthDetailClass(p.healthState)">{{ healthBadgeLabel(p) }}</dd>
                </div>
                <div v-if="p.lastErr">
                  <dt class="text-xs text-muted-foreground">Last error</dt>
                  <dd class="text-destructive">{{ p.lastErr }}</dd>
                </div>
              </dl>
              <div v-if="visibleModels(p).length" class="flex flex-wrap gap-1.5">
                <span v-for="m in visibleModels(p)" :key="m" class="inline-flex items-center gap-1">
                  <ModelChip
                    :id="routableModelId(p.name, m)"
                    :context-tokens="modelMetaFor(p, m).contextTokens"
                    :input-per-m-tok-usd="modelMetaFor(p, m).inputPerMTokUsd"
                    :output-per-m-tok-usd="modelMetaFor(p, m).outputPerMTokUsd"
                  />
                  <!--
                    Context chip, visible, muted (feature v0.23) — only
                    when known. Cost stays hover-only on ModelChip itself
                    (operator directive: chips + hover would otherwise
                    show the identical price figures twice; context
                    visible for scanning, price on hover for the exact
                    numbers).
                  -->
                  <span
                    v-if="modelMetaFor(p, m).contextTokens !== undefined"
                    title="context window"
                    class="text-xs text-muted-foreground tabular-nums"
                  >
                    {{ formatContextWindow(modelMetaFor(p, m).contextTokens as number) }}
                  </span>
                  <!--
                    attempts-minute/failures-minute deliberately omitted
                    (SHOULD-2, v0.22 review round): AdminModelRateView no
                    longer carries them at all, so leaving them unbound
                    (undefined) is what tells ProviderRateBadge to render
                    its day-window-only detail (dayRateTitle) instead of
                    claiming a live minute-window reading no data backs.
                  -->
                  <ProviderRateBadge
                    v-if="modelIsDegraded(p, m)"
                    :attempts-day="modelRateFor(p, m).attemptsDay"
                    :failures-day="modelRateFor(p, m).failuresDay"
                  />
                </span>
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
