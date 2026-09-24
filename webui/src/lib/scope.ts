// reuse-audit.md F5: scope-string parsing (the `group:`/`user:`/`model:`/
// `provider:` prefix convention shared by stores/filters.ts's global scope
// filter, AttributionDrilldown's `drill` breadcrumb, and every per-series
// `scope` string an /admin/api/usage/* or /admin/api/events response
// carries) used to be reimplemented at every call site: a prefix strip
// three different ways, `drilldownLevel` duplicated verbatim, and a
// scope-to-budget lookup written out twice more. Kept pure and out of any
// component/store the same way lib/series.ts and lib/kpi.ts keep their own
// math out of theirs — vitest's node-environment config exercises every
// function here directly, no component mount required.
import { sumGroupBudgetMicros } from '@/lib/kpi'
import { microsToUsd } from '@/lib/usage-bars'
import type { AdminGroupView, AdminUsageEntryView, LimitsConfig } from '@/types/api'

/** ScopeKind is every prefix a "kind:id" scope string can carry across this panel — not every caller accepts every kind (a Spend burn-down scope never carries 'provider', a Reliability series scope never carries 'group'/'user'), but the prefix syntax itself is the same everywhere. */
export type ScopeKind = 'group' | 'user' | 'model' | 'provider'

export interface ParsedScope {
  kind: ScopeKind
  id: string
}

/**
 * parseScope splits a "kind:id" scope string into its parts. Returns null
 * for a string with no recognized "kind:" prefix — a caller's own root/
 * unscoped sentinel ('all', 'total', or AttributionDrilldown's drill ''),
 * which only that caller knows how to interpret, so this never guesses at
 * one.
 */
export function parseScope(scope: string): ParsedScope | null {
  for (const kind of ['group', 'user', 'model', 'provider'] as const) {
    const prefix = `${kind}:`
    if (scope.startsWith(prefix)) return { kind, id: scope.slice(prefix.length) }
  }
  return null
}

/** scopeId strips a scope string's own "kind:" prefix, or returns it unchanged when it carries none (the caller's own root sentinel, or an id that was never prefixed to begin with). */
export function scopeId(scope: string): string {
  return parseScope(scope)?.id ?? scope
}

/** makeScope builds a "kind:id" scope string — parseScope's own inverse. */
export function makeScope(kind: ScopeKind, id: string): string {
  return `${kind}:${id}`
}

/** DrilldownLevel is AttributionDrilldown's current GET /admin/api/usage/totals request shape, derived from its `drill` breadcrumb state. */
export type DrilldownLevel = { kind: 'group' } | { kind: 'user'; group: string } | { kind: 'usermodel'; user: string }

/**
 * drilldownLevel derives AttributionDrilldown's current level from its
 * `drill` breadcrumb state: '' is the top (every group listed), a
 * `group:{name}` drill shows that group's own users, anything else is
 * read as a `user:{name}` drill showing that user's own per-model
 * breakdown — the ONE definition (moved from stores/spend.ts, which used
 * to duplicate AttributionDrilldown.vue's own identical copy verbatim).
 */
export function drilldownLevel(drill: string): DrilldownLevel {
  if (drill === '') return { kind: 'group' }
  const parsed = parseScope(drill)
  if (parsed?.kind === 'group') return { kind: 'user', group: parsed.id }
  return { kind: 'usermodel', user: parsed?.kind === 'user' ? parsed.id : drill }
}

/**
 * scopeBudgetLimits resolves the configured cost/month LimitsConfig for a
 * `filters.scope`-shaped string (redesign-plan.md section 3.1:
 * `scope=all|group:x|user:x|provider:x`) from the already-polled dashboard
 * overview/usage data — no second network round trip. `'all'` (or any
 * unscoped/root string parseScope does not recognize a `group:`/`user:`
 * prefix on) resolves to the FLEET-WIDE sum of every configured group's
 * own budget (Q2/DECISIONS: lib/kpi.ts's sumGroupBudgetMicros — the ONE
 * fleet-budget definition, replacing SpendPage.vue's own inline recompute
 * of the identical sum), `undefined` when that total is 0 ("no budget
 * set", never a fabricated zero-dollar budget). `group:`/`user:` resolve
 * to that one group's/user's own configured limits (undefined if the
 * scope names a group/user this fleet does not actually have); a
 * `provider:` scope (or any other unrecognized prefix) has no single
 * group/user limit to report and also resolves to undefined.
 */
export function scopeBudgetLimits(scope: string, groups: AdminGroupView[], users: AdminUsageEntryView[]): LimitsConfig | undefined {
  const parsed = parseScope(scope)
  if (!parsed) {
    const totalMicros = sumGroupBudgetMicros(groups)
    return totalMicros > 0 ? { costPerMonthUSD: microsToUsd(totalMicros) } : undefined
  }
  if (parsed.kind === 'group') return groups.find((g) => g.name === parsed.id)?.limits
  if (parsed.kind === 'user') return users.find((u) => u.id === parsed.id)?.limits
  return undefined
}
