import type { ColumnDef } from '@tanstack/vue-table'
import { h } from 'vue'

import CompactNumber from '@/components/CompactNumber.vue'
import { Badge } from '@/components/ui/badge'
import { formatElapsedAgo, formatLatencyMs } from '@/lib/format'
import type { AdminTargetHealthView, AdminTargetView, AdminTotalsResponse } from '@/types/api'

/**
 * numericColumn builds one right-aligned, sortable request-counter
 * column, rendered via CompactNumber (SI-style, exact value on title/
 * aria-label — usage-columns.ts's own numericColumn applies the
 * identical treatment). Mirrors usage-columns.ts's own helper of the
 * same name and purpose, but without that one's storeDown '?' masking: a
 * target's counters carry no storeDown flag (limiter.targetUsage,
 * limits.go — a target scope has no limit to protect from a
 * misleadingly-confident zero, unlike a limited user/group scope), so
 * there is nothing to mask here. accessorFn (and therefore sorting)
 * always reads the true raw number.
 */
function numericColumn(id: string, header: string, read: (t: AdminTargetView) => number): ColumnDef<AdminTargetView, unknown> {
  return {
    id,
    header,
    accessorFn: read,
    meta: { align: 'right' },
    cell: ({ row }) => h(CompactNumber, { value: read(row.original) }),
  }
}

// --- target health (feat/target-health) ---
//
// Mirrors ProviderHealthPanel.vue's own healthBadgeVariant/healthBadgeLabel
// convention for the discovery circuit breaker: a Badge only for the two
// states worth flagging (unhealthy, unknown); the common healthy case
// renders no badge at all, just muted inline text — the same "nothing to
// show reads as no badge" idiom every health/rate indicator in this panel
// already follows (see AdminTargetHealthView's own doc comment, api.ts,
// for what each field means).

const UNKNOWN_HEALTH_TITLE = 'not observed yet — no traffic and probes disabled or not yet run'

/** unhealthyLabel keeps the Badge's own visible text short — "unhealthy (Ns ago)" when lastCheck is known, plain "unhealthy" otherwise (defensive: a version-skewed server response omitting lastCheck on an unhealthy row). */
function unhealthyLabel(health: AdminTargetHealthView): string {
  const ago = formatElapsedAgo(health.lastCheck)
  return ago ? `unhealthy (${ago})` : 'unhealthy'
}

/** unhealthyTitle carries the detail the short label leaves out — consecutiveFailures, source, and lastError (when present) — e.g. "3 consecutive failures via probe: connection refused". */
function unhealthyTitle(health: AdminTargetHealthView): string {
  const failWord = health.consecutiveFailures === 1 ? 'failure' : 'failures'
  const sourcePart = health.source ? ` via ${health.source}` : ''
  const base = `${health.consecutiveFailures} consecutive ${failWord}${sourcePart}`
  return health.lastError ? `${base}: ${health.lastError}` : base
}

/** healthyTitle is the muted "healthy" text's hover detail — "Ns ago via probe/traffic" — omitting whichever half (relative time, source) the response does not carry. */
function healthyTitle(health: AdminTargetHealthView): string {
  const ago = formatElapsedAgo(health.lastCheck)
  if (!ago) return health.source ? `via ${health.source}` : ''
  return health.source ? `${ago} via ${health.source}` : ago
}

/**
 * unknownTitle renders the "unknown" badge's hover detail. F4
 * (feat/target-health) reads "unknown" two different ways — see
 * AdminTargetHealthView's own doc comment (types/api.ts): never observed
 * at all, which gets the generic UNKNOWN_HEALTH_TITLE, or observed but
 * never yet succeeded (still below failureThreshold), which carries real
 * failure detail (consecutiveFailures/source/lastError) even though the
 * label stays "unknown" — this surfaces that detail instead of the
 * generic message, mirroring unhealthyTitle's own conditional shape.
 */
function unknownTitle(health: AdminTargetHealthView): string {
  if (health.consecutiveFailures <= 0 && !health.lastError) return UNKNOWN_HEALTH_TITLE
  const failWord = health.consecutiveFailures === 1 ? 'failure' : 'failures'
  const sourcePart = health.source ? ` via ${health.source}` : ''
  const base = `${health.consecutiveFailures} ${failWord} so far${sourcePart}`
  return health.lastError ? `${base}: ${health.lastError}` : base
}

