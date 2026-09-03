import type { ColumnDef } from '@tanstack/vue-table'
import { h } from 'vue'

import CompactNumber from '@/components/CompactNumber.vue'
import { Badge } from '@/components/ui/badge'
import { formatElapsedAgo, formatLatencyMs } from '@/lib/format'
import type { AdminTargetHealthView, AdminTargetView } from '@/types/api'

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
// Mirrors ProvidersView.vue's own healthBadgeVariant/healthBadgeLabel
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
 * routable model id); url is muted and truncated with a title attribute
 * for the full value; health is the Badge/muted-text pair healthCell above
 * builds (feat/target-health); access renders as a Badge per allowed
 * group, or a muted "All groups" when unrestricted — the exact idiom
 * UsageView.vue's own group Access block already established (Providers/
 * Models/MCP servers/Agents dt/dd pairs there).
 */
export function targetColumns(): ColumnDef<AdminTargetView, unknown>[] {
  return [
    {
      id: 'name',
      header: 'Name',
      accessorFn: (t) => t.name,
      cell: ({ row }) => h('span', { class: 'font-medium' }, row.original.name),
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
        if (!access?.length) return h('span', { class: 'text-muted-foreground' }, 'All groups')
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
