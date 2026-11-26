import type { AdminTargetsResponse } from '@/types/api'

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