/**
 * healthCell renders one row's health column: destructive Badge for
 * unhealthy, muted plain text (plus " · " and formatLatencyMs when
 * latencyMs is known) for the explicit healthy case, and an outline
 * "unknown" Badge as the FALLBACK for everything else — not the reverse
 * — so a state value this webui build does not recognize renders as
 * "unknown" rather than silently reading as healthy.
 */
function healthCell(health: AdminTargetHealthView) {
  if (health.state === 'unhealthy') {
    return h(
      Badge,
      { as: 'span', variant: 'destructive', class: 'font-normal', title: unhealthyTitle(health) },
      () => unhealthyLabel(health),
    )
  }
  if (health.state === 'healthy') {
    const latencyPart = health.latencyMs !== undefined ? ` · ${formatLatencyMs(health.latencyMs)}` : ''
    return h('span', { class: 'text-muted-foreground', title: healthyTitle(health) }, `healthy${latencyPart}`)
  }
  return h(Badge, { as: 'span', variant: 'outline', class: 'font-normal', title: unknownTitle(health) }, () => 'unknown')
}

/**
 * targetColumns builds the shared TanStack `ColumnDef` set both the MCP
 * servers and the Agents DataTable in TargetsView.vue use (admin.go:
 * adminTargetView — the two tables share one identical row shape). name is
 * plain text (not the ModelChip copy treatment — a target name is not a
 * routable model id) UNLESS `onSelect` is given, in which case it renders
 * as a real `<button>` (same structural convention as EntityLink.vue: a
 * click opens that target's own caller breakdown, TargetCallers.vue,
 * rather than navigating away — redesign-plan.md section 3.4's
 * "TargetCallers.vue expander"). url is muted and truncated with a title
 * attribute for the full value; health is the Badge/muted-text pair
 * healthCell above builds (feat/target-health); access renders as a
 * Badge per allowed group, or a muted "All groups" when unrestricted —
 * the exact idiom ConsumerDirectory.vue's own group Access block already
 * established (Providers/Models/MCP servers/Agents dt/dd pairs there).
 */
export function targetColumns(onSelect?: (target: AdminTargetView) => void): ColumnDef<AdminTargetView, unknown>[] {
  return [
    {
      id: 'name',
      header: 'Name',
      accessorFn: (t) => t.name,
      cell: ({ row }) => {
        if (!onSelect) return h('span', { class: 'font-medium' }, row.original.name)
        return h(
          'button',
          {
            type: 'button',
            class:
              'cursor-pointer rounded-sm font-medium hover:text-primary hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50',
            title: `View ${row.original.name}'s callers`,
            onClick: (event: MouseEvent) => {
              event.stopPropagation()
              onSelect(row.original)
            },
          },
          row.original.name,
        )
      },
    },
    {
      id: 'url',
      header: 'URL',
      accessorFn: (t) => t.url,
      cell: ({ row }) =>
        h(
          'span',
          { class: 'block max-w-xs truncate text-muted-foreground', title: row.original.url },
          row.original.url,
        ),
    },
    {
      id: 'health',
      header: 'Health',
      // No natural sort order across three semantic states (healthy/
      // unhealthy/unknown) — plain alphabetical would put "healthy" and
      // "unhealthy" beside each other for the wrong reason. Same call the
      // access column below makes for its own non-primitive shape.
      enableSorting: false,
      accessorFn: (t) => t.health.state,
      cell: ({ row }) => healthCell(row.original.health),
    },
    {
      id: 'access',
      header: 'Access',
      enableSorting: false,
      accessorFn: (t) => (t.access ?? []).join(', '),
      cell: ({ row }) => {
        const access = row.original.access
        // null/absent = every group; [] = no group (admin.go adminTargetView.Access).
        if (access == null) return h('span', { class: 'text-muted-foreground' }, 'All groups')
        if (access.length === 0) return h('span', { class: 'text-muted-foreground' }, 'No groups')
        return h(
          'div',
          { class: 'flex flex-wrap gap-1.5' },
          access.map((name) => h(Badge, { key: name, as: 'span', variant: 'secondary', class: 'font-mono font-normal' }, () => name)),
        )
      },
    },
    numericColumn('reqMin', 'req/min', (t) => t.counters.requestsPerMinute),
    numericColumn('reqDay', 'req/day', (t) => t.counters.requestsPerDay),
    numericColumn('reqMonth', 'req/month', (t) => t.counters.requestsPerMonth),
  ]
}

