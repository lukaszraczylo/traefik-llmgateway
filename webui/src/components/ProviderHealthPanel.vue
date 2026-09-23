<script setup lang="ts">
import type { ColumnDef } from '@tanstack/vue-table'
import {
  faCircleCheck,
  faCircleXmark,
  faTriangleExclamation,
} from '@fortawesome/free-solid-svg-icons'
import { computed, h, reactive, watch } from 'vue'

import DataTable from '@/components/DataTable.vue'
import EntityLink from '@/components/EntityLink.vue'
import ErrorState from '@/components/ErrorState.vue'
import ModelChip from '@/components/ModelChip.vue'
import ProviderRateBadge from '@/components/ProviderRateBadge.vue'
import SearchInput from '@/components/SearchInput.vue'
import SkeletonList from '@/components/SkeletonList.vue'
import { Accordion, AccordionContent, AccordionItem, AccordionTrigger } from '@/components/ui/accordion'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { useSearchQuery } from '@/composables/useSearchQuery'
import { formatAgo, formatContextWindow, formatLatencyMs, formatUntil, refreshDetailLabel, refreshLabel, routableModelId } from '@/lib/format'
import { loadState } from '@/lib/load-state'
import { isModelDegraded } from '@/lib/provider-rate'
import { type ExpandState, clearExpandOverrides, computeExpandedItems, toggleItemExpand } from '@/lib/search-expand'
import { useDashboardStore } from '@/stores/dashboard'
import { useModelsStore } from '@/stores/models'
import { useNavStore } from '@/stores/nav'
import type {
  AdminAliasView,
  AdminLatencyView,
  AdminModelMetaView,
  AdminModelRateView,
  AdminPerfRow,
  AdminProvenanceView,
  AdminProviderView,
} from '@/types/api'

/**
 * ProviderHealthPanel (redesign-plan.md section 3.4, Models & providers
 * page) is ProvidersView.vue's own Providers accordion + aliases table,
 * moved here verbatim, PLUS fleet-wide performance (p50/p95, timeouts,
 * failovers — GET /admin/api/performance?kind=provider, read from
 * useModelsStore, which the Models page's own onMounted fetches once for
 * the whole page, shared with ModelCatalogTable.vue). The per-replica
 * discovery/latency/provenance badges below are UNCHANGED from
 * ProvidersView.vue — still sourced from the SAME 5s dashboard poll
 * (GET /admin/api/overview), still per-replica/in-process (see each
 * badge's own doc comment for why that never averages across replicas).
 */
const dashboard = useDashboardStore()
const models = useModelsStore()
const overview = computed(() => dashboard.overview)
const nav = useNavStore()

/**
 * goToProviderModels navigates to the Models catalog table, pre-filtered
 * (via its own `q` search) to this provider's own models — the redesign's
 * replacement for the pre-redesign nav.goToModels (stores/nav.ts no
 * longer has a per-page goTo* method at all, see that store's own doc
 * comment; every navigation goes through the single goTo(page, params)).
 */
function goToProviderModels(providerName: string): void {
  nav.goTo('models', { q: `${providerName}/` })
}

// --- provider expand/collapse (shadcn-vue Accordion) — unchanged from ProvidersView.vue ---
const expandState: ExpandState = reactive({
  manuallyExpanded: new Set<string>(),
  manuallyCollapsed: new Set<string>(),
})

// --- model/alias search filter (operator feature) — unchanged from ProvidersView.vue ---
const { query: modelQuery, normalized: normalizedQuery, hasQuery } = useSearchQuery()

function modelMatches(providerName: string, modelId: string): boolean {
  return routableModelId(providerName, modelId).toLowerCase().includes(normalizedQuery.value)
}
function providerMatches(p: AdminProviderView): boolean {
  return p.models.some((m) => modelMatches(p.name, m))
}
function aliasMatches(a: AdminAliasView): boolean {
  return a.alias.toLowerCase().includes(normalizedQuery.value) || a.target.toLowerCase().includes(normalizedQuery.value)
}

/**
 * providersLoadState (live-preview fix, same class of bug as
 * lib/access-matrix.ts's matrixColumnsState / TargetsView.vue's
 * targetsLoadState) — `overview` maps a not-yet-loaded dashboard.overview
 * straight to `[]` via `?? []` below, which the template used to render as
 * a confirmed "none" (no providers configured at all) before the 5s
 * dashboard poll's first response had even landed. `loading: true` mirrors
 * those same two call sites' own idiom: it is only ever consulted once
 * `hasData` (dashboard.overview !== null) is already false, so there is
 * nothing else to fall back to but a skeleton or the dashboard's own error.
 */
