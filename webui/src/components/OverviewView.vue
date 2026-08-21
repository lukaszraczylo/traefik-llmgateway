<script setup lang="ts">
import {
  faChevronRight,
  faCircleCheck,
  faCircleXmark,
  faMagnifyingGlass,
  faTriangleExclamation,
  faXmark,
} from '@fortawesome/free-solid-svg-icons'
import { computed, reactive, ref, watch } from 'vue'

import ModelChip from '@/components/ModelChip.vue'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { formatAgo, formatTimestamp, routableModelId } from '@/lib/format'
import { type ExpandState, clearExpandOverrides, computeExpandedProviders, toggleProviderExpand } from '@/lib/provider-expand'
import { useDashboardStore } from '@/stores/dashboard'
import type { AdminAliasView, AdminProviderView } from '@/types/api'

const dashboard = useDashboardStore()
const overview = computed(() => dashboard.overview)

// --- provider row expand/collapse ---
//
// Providers table rows expand to a model list (operator feature). This is
// NOT shadcn-vue's Accordion or Collapsible component: both wrap
// trigger+content in one <div>, which cannot legally sit between two
// <tr> elements inside a <table>/<tbody>. This app renders entirely via
// Vue's DOM APIs (createElement/appendChild), never by parsing an HTML
// string, so the HTML5 parser's "foster parenting" algorithm — which
// only runs while building a DOM tree FROM a token stream — never fires
// here at all; a <div> placed as a <tbody> child by direct DOM
// manipulation stays exactly where it was put. What actually breaks is
// layout: CSS table rendering (CSS2.1 §17.2.1's anonymous-table-object
// generation) does not know what to do with a block-level box sitting
// directly inside a table-row-group box, so the table's visual layout
// misbehaves around it. Either way, a <div> does not belong there.
// (reka-ui's Collapsible primitives DO support an `as` prop that could in
// principle render Root as a <tbody> and Trigger/Content as <tr> —
// installed and inspected via the CLI to check, then not used:
// CollapsibleContent's animation-measurement code calls
// getBoundingClientRect() and sets inline transition/animation styles on
// whatever element `as` names, an interaction with a <tr> this component
// was never designed around and was not worth taking on for an instant
// show/hide with no transition.) A second, plain <TableRow> toggled by
// v-if is the correct table-native shape for an expandable row; it
// reuses Accordion's own visual language (a rotating chevron, a
// keyboard-accessible trigger) without misusing a component built for
// block content.
//
// Expand state itself (computeExpandedProviders/toggleProviderExpand/
// clearExpandOverrides) is a plain, Vue-free module — lib/provider-expand.ts
// — specifically so the exact toggle-during-search sequence a review
// flagged is checkable by a throwaway assertion script outside Vue,
// without standing up a component-test framework this project does not
// otherwise have.
const expandState: ExpandState = reactive({
  manuallyExpanded: new Set<string>(),
  manuallyCollapsed: new Set<string>(),
})

// --- model/alias search filter (operator feature) ---
const modelQuery = ref('')
const normalizedQuery = computed(() => modelQuery.value.trim().toLowerCase())
const hasQuery = computed(() => normalizedQuery.value.length > 0)

/**
 * Matching is against each model's full ROUTABLE id
 * (routableModelId(p.name, m) — the same string ModelChip both displays
 * and copies), not the bare model id: copying a chip's text and pasting
 * it back into search must find it, and the routable form is strictly
 * more permissive (it contains the bare id as a substring, plus the
 * provider prefix), so this never hides a match the bare-id form would
 * have found.
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

/** expandedProviders is what the template reads to decide which rows show their model list — see lib/provider-expand.ts's own doc comments for the manual/auto-expand/override semantics. */
const expandedProviders = computed<Set<string>>(() =>
  computeExpandedProviders(
    expandState,
    hasQuery.value,
    filteredProviders.value.map((p) => p.name),
  ),
)

/** toggleProvider reads the CURRENT effective (visible) state for name before flipping it — see toggleProviderExpand's own doc comment for exactly which bug this avoids. */
function toggleProvider(name: string): void {
  toggleProviderExpand(expandState, name, expandedProviders.value.has(name))
}

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

/**
 * providerModelsId is the id shared by a provider's chevron button
 * (aria-controls, only set while expanded — see the template) and its
 * expanded content row (id). Provider names are not escaped before this
 * template-literal interpolation: buildAdapters (providers.go) rejects
 * any name that does not match configNamePattern
 * (`^[a-zA-Z0-9._-]+$`, providers.go) before this app ever sees it, so
 * no provider name this endpoint can return contains a character an
 * HTML id/attribute value needs escaped.
 */
function providerModelsId(name: string): string {
  return `provider-models-${name}`
}

