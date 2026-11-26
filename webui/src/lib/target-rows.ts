import { composeTargetId, resolveTargetRef } from '@/lib/target-columns'
import type { TargetKind } from '@/lib/target-columns'
import type { AdminTargetsResponse, AdminTargetView } from '@/types/api'

export type TargetHealth = 'healthy' | 'unhealthy' | 'unknown'

export interface TargetRow {
  id: string
  kind: 'MCP' | 'Agent'
  name: string
  health: TargetHealth
  /** Sort key for the health column: worst first when descending. */
  healthRank: number
  host: string
}

const HEALTH_RANK: Record<TargetHealth, number> = { unhealthy: 2, unknown: 1, healthy: 0 }
const KIND_LABEL: Record<TargetKind, TargetRow['kind']> = { mcp: 'MCP', agent: 'Agent' }
const NONE = '—'

/** Host (with port) of a target URL; the raw text when it does not parse, a dash when empty. */
export function hostOf(url: string): string {
  if (!url) return NONE
  try {
    return new URL(url).host || url
  } catch {
    return url
  }
}

function healthOf(t: AdminTargetView): TargetHealth {
  const state = t.health?.state
  return state === 'healthy' || state === 'unhealthy' ? state : 'unknown'
}

function toRow(kind: TargetKind, t: AdminTargetView): TargetRow {
  const health = healthOf(t)
  return { id: composeTargetId(kind, t.name), kind: KIND_LABEL[kind], name: t.name, health, healthRank: HEALTH_RANK[health], host: hostOf(t.url) }
}

/** MCP servers then agents; ids are composed so an MCP server and an agent may share a name. */
export function targetRows(targets: AdminTargetsResponse | null): TargetRow[] {
  if (!targets) return []
  return [...targets.mcpServers.map((t) => toRow('mcp', t)), ...targets.agents.map((t) => toRow('agent', t))]
}

/** The target a composed ("mcp/x") or unambiguous bare id names, else null. */
export function findTarget(targets: AdminTargetsResponse | null, id: string): AdminTargetView | null {
  const res = resolveTargetRef(id, targets)
  if (res.kind !== 'ref') return null
  const list = res.ref.startsWith('agent/') ? targets?.agents : targets?.mcpServers
  return list?.find((t) => t.name === res.name) ?? null
}