const providersLoadState = computed(() => loadState({ loading: true, hasData: overview.value !== null, error: dashboard.error }))

const filteredProviders = computed<AdminProviderView[]>(() => {
  const all = overview.value?.providers ?? []
  return hasQuery.value ? all.filter(providerMatches) : all
})
const filteredAliases = computed<AdminAliasView[]>(() => {
  const all = overview.value?.aliases ?? []
  return hasQuery.value ? all.filter(aliasMatches) : all
})

const expandedProviders = computed<Set<string>>(() =>
  computeExpandedItems(
    expandState,
    hasQuery.value,
    filteredProviders.value.map((p) => p.name),
  ),
)

function toggleProvider(name: string): void {
  toggleItemExpand(expandState, name, expandedProviders.value.has(name))
}

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

watch(modelQuery, (value) => {
  if (value.trim() === '') clearExpandOverrides(expandState)
})

function visibleModels(p: AdminProviderView): string[] {
  return hasQuery.value ? p.models.filter((m) => modelMatches(p.name, m)) : p.models
}

const ZERO_MODEL_RATE: AdminModelRateView = { attemptsDay: 0, failuresDay: 0 }

function modelRateFor(p: AdminProviderView, model: string): AdminModelRateView {
  return p.modelRates?.[model] ?? ZERO_MODEL_RATE
}

function modelIsDegraded(p: AdminProviderView, model: string): boolean {
  const r = modelRateFor(p, model)
  return isModelDegraded(r.attemptsDay, r.failuresDay)
}

const ZERO_MODEL_META: AdminModelMetaView = {}

function modelMetaFor(p: AdminProviderView, model: string): AdminModelMetaView {
  return p.modelMeta?.[model] ?? ZERO_MODEL_META
}

// --- discovery circuit breaker (feat/provider-health) — unchanged from ProvidersView.vue ---

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

function healthBadgeLabel(p: AdminProviderView): string {
  const until = formatUntil(p.openUntil)
  return until ? `${p.healthState} (${until})` : p.healthState
}

function healthDetailClass(state: AdminProviderView['healthState']): string {
  return state === 'open' ? 'text-destructive' : 'text-muted-foreground'
}

// --- per-replica upstream latency (feat: instrument upstream latency) — unchanged from ProvidersView.vue ---

function streamingLatency(p: AdminProviderView): AdminLatencyView | undefined {
  return p.latency?.streaming
}
function nonStreamingLatency(p: AdminProviderView): AdminLatencyView | undefined {
  return p.latency?.['non-streaming']
}

function latencyObservationCount(v: AdminLatencyView): string {
  if (v.count === undefined) return 'no observations counted'
  return `${v.count} ${v.count === 1 ? 'observation' : 'observations'} on this replica`
}

function perReplicaCaveat(): string {
  const replica = overview.value?.replica
  const who = replica ? `replica ${replica}` : 'this replica'
  return `Per-replica (${who}), in-process only — not a fleet-wide average across this deployment’s replicas.`
}

function streamingLatencyTitle(v: AdminLatencyView): string {
  return `Average time to first byte, the load-sensitive reading for streaming traffic. ${latencyObservationCount(v)}. ${perReplicaCaveat()}`
}

function nonStreamingLatencyTitle(v: AdminLatencyView): string {
  return `Average total response time, including generation — not a load-sensitive signal like streaming ttfb, since this provider buffers the whole completion before sending anything. ${latencyObservationCount(v)}. ${perReplicaCaveat()}`
}

// --- usage provenance (feat: expose token-accounting provenance) — unchanged from ProvidersView.vue ---

function estimatedProvenance(p: AdminProviderView): AdminProvenanceView | undefined {
  return p.provenance?.estimated
}
function unbilledProvenance(p: AdminProviderView): AdminProvenanceView | undefined {
  return p.provenance?.unbilled
}

function estimatedProvenanceTitle(v: AdminProvenanceView): string {
  const plural = v.requests === 1 ? '' : 's'
  return `${v.requests} non-streaming response${plural} carried no usage from the provider — prompt tokens were estimated from request body size instead (${v.tokens} tokens billed from the estimate), completion billed as zero. ${perReplicaCaveat()}`
}

