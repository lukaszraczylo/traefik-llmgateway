<script setup lang="ts">
import { faCheck, faTriangleExclamation, faXmark } from '@fortawesome/free-solid-svg-icons'
import { computed, onMounted } from 'vue'

import EmptyState from '@/components/EmptyState.vue'
import ErrorState from '@/components/ErrorState.vue'
import SkeletonTable from '@/components/SkeletonTable.vue'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { buildAccessMatrix, MATRIX_KIND_LABEL, MATRIX_KINDS, matrixColumnsState } from '@/lib/access-matrix'
import type { AccessCell, MatrixKind } from '@/lib/access-matrix'
import { loadState } from '@/lib/load-state'
import { useConsumersStore } from '@/stores/consumers'
import { useDashboardStore } from '@/stores/dashboard'
import { useNavStore } from '@/stores/nav'

/**
 * AccessMatrix (redesign-plan.md section 3.4; P4 three-view split) is the
 * Consumers page's `view=matrix` alternative to ConsumerDirectory: rows
 * are groups, columns are ONE of three kinds at a time — providers
 * (allowed check + "n/m models", from GET /admin/api/consumers'
 * AllowedProviders/modelAccess), MCP servers, or agents (both from GET
 * /admin/api/targets' own `access` list) — switched by a Tabs control in
 * the card header, CONTROLLED via the `kind` prop/`update:kind` emit the
 * same way AttributionDrilldown.vue's `drill` prop works: this component
 * owns no navigation state itself, ConsumersPage.vue keeps `kind` in
 * nav.params (lib/access-matrix.ts's own `matrix` hash param) so a
 * reload or shared link restores the same view. Pure data shaping lives
 * in lib/access-matrix.ts; this component only fetches, lays out the
 * selected kind's columns, and renders.
 */
const props = defineProps<{ kind: MatrixKind }>()
const emit = defineEmits<{ 'update:kind': [kind: MatrixKind] }>()

const consumers = useConsumersStore()
const dashboard = useDashboardStore()
const nav = useNavStore()

onMounted(() => {
  void consumers.ensureConsumers()
})

const providerNames = computed(() => (dashboard.overview?.providers ?? []).map((p) => p.name))
const mcpServers = computed(() => dashboard.targets?.mcpServers ?? [])
const agents = computed(() => dashboard.targets?.agents ?? [])

/** MatrixSection is the AccessMatrixRow key each kind reads its cells from. */
type MatrixSection = 'providers' | 'mcpServers' | 'agents'

/** KIND_SECTION maps the page-facing MatrixKind to the row-shape key buildAccessMatrix produced it under (lib/access-matrix.ts's own AccessMatrixRow). */
const KIND_SECTION: Record<MatrixKind, MatrixSection> = {
  models: 'providers',
  mcp: 'mcpServers',
  agents: 'agents',
}

/** KIND_DESCRIPTION is the CardDescription text for the currently selected kind. */
const KIND_DESCRIPTION: Record<MatrixKind, string> = {
  models: "Which groups can reach which providers, and how many of each provider's models.",
  mcp: 'Which groups can reach which MCP servers.',
  agents: 'Which groups can reach which agents.',
}

/** KIND_EMPTY_MESSAGE is shown instead of a table with zero columns — distinct from the "no groups configured" state below, since groups can exist while this kind's own column list is empty (e.g. no MCP servers configured at all). */
const KIND_EMPTY_MESSAGE: Record<MatrixKind, string> = {
  models: 'No providers configured.',
  mcp: 'No MCP servers configured.',
  agents: 'No agents configured.',
}

/** MatrixColumn is one header/cell column of the currently selected kind — no more "(MCP)"/"(agent)" label suffix, since the Tabs control already says which kind is showing. */
interface MatrixColumn {
  key: string
  label: string
  section: MatrixSection
}

const columns = computed<MatrixColumn[]>(() => {
  const section = KIND_SECTION[props.kind]
  if (props.kind === 'models') return providerNames.value.map((name): MatrixColumn => ({ key: name, label: name, section }))
  const targets = props.kind === 'mcp' ? mcpServers.value : agents.value
  return targets.map((t): MatrixColumn => ({ key: t.name, label: t.name, section }))
})

const rows = computed(() => buildAccessMatrix(consumers.data?.groups ?? [], providerNames.value, mcpServers.value, agents.value))

/** sourceLoaded is whether the CURRENT kind's own raw dashboard field has loaded at least once — dashboard.overview for 'models', dashboard.targets for 'mcp'/'agents' — read straight off the store rather than the derived `columns` list, which maps a not-yet-loaded source to `[]` indistinguishably from a genuinely empty one (lib/access-matrix.ts's matrixColumnsState doc comment). */
const sourceLoaded = computed(() => (props.kind === 'models' ? dashboard.overview !== null : dashboard.targets !== null))

/** columnsState is the current kind's own loading/error/empty/ready state (lib/access-matrix.ts's matrixColumnsState) — drives which CardContent branch renders below, independent of the groups-only groupsLoadState/noGroupsConfigured checks above it. */
const columnsState = computed(() => matrixColumnsState(sourceLoaded.value, columns.value.length, dashboard.error))

