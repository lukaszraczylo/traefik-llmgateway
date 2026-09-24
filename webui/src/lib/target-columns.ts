import type { ColumnDef } from '@tanstack/vue-table'
import { h } from 'vue'

import { Badge } from '@/components/ui/badge'
import { compactColumn } from '@/lib/columns'
import { formatElapsedAgo, formatLatencyMs } from '@/lib/format'
import type { AdminTargetHealthView, AdminTargetsResponse, AdminTargetView, AdminTotalsResponse } from '@/types/api'

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
    compactColumn<AdminTargetView>('reqMin', 'req/min', (t) => t.counters.requestsPerMinute),
    compactColumn<AdminTargetView>('reqDay', 'req/day', (t) => t.counters.requestsPerDay),
    compactColumn<AdminTargetView>('reqMonth', 'req/month', (t) => t.counters.requestsPerMonth),
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
 * TargetRefResolution is TargetsPage.vue's own read of the `target` hash
 * param, normalized for GET /admin/api/usage/totals?kind=targetcaller&
 * target= (admin.go's parseTargetRef: only "mcp/{name}" or "agent/{name}"
 * is valid — a bare name 400s):
 *   - 'ref': `ref` is a valid "mcp/{name}"/"agent/{name}" composite,
 *     safe to pass straight to TargetCallers.vue; `name` is the bare
 *     name alone, for a nicer fallback label than the full composite.
 *   - 'pending': `raw` was a bare name and dashboard.targets has not
 *     loaded yet (still null) — there is nothing to resolve against
 *     yet, so the caller shows neither a table nor a notice.
 *   - 'notice': `raw` could not be resolved to exactly one target —
 *     either no configured MCP server or agent has that name, or (rarer)
 *     BOTH an MCP server and an agent share it, so guessing either one
 *     would risk showing the wrong caller breakdown. `message` is ready
 *     to render as-is.
 */
export type TargetRefResolution =
  | { kind: 'ref'; ref: string; name: string }
  | { kind: 'pending' }
  | { kind: 'notice'; message: string }

/** parsePrefixedTargetRef reads a "mcp/{name}"/"agent/{name}" composite — mirrors stats_read.go's own parseTargetRef (Cut on the FIRST "/", so a name containing further slashes still round-trips) — or null for anything else (a bare name, an unknown prefix, or an empty name). */
function parsePrefixedTargetRef(raw: string): { kind: TargetKind; name: string } | null {
  const cut = raw.indexOf('/')
  if (cut === -1) return null
  const prefix = raw.slice(0, cut)
  const name = raw.slice(cut + 1)
  if (name === '' || (prefix !== 'mcp' && prefix !== 'agent')) return null
  return { kind: prefix, name }
}

/**
 * resolveTargetRef normalizes TargetsPage.vue's `target` hash param
 * (P2 item: a hand-typed `#targets?target=demo-mcp` 400ed — the backend
 * only accepts the prefixed composite form, never a bare name) into a
 * TargetRefResolution: an already-prefixed `raw` ("mcp/x"/"agent/x")
 * passes through unchanged (server-side existence is not checked here —
 * an unknown target under a valid prefix just returns zero rows,
 * TargetCallers.vue's own existing "no callers recorded" empty state,
 * not an error); a bare name is looked up in `targets.mcpServers` first,
 * then `targets.agents` — resolved only when it matches EXACTLY ONE of
 * the two lists, otherwise 'notice' ('unknown' for neither, 'ambiguous'
 * for both, since silently guessing which kind the reader meant could
 * show the wrong target's callers).
 */
export function resolveTargetRef(raw: string, targets: Pick<AdminTargetsResponse, 'mcpServers' | 'agents'> | null): TargetRefResolution {
  const prefixed = parsePrefixedTargetRef(raw)
  if (prefixed) return { kind: 'ref', ref: composeTargetId(prefixed.kind, prefixed.name), name: prefixed.name }

  if (targets === null) return { kind: 'pending' }

  const inMcp = targets.mcpServers.some((t) => t.name === raw)
  const inAgents = targets.agents.some((t) => t.name === raw)
  if (inMcp && inAgents) {
    return { kind: 'notice', message: `"${raw}" matches both an MCP server and an agent — use "mcp/${raw}" or "agent/${raw}" to pick one.` }
  }
  if (inMcp) return { kind: 'ref', ref: composeTargetId('mcp', raw), name: raw }
  if (inAgents) return { kind: 'ref', ref: composeTargetId('agent', raw), name: raw }
  return { kind: 'notice', message: `No MCP server or agent named "${raw}" is configured.` }
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
  columns.push(compactColumn<TargetCallerRow>('requests', 'Requests', (row) => row.values.req ?? 0))
  return columns
}