function clearQuery(): void {
  modelQuery.value = ''
}
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
        <div class="relative mt-2 max-w-sm">
          <FontAwesomeIcon
            :icon="faMagnifyingGlass"
            class="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground"
            aria-hidden="true"
          />
          <Input
            v-model="modelQuery"
            type="text"
            placeholder="Search models or aliases..."
            aria-label="Search models or aliases"
            class="pr-8 pl-8"
          />
          <button
            v-if="hasQuery"
            type="button"
            aria-label="Clear search"
            class="absolute top-1/2 right-2 -translate-y-1/2 rounded text-muted-foreground hover:text-foreground"
            @click="clearQuery"
          >
            <FontAwesomeIcon :icon="faXmark" class="size-3.5" />
          </button>
        </div>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Base URL</TableHead>
              <TableHead class="text-right">Models</TableHead>
              <TableHead>Last refresh</TableHead>
              <TableHead>Last error</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            <TableEmpty v-if="!overview?.providers.length" :colspan="6" class="text-muted-foreground">
              none
            </TableEmpty>
            <TableEmpty
              v-else-if="hasQuery && filteredProviders.length === 0"
              :colspan="6"
              class="text-muted-foreground"
            >
              no providers match &quot;{{ modelQuery }}&quot;
            </TableEmpty>
            <template v-for="p in filteredProviders" :key="p.name">
              <TableRow class="cursor-pointer select-none hover:bg-accent/50" @click="toggleProvider(p.name)">
                <TableCell class="font-medium">
                  <!--
                    A real <button>, not role="button" on the <tr>: a <tr>
                    carries table-row AT semantics (a screen reader
                    announces it as part of the table's row/column
                    structure), and overriding that to "button" strips
                    those semantics from the whole row — exactly the kind
                    of ARIA-over-native mistake the "semantics first"
                    rule warns against. The button lives in the natural
                    host, the chevron+name span, and needs no click
                    handler of its own: its native click (mouse, or Enter/
                    Space while focused — free, standard <button>
                    behavior, no keydown handling to write) bubbles up to
                    the row's own @click above, so exactly one place
                    (the row) ever runs the actual toggle. aria-controls
                    is set ONLY while expanded (not unconditionally): the
                    content row is v-if, not v-show (Lazy rendering — see
                    the models div below), so an id it would point to
                    while collapsed does not exist in the DOM yet, and
                    ARIA requires aria-controls name an id that exists.
                  -->
                  <button
                    type="button"
                    class="flex items-center gap-2 rounded focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:outline-1 focus-visible:outline-ring"
                    :aria-expanded="expandedProviders.has(p.name)"
                    :aria-controls="expandedProviders.has(p.name) ? providerModelsId(p.name) : undefined"
                  >
                    <FontAwesomeIcon
                      :icon="faChevronRight"
                      class="size-3 shrink-0 text-muted-foreground transition-transform duration-150"
                      :class="expandedProviders.has(p.name) ? 'rotate-90' : ''"
                      aria-hidden="true"
                    />
                    {{ p.name }}
                  </button>
                </TableCell>
                <TableCell class="text-muted-foreground">{{ p.type }}</TableCell>
                <TableCell class="text-muted-foreground">{{ p.baseUrl }}</TableCell>
                <TableCell class="text-right tabular-nums">{{ p.modelCount }}</TableCell>
                <TableCell class="text-muted-foreground">{{ formatTimestamp(p.lastRefresh) }}</TableCell>
                <TableCell class="text-destructive">{{ p.lastErr ?? '' }}</TableCell>
              </TableRow>
              <TableRow v-if="expandedProviders.has(p.name)" :id="providerModelsId(p.name)">
                <TableCell colspan="6" class="whitespace-normal bg-muted/30">
                  <div v-if="visibleModels(p).length" class="flex flex-wrap gap-1.5 py-1">
                    <ModelChip v-for="m in visibleModels(p)" :key="m" :id="routableModelId(p.name, m)" />
                  </div>
                  <p v-else class="py-1 text-sm text-muted-foreground">no models known yet</p>
                </TableCell>
              </TableRow>
            </template>
          </TableBody>
        </Table>
      </CardContent>
    </Card>

    <Card>
      <CardHeader>
        <CardTitle>Model aliases</CardTitle>
        <CardDescription>Operator-defined alias &rarr; target model mappings.</CardDescription>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Alias</TableHead>
              <TableHead>Target</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            <TableEmpty v-if="!overview?.aliases.length" :colspan="2" class="text-muted-foreground">
              none configured
            </TableEmpty>
            <TableEmpty
              v-else-if="hasQuery && filteredAliases.length === 0"
              :colspan="2"
              class="text-muted-foreground"
            >
              no aliases match &quot;{{ modelQuery }}&quot;
            </TableEmpty>
            <TableRow v-for="a in filteredAliases" :key="a.alias">
              <TableCell class="font-medium">{{ a.alias }}</TableCell>
              <TableCell class="text-muted-foreground">{{ a.target }}</TableCell>
            </TableRow>
          </TableBody>
        </Table>
      </CardContent>
    </Card>

    <p v-if="overview?.version" class="text-xs text-muted-foreground">Version: {{ overview.version }}</p>
  </div>
</template>
