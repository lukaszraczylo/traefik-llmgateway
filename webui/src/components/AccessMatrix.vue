<script setup lang="ts">
import { faCheck, faTriangleExclamation, faXmark } from '@fortawesome/free-solid-svg-icons'
import { computed, onMounted } from 'vue'

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { buildAccessMatrix } from '@/lib/access-matrix'
import type { AccessCell } from '@/lib/access-matrix'
import { useConsumersStore } from '@/stores/consumers'
import { useDashboardStore } from '@/stores/dashboard'

/**
 * AccessMatrix (redesign-plan.md section 3.4) is the Consumers page's
 * `view=matrix` alternative to ConsumerDirectory: rows are groups, columns
 * are every configured provider (allowed check + "n/m models", from GET
 * /admin/api/consumers' AllowedProviders/modelAccess) and every configured
 * MCP server/agent target (from GET /admin/api/targets' own `access`
 * list). Pure data shaping lives in lib/access-matrix.ts; this component
 * only fetches, lays out columns, and renders.
 */
const consumers = useConsumersStore()
const dashboard = useDashboardStore()

onMounted(() => {
  void consumers.ensureConsumers()
})

const providerNames = computed(() => (dashboard.overview?.providers ?? []).map((p) => p.name))
const mcpServers = computed(() => dashboard.targets?.mcpServers ?? [])
const agents = computed(() => dashboard.targets?.agents ?? [])

/** MatrixColumn flattens the three heterogeneous column groups (providers, MCP servers, agents) into one ordered list — a single v-for renders every header AND every cell, rather than three near-identical template blocks. */
interface MatrixColumn {
  key: string
  label: string
  section: 'providers' | 'mcpServers' | 'agents'
}

const columns = computed<MatrixColumn[]>(() => [
  ...providerNames.value.map((name): MatrixColumn => ({ key: name, label: name, section: 'providers' })),
  ...mcpServers.value.map((t): MatrixColumn => ({ key: t.name, label: `${t.name} (MCP)`, section: 'mcpServers' })),
  ...agents.value.map((t): MatrixColumn => ({ key: t.name, label: `${t.name} (agent)`, section: 'agents' })),
])

const rows = computed(() => buildAccessMatrix(consumers.data?.groups ?? [], providerNames.value, mcpServers.value, agents.value))

const EMPTY_CELL: AccessCell = { allowed: false }

function cellFor(row: (typeof rows.value)[number], column: MatrixColumn): AccessCell {
  return row[column.section][column.key] ?? EMPTY_CELL
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
      <CardDescription>Which groups can reach which providers, MCP servers, and agents.</CardDescription>
    </CardHeader>
    <CardContent v-if="consumers.error" class="text-sm text-destructive">{{ consumers.error }}</CardContent>
    <CardContent v-else-if="rows.length === 0" class="py-6 text-center text-sm text-muted-foreground">
      {{ consumers.loading ? 'loading…' : 'no groups configured' }}
    </CardContent>
    <CardContent v-else class="overflow-x-auto">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Group</TableHead>
            <TableHead v-for="column in columns" :key="column.key + column.section">{{ column.label }}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          <TableRow v-for="row in rows" :key="row.group">
            <TableHead scope="row" class="font-medium">{{ row.group }}</TableHead>
            <TableCell v-for="column in columns" :key="column.key + column.section" class="text-center" :title="cellFor(row, column).detail">
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
