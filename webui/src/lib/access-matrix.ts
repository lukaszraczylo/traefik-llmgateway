// AccessMatrix.vue (redesign-plan.md section 3.4): pure matrix-building
// logic — rows are groups, columns are providers (an allowed check plus
// "n/m models") and every configured MCP server/agent target. Kept out of
// the component the same way every other pure data-shaping helper in this
// panel is (lib/usage-bars.ts, lib/forecast.ts) — vitest's node-environment
// config exercises it directly, no component mount required.
import type { AdminConsumerGroup, AdminTargetView } from '@/types/api'

/**
 * AccessCell is one matrix cell's reading: whether the group can reach
 * this provider/target at all, plus an optional human-readable detail (a
 * provider cell's "n/m models"; a target cell has none). `zero` (P3 item
 * 18) marks a provider cell where the provider itself is allowed but the
 * group's own model grant resolves to 0 of the provider's models — e.g.
 * a `models` glob that happens to match nothing this provider currently
 * serves — distinct from an ordinary allowed cell (0 usable models is
 * not the same reading as "unrestricted access", even though both would
 * otherwise render an identical green check).
 */
export interface AccessCell {
  allowed: boolean
  detail?: string
  zero?: boolean
}

/**
 * providerAccessCell reads a group's resolved provider access (admin.go:
 * adminConsumerGroup.AllowedProviders — never nil, already glob-matched
 * server-side, so this is a plain membership check, not a re-implementation
 * of path.Match) plus its per-provider model count (modelAccess, "n/m
 * models") when the provider is allowed at all. A provider absent from
 * modelAccess (the group is allowed the provider but the server reported no
 * model breakdown for it — should not happen in practice, but the type
 * itself is a plain Record with no guaranteed key) renders `allowed: true`
 * with no detail, rather than a fabricated "0/0 models". `zero` is set only
 * when the provider genuinely serves models (`total > 0`) but the group's
 * grant reaches none of them (`allowed === 0`) — a `total` of 0 (a
 * provider with no discovered models at all) is a different, unrelated
 * situation and does not trip it.
 */
export function providerAccessCell(group: AdminConsumerGroup, providerName: string): AccessCell {
  if (!group.allowedProviders.includes(providerName)) return { allowed: false }
  const access = group.modelAccess[providerName]
  if (!access) return { allowed: true }
  // `zero` follows this module's own "undefined means unset" convention
  // (every other optional AccessCell/BudgetRatio-style field in this
  // panel) — omitted (never a literal `false`) so the ordinary allowed
  // case's shape stays exactly `{ allowed, detail }`.
  const zero = access.allowed === 0 && access.total > 0 ? true : undefined
  return { allowed: true, detail: `${access.allowed}/${access.total} models`, zero }
}

/**
 * targetAccessCell reads a group's access to one MCP server/agent target
 * from that target's OWN `access` field (admin.go: adminTargetView.Access —
 * types/api.ts's own doc comment): `null`/`undefined` means every
 * configured group can reach it, `[]` means none can, a non-empty list
 * names the groups that can. No detail (unlike a provider cell) — a
 * target has no "n/m" breakdown, only reachable or not.
 */
export function targetAccessCell(group: AdminConsumerGroup, target: Pick<AdminTargetView, 'name' | 'access'>): AccessCell {
  if (target.access === undefined || target.access === null) return { allowed: true }
  return { allowed: target.access.includes(group.name) }
}

/**
 * MatrixKind is AccessMatrix.vue's own view switch: which one column
 * group the table currently renders (P4 three-view split — the old
 * matrix mixed providers, MCP servers, and agents in one wide table,
 * which was hard to scan). `'models'` is the default.
 */
export type MatrixKind = 'models' | 'mcp' | 'agents'

/** MATRIX_KINDS is the ordered list AccessMatrix.vue's Tabs render from — this array's order IS the tab order. */
export const MATRIX_KINDS: MatrixKind[] = ['models', 'mcp', 'agents']

