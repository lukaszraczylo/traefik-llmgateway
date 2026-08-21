<script setup lang="ts">
import {
  faChevronRight,
  faCircleCheck,
  faCircleXmark,
  faMagnifyingGlass,
  faTriangleExclamation,
  faXmark,
} from '@fortawesome/free-solid-svg-icons'
import { computed, reactive, ref } from 'vue'

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
import { useDashboardStore } from '@/stores/dashboard'
import type { AdminAliasView, AdminProviderView } from '@/types/api'

const dashboard = useDashboardStore()
const overview = computed(() => dashboard.overview)

// --- provider row expand/collapse ---
//
// Providers table rows expand to a model list (operator feature). This is
// NOT shadcn-vue's Accordion or Collapsible component: both wrap
// trigger+content in one <div>, which cannot legally sit between two
// <tr> elements inside a <table>/<tbody> — a browser's HTML table parser
// reparents (hoists) a <div> found there out of the table, breaking the
// layout. (reka-ui's Collapsible primitives DO support an `as` prop that
// could in principle render Root as a <tbody> and Trigger/Content as
// <tr> — installed and inspected via the CLI to check, then not used:
// CollapsibleContent's animation-measurement code calls
// getBoundingClientRect() and sets inline transition/animation styles on
// whatever element `as` names, an interaction with a <tr> this component
// was never designed around and was not worth taking on for an
// instant show/hide with no transition.) A second, plain <TableRow>
// toggled by v-if is the correct table-native shape for an expandable
// row; it reuses Accordion's own visual language (a rotating chevron,
// the trigger row as a button) so it reads as the same interaction
// pattern without misusing components built for block content.
const manuallyExpanded = reactive(new Set<string>())
function toggleProvider(name: string): void {
  if (manuallyExpanded.has(name)) manuallyExpanded.delete(name)
  else manuallyExpanded.add(name)
}

// --- model/alias search filter (operator feature) ---
const modelQuery = ref('')
const normalizedQuery = computed(() => modelQuery.value.trim().toLowerCase())
const hasQuery = computed(() => normalizedQuery.value.length > 0)

function modelMatches(modelId: string): boolean {
  return modelId.toLowerCase().includes(normalizedQuery.value)
}
function providerMatches(p: AdminProviderView): boolean {
  return p.models.some(modelMatches)
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

/**
 * expandedProviders is what the template reads to decide which rows show
 * their model list: the user's own manual toggles (manuallyExpanded),
 * plus — only while a search query is active — every provider
 * filteredProviders kept (they were kept because they have a matching
 * model, so auto-expanding them surfaces the match without an extra
 * click). Clearing the query drops this back to exactly
 * manuallyExpanded, which manualExpanded's own toggle is the only thing
 * that ever writes to — the "restore collapsed state when cleared"
 * requirement falls out for free, because the auto-expand set was never
 * a manual toggle to begin with.
 */
const expandedProviders = computed<Set<string>>(() => {
  if (!hasQuery.value) return manuallyExpanded
  const expanded = new Set(manuallyExpanded)
  for (const p of filteredProviders.value) expanded.add(p.name)
  return expanded
})

/** visibleModels is p's own model list, filtered to matches while a query is active — p only appears in filteredProviders at all because it has one, so this is never empty in that case; it exists so the accordion shows WHICH models matched instead of re-showing all 70+ with no distinction. */
function visibleModels(p: AdminProviderView): string[] {
  return hasQuery.value ? p.models.filter(modelMatches) : p.models
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
              <TableRow
                role="button"
                tabindex="0"
                class="cursor-pointer select-none hover:bg-accent/50"
                :aria-expanded="expandedProviders.has(p.name)"
                :aria-controls="`provider-models-${p.name}`"
                @click="toggleProvider(p.name)"
                @keydown.enter.prevent="toggleProvider(p.name)"
                @keydown.space.prevent="toggleProvider(p.name)"
              >
                <TableCell class="font-medium">
                  <span class="flex items-center gap-2">
                    <FontAwesomeIcon
                      :icon="faChevronRight"
                      class="size-3 shrink-0 text-muted-foreground transition-transform duration-150"
                      :class="expandedProviders.has(p.name) ? 'rotate-90' : ''"
                      aria-hidden="true"
                    />
                    {{ p.name }}
                  </span>
                </TableCell>
                <TableCell class="text-muted-foreground">{{ p.type }}</TableCell>
                <TableCell class="text-muted-foreground">{{ p.baseUrl }}</TableCell>
                <TableCell class="text-right tabular-nums">{{ p.modelCount }}</TableCell>
                <TableCell class="text-muted-foreground">{{ formatTimestamp(p.lastRefresh) }}</TableCell>
                <TableCell class="text-destructive">{{ p.lastErr ?? '' }}</TableCell>
              </TableRow>
              <TableRow v-if="expandedProviders.has(p.name)" :id="`provider-models-${p.name}`">
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