// --- target x caller (redesign-plan.md section 3.4's "TargetCallers.vue
// expander" + "top callers" card) ---
//
// GET /admin/api/usage/totals?kind=targetcaller (redesign-plan.md section
// 1.3.v) has no dedicated row type in types/api.ts — AdminTotalsResponse
// inlines its row shape rather than naming it, so `TargetCallerRow` is
// derived from that response's own `rows` element type here (single source
// of truth: a future field added to the inline shape needs no second edit
// in this file).
type TargetCallerRow = AdminTotalsResponse['rows'][number]

export type TargetKind = 'mcp' | 'agent'

/**
 * composeTargetId builds the "mcp/{name}" / "agent/{name}" composite id
 * GET /admin/api/usage/totals?kind=targetcaller&target= expects (admin.go)
 * — the SAME prefix a targetcaller row's own `id` starts with
 * (parseTargetCallerId below splits it back apart).
 */
export function composeTargetId(kind: TargetKind, name: string): string {
  return `${kind}/${name}`
}

/**
 * ParsedTargetCallerId is one targetcaller row's `id`
 * ("mcp/fetch/alice" — AdminTotalsResponse's own doc comment) split into
 * its three parts.
 */
export interface ParsedTargetCallerId {
  kind: string
  target: string
  caller: string
}

/**
 * parseTargetCallerId never throws: an id missing either slash (a
 * version-skewed server, or a caller name that happens to contain no
 * slash of its own — the only way this format is ambiguous) degrades
 * gracefully rather than dropping the row — `target`/`caller` fall back to
 * "" so a caller can still detect the shortfall (empty string, not
 * undefined) and choose to render the raw id verbatim.
 */
export function parseTargetCallerId(id: string): ParsedTargetCallerId {
  const first = id.indexOf('/')
  if (first === -1) return { kind: '', target: '', caller: id }
  const kind = id.slice(0, first)
  const rest = id.slice(first + 1)
  const second = rest.indexOf('/')
  if (second === -1) return { kind, target: rest, caller: '' }
  return { kind, target: rest.slice(0, second), caller: rest.slice(second + 1) }
}

/**
 * targetCallerColumns builds the shared TanStack `ColumnDef` set
 * TargetCallers.vue's DataTable uses for both its modes: a single
 * target's own caller breakdown (`showTarget: false` — the panel's own
 * heading already names the target) and the fleet-wide "top callers" card
 * (`showTarget: true` — each row's own target is not implied by anything
 * else on screen). `requests` is the only metric the server returns for
 * this kind (contract: "targetcaller req only"), so there is no metric
 * picker here, unlike model-table-columns.ts's multi-metric table.
 */
export function targetCallerColumns(showTarget: boolean): ColumnDef<TargetCallerRow, unknown>[] {
  const columns: ColumnDef<TargetCallerRow, unknown>[] = []
  if (showTarget) {
    columns.push({
      id: 'target',
      header: 'Target',
      accessorFn: (row) => {
        const p = parseTargetCallerId(row.id)
        return `${p.kind}/${p.target}`
      },
      cell: ({ row }) => {
        const p = parseTargetCallerId(row.original.id)
        return h('span', { class: 'font-mono text-xs text-muted-foreground' }, `${p.kind}/${p.target}`)
      },
    })
  }
  columns.push({
    id: 'caller',
    header: 'Caller',
    accessorFn: (row) => parseTargetCallerId(row.id).caller || row.id,
    cell: ({ row }) => {
      const caller = parseTargetCallerId(row.original.id).caller
      return h('span', { class: 'font-medium' }, caller || row.original.id)
    },
  })
  columns.push({
    id: 'requests',
    header: 'Requests',
    meta: { align: 'right' },
    accessorFn: (row) => row.values.req ?? 0,
    cell: ({ row }) => h(CompactNumber, { value: row.original.values.req ?? 0 }),
  })
  return columns
}