/** MATRIX_KIND_LABEL is each kind's tab/column-group label. */
export const MATRIX_KIND_LABEL: Record<MatrixKind, string> = {
  models: 'Models',
  mcp: 'MCP servers',
  agents: 'Agents',
}

/**
 * parseMatrixKind reads the `matrix` hash page param (ConsumersPage.vue,
 * mirroring its own `view` param's parsing) into a MatrixKind — unknown
 * or absent values (a stale/hand-edited link, or the clean default hash
 * with no `matrix` param at all) fall back to `'models'` rather than
 * throwing.
 */
export function parseMatrixKind(raw: string | undefined): MatrixKind {
  return (MATRIX_KINDS as readonly string[]).includes(raw ?? '') ? (raw as MatrixKind) : 'models'
}

/** AccessMatrixRow is one group's full row: its own provider/MCP-server/agent cells, each keyed by that column's own name — AccessMatrix.vue iterates `providerNames`/`mcpServers`/`agents` (the caller's own column order) and looks up each cell by name rather than relying on object key iteration order. */
export interface AccessMatrixRow {
  group: string
  providers: Record<string, AccessCell>
  mcpServers: Record<string, AccessCell>
  agents: Record<string, AccessCell>
}

/**
 * buildAccessMatrix builds one row per group, over the given provider
 * names and MCP-server/agent target lists — the caller (AccessMatrix.vue)
 * supplies the column universe (every configured provider name from GET
 * /admin/api/overview; every target from GET /admin/api/targets) so this
 * function stays a pure join, with no fetching or ordering opinion of
 * its own.
 */
export function buildAccessMatrix(
  groups: AdminConsumerGroup[],
  providerNames: string[],
  mcpServers: AdminTargetView[],
  agents: AdminTargetView[],
): AccessMatrixRow[] {
  return groups.map((group) => ({
    group: group.name,
    providers: Object.fromEntries(providerNames.map((name) => [name, providerAccessCell(group, name)])),
    mcpServers: Object.fromEntries(mcpServers.map((target) => [target.name, targetAccessCell(group, target)])),
    agents: Object.fromEntries(agents.map((target) => [target.name, targetAccessCell(group, target)])),
  }))
}

/**
 * MatrixColumnsState is what AccessMatrix.vue's CardContent renders in
 * place of a table for the CURRENTLY SELECTED kind's own columns —
 * distinct from the separate consumers.error/consumers.loading/
 * rows.length===0 branches ahead of it in the template, which are about
 * GROUPS (rows, stores/consumers.ts), a different store entirely.
 */
export type MatrixColumnsState = 'loading' | 'error' | 'empty' | 'ready'

/**
 * matrixColumnsState decides which of the four states above the current
 * kind's own column source is in. `sourceLoaded` must be whether the
 * RAW dashboard field is non-null (`dashboard.overview !== null` for
 * 'models', `dashboard.targets !== null` for 'mcp'/'agents') — NOT
 * whether the derived column list (which maps a null source to `[]`) is
 * empty, since those two are indistinguishable by column count alone.
 * Previously AccessMatrix.vue read `columns.length === 0` on its own to
 * decide whether to show "No providers configured." etc — true both
 * while the source had not loaded yet (or had just failed) AND once it
 * had genuinely loaded with zero columns, so a page still on its first
 * fetch, or whose fetch had just failed, showed "no MCP servers
 * configured" as a confirmed fact about the fleet rather than "still
 * loading" or the real error. `dashboardError` (dashboard.ts's own
 * `error` field) is read only in the 'error' state.
 */
export function matrixColumnsState(sourceLoaded: boolean, columnCount: number, dashboardError: string): MatrixColumnsState {
  if (!sourceLoaded) return dashboardError ? 'error' : 'loading'
  return columnCount === 0 ? 'empty' : 'ready'
}