function unbilledProvenanceTitle(v: AdminProvenanceView): string {
  const plural = v.requests === 1 ? '' : 's'
  return `${v.requests} streaming response${plural} carried no usage from the provider — the request was counted but zero tokens were billed. If this keeps happening, this provider may be serving completions entirely free against every budget. ${perReplicaCaveat()}`
}

// --- fleet-wide performance (NEW: GET /admin/api/performance?kind=provider) ---
//
// Fleet-wide (Redis-backed via the latency histogram counters, WP-A), UNLIKE
// every badge above this point in the file — a provider's own p50/p95
// here is the SAME reading regardless which replica answered the poll,
// the opposite of the per-replica caveat every latency/provenance badge
// above carries. timeouts/failovers are always populated (they do not
// need admin.stats.latency — only the percentile fields do, AdminPerfRow's
// own doc comment); a provider with zero of either renders no badge at
// all, mirroring every other "nothing to show" convention in this file.

function providerPerfFor(name: string): AdminPerfRow | undefined {
  return models.providerPerf.find((r) => r.id === name)
}

function fleetLatencyTitle(kind: 'p50' | 'p95'): string {
  const { window, span } = models.perfWindow
  return `Fleet-wide (every replica combined) ${kind === 'p50' ? 'median' : '95th percentile'} response time, over the last ${span} ${window}${span === 1 ? '' : 's'}.`
}

function fleetCountTitle(kind: 'timeouts' | 'failovers'): string {
  const { window, span } = models.perfWindow
  const noun = kind === 'timeouts' ? 'requests that exceeded the deadline' : "requests failed away from this provider to another candidate"
  return `${noun}, fleet-wide, over the last ${span} ${window}${span === 1 ? '' : 's'}.`
}