/**
 * groupsLoadState (lib/load-state.ts) replaces the old bare
 * `consumers.error`/`consumers.loading` checks — those used to show the
 * error text UNCONDITIONALLY whenever consumers.error was set, even with
 * perfectly good stale group rows already in `rows` from a PRIOR
 * successful fetch (fetchConsumers, stores/consumers.ts, never clears
 * `data` on a failed refetch) — states-plan.md item 1's own "never wipe
 * rendered data on a background failure" rule, the same class of bug
 * this states pass fixes everywhere else. 'ready' once consumers.data has
 * loaded at least once, even with zero groups — `noGroupsConfigured`
 * below is the separate "loaded, but genuinely nothing configured" case.
 */
const groupsLoadState = computed(() => loadState({ loading: consumers.loading, hasData: consumers.data !== null, error: consumers.error }))

/** noGroupsConfigured (states-plan.md item 2: "Consumers: 'No groups configured' + hint to the Config page change helper") is true only once groups have genuinely loaded with none configured — not while still loading or errored (groupsLoadState handles those first, see the template below). */
const noGroupsConfigured = computed(() => groupsLoadState.value === 'ready' && rows.value.length === 0)

const EMPTY_CELL: AccessCell = { allowed: false }

function cellFor(row: (typeof rows.value)[number], column: MatrixColumn): AccessCell {
  return row[column.section][column.key] ?? EMPTY_CELL
}

/** setKind forwards a Tabs selection as `update:kind`, guarding the `unknown` reka-ui hands back down to a real MatrixKind (mirrors ConsumersPage.vue's own setView / ChangeHelper.vue's setHelper). */
function setKind(next: unknown): void {
  if (typeof next !== 'string' || !(MATRIX_KINDS as readonly string[]).includes(next)) return
  emit('update:kind', next as MatrixKind)
}

/**
 * cellSrText is the ONLY accessible text a screen reader announces for
 * one cell — the visible icon is aria-hidden and the visible detail span
 * is aria-hidden too (P3 item 18: it used to be read TWICE, once as
 * plain visible text and again inside this string). A zero-model cell
 * (`zero`) gets its own wording rather than the plain "Allowed, 0/5
 * models" reading, which sounds identical to "Allowed, 5/5 models" to a
 * screen reader user skimming past the number.
 */
function cellSrText(cell: AccessCell): string {
  if (!cell.allowed) return 'Not allowed'
  if (cell.zero) return `Allowed, but ${cell.detail} reachable — the model grant matches none of this provider's models`
  return cell.detail ? `Allowed, ${cell.detail}` : 'Allowed'
}

/** cellIcon picks the FontAwesome icon for a cell's three visual states: not allowed (X), allowed-but-zero-usable-models (warning triangle — P3 item 18: this used to render an identical green check to a genuinely unrestricted cell), and ordinarily allowed (check). */
function cellIcon(cell: AccessCell) {
  if (!cell.allowed) return faXmark
  if (cell.zero) return faTriangleExclamation
  return faCheck
}

/** cellIconClass colors cellIcon's three states distinctly — not-allowed and zero-but-allowed must never share the "everything is fine" green. */
function cellIconClass(cell: AccessCell): string {
  if (!cell.allowed) return 'text-muted-foreground/40'
  if (cell.zero) return 'text-status-warn'
  return 'text-chart-requests'
}
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>Access matrix</CardTitle>
      <CardDescription>{{ KIND_DESCRIPTION[props.kind] }}</CardDescription>
      <Tabs :model-value="props.kind" @update:model-value="setKind">
        <TabsList>
          <TabsTrigger v-for="k in MATRIX_KINDS" :key="k" :value="k">{{ MATRIX_KIND_LABEL[k] }}</TabsTrigger>
        </TabsList>
      </Tabs>
    </CardHeader>
    <CardContent v-if="groupsLoadState === 'error'">
      <ErrorState :message="consumers.error" :on-retry="consumers.fetchConsumers" />
    </CardContent>
    <CardContent v-else-if="groupsLoadState === 'skeleton'">
      <SkeletonTable :rows="4" :cols="4" />
    </CardContent>
    <CardContent v-else-if="noGroupsConfigured">
      <EmptyState title="No groups configured." description="Grant a group access from the Config page's change helper.">
        <Button type="button" variant="outline" size="sm" @click="nav.goTo('config', { helper: 'grant' })">Open change helper</Button>
      </EmptyState>
    </CardContent>
    <CardContent v-else-if="columnsState === 'error'">
      <ErrorState :message="dashboard.error" />
    </CardContent>
    <CardContent v-else-if="columnsState === 'loading'">
      <SkeletonTable :rows="rows.length" :cols="4" />
    </CardContent>
    <CardContent v-else-if="columnsState === 'empty'">
      <EmptyState :title="KIND_EMPTY_MESSAGE[props.kind]" />
    </CardContent>
    <CardContent v-else class="overflow-x-auto">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Group</TableHead>
            <TableHead v-for="column in columns" :key="column.key">{{ column.label }}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          <TableRow v-for="row in rows" :key="row.group">
            <TableHead scope="row" class="font-medium">{{ row.group }}</TableHead>
            <TableCell v-for="column in columns" :key="column.key" class="text-center" :title="cellFor(row, column).detail">
              <FontAwesomeIcon :icon="cellIcon(cellFor(row, column))" :class="cellIconClass(cellFor(row, column))" class="size-3.5" aria-hidden="true" />
              <span v-if="cellFor(row, column).detail" aria-hidden="true" class="ml-1 text-xs text-muted-foreground">{{ cellFor(row, column).detail }}</span>
              <span class="sr-only">{{ cellSrText(cellFor(row, column)) }}</span>
            </TableCell>
          </TableRow>
        </TableBody>
      </Table>
    </CardContent>
  </Card>
</template>