// --- model aliases (sortable DataTable) — unchanged from ProvidersView.vue ---
const aliasColumns: ColumnDef<AdminAliasView, unknown>[] = [
  {
    id: 'alias',
    header: 'Alias',
    accessorFn: (a) => a.alias,
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
              aria-hidden="true"
            />
            Redis
          </CardTitle>
        </CardHeader>
        <CardContent class="flex flex-col gap-1 text-sm">
          <p class="font-medium">{{ overview?.redis.configured ? 'Configured' : 'Not configured' }}</p>
          <p v-if="overview?.redis.lastErr" class="flex items-start gap-1.5 text-destructive">
            <FontAwesomeIcon :icon="faTriangleExclamation" class="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
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
              aria-hidden="true"
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
              aria-hidden="true"
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
          Every configured upstream, its last discovery refresh, and fleet-wide performance. Search filters providers and
          aliases by model id.
        </CardDescription>
        <SearchInput v-model="modelQuery" placeholder="Search models or aliases..." class="mt-2 max-w-sm" />
      </CardHeader>
      <CardContent class="flex flex-col gap-3">
        <Alert v-if="!models.latencyEnabled" variant="warn">
          <AlertTitle>Latency statistics are off</AlertTitle>
          <AlertDescription class="flex flex-wrap items-center gap-2">
            <span>Enable <code>admin.stats.latency</code> in the middleware config to see fleet p50/p95.</span>
            <Button type="button" variant="outline" size="sm" @click="nav.goTo('config')">Open Config</Button>
          </AlertDescription>
        </Alert>
        <SkeletonList v-if="providersLoadState === 'skeleton'" :rows="3" />
        <ErrorState v-else-if="providersLoadState === 'error'" :message="dashboard.error" :on-retry="dashboard.refresh" />
        <p v-else-if="!overview?.providers.length" class="py-6 text-center text-sm text-muted-foreground">none</p>
        <p
          v-else-if="hasQuery && filteredProviders.length === 0"
          class="py-6 text-center text-sm text-muted-foreground"
        >
          no providers match &quot;{{ modelQuery }}&quot;
        </p>
        <template v-else>
          <p class="text-xs text-muted-foreground">{{ perReplicaCaveat() }} Fleet p50/p95/timeouts/failovers below are fleet-wide, not per-replica.</p>
          <Accordion v-model="expandedProviderValues" type="multiple" class="rounded-md border px-3">
          <AccordionItem v-for="p in filteredProviders" :key="p.name" :value="p.name">
            <div class="flex items-center gap-2">
              <button
                type="button"
                class="shrink-0 cursor-pointer rounded-sm py-2.5 font-medium hover:text-primary hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
                :aria-label="`View ${p.name}'s models in the Models table`"
                :title="`View ${p.name}'s models in the Models table`"
                @click="goToProviderModels(p.name)"
              >{{ p.name }}</button>
              <AccordionTrigger header-class="flex-1">
              <span class="sr-only">Provider {{ p.name }} details</span>
              <span class="flex flex-1 flex-wrap items-center gap-x-3 gap-y-1 pr-2 text-left">
                <Badge as="span" variant="secondary" class="font-normal">{{ p.type }}</Badge>
                <span class="text-xs text-muted-foreground tabular-nums">{{ p.modelCount }} models</span>
                <ProviderRateBadge
                  :attempts-day="p.attemptsDay"
                  :failures-day="p.failuresDay"
                  :attempts-minute="p.attemptsMinute"
                  :failures-minute="p.failuresMinute"
                />
                <Badge
                  v-if="p.healthState !== 'closed'"
                  as="span"
                  :variant="healthBadgeVariant(p.healthState)"
                  class="font-normal"
                  :title="p.healthState === 'open' ? 'discovery is backing off after repeated failures' : 'a discovery probe is deciding whether to recover'"
                >
                  {{ healthBadgeLabel(p) }}
                </Badge>
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
                <Badge
                  v-if="providerPerfFor(p.name)?.p50Ms !== undefined"
                  as="span"
                  variant="outline"
                  class="font-normal tabular-nums"
                  :title="fleetLatencyTitle('p50')"
                >
                  fleet p50 {{ formatLatencyMs(providerPerfFor(p.name)!.p50Ms as number) }}
                </Badge>
                <Badge
                  v-if="providerPerfFor(p.name)?.p95Ms !== undefined"
                  as="span"
                  variant="outline"
                  class="font-normal tabular-nums"
                  :title="fleetLatencyTitle('p95')"
                >
                  fleet p95 {{ formatLatencyMs(providerPerfFor(p.name)!.p95Ms as number) }}
                </Badge>
                <Badge
                  v-if="(providerPerfFor(p.name)?.timeouts ?? 0) > 0"
                  as="span"
                  variant="secondary"
                  class="font-normal tabular-nums"
                  :title="fleetCountTitle('timeouts')"
                >
                  {{ providerPerfFor(p.name)!.timeouts }} timeouts
                </Badge>
                <Badge
                  v-if="(providerPerfFor(p.name)?.failovers ?? 0) > 0"
                  as="span"
                  variant="secondary"
                  class="font-normal tabular-nums"
                  :title="fleetCountTitle('failovers')"
                >
                  {{ providerPerfFor(p.name)!.failovers }} failovers
                </Badge>
                <Badge
                  v-if="estimatedProvenance(p)"
                  as="span"
                  variant="secondary"
                  class="font-normal tabular-nums"
                  :title="estimatedProvenanceTitle(estimatedProvenance(p) as AdminProvenanceView)"
                >
                  estimated {{ estimatedProvenance(p)!.requests }}
                </Badge>
                <Badge
                  v-if="unbilledProvenance(p)"
                  as="span"
                  variant="destructive"
                  class="font-normal tabular-nums"
                  :title="unbilledProvenanceTitle(unbilledProvenance(p) as AdminProvenanceView)"
                >
                  unbilled {{ unbilledProvenance(p)!.requests }}
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
            </div>
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
                <span v-for="m in visibleModels(p)" :key="m" class="inline-flex min-w-0 max-w-full items-center gap-1">
                  <ModelChip
                    :id="routableModelId(p.name, m)"
                    :context-tokens="modelMetaFor(p, m).contextTokens"
                    :input-per-m-tok-usd="modelMetaFor(p, m).inputPerMTokUsd"
                    :output-per-m-tok-usd="modelMetaFor(p, m).outputPerMTokUsd"
                  />
                  <EntityLink :label="routableModelId(p.name, m)" kind="model" :id="routableModelId(p.name, m)" class="min-w-0" label-class="truncate" />
                  <span
                    v-if="modelMetaFor(p, m).contextTokens !== undefined"
                    title="context window"
                    class="text-xs text-muted-foreground tabular-nums"
                  >
                    {{ formatContextWindow(modelMetaFor(p, m).contextTokens as number) }}
                  </span>
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
        </template>
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
