import type { PanelKind } from '@/lib/panel-selection'

export interface PaletteEntity {
  kind: PanelKind
  id: string
  label: string
}

type OverviewSource = {
  providers?: Array<{ name: string; models: string[] }>
  groups?: Array<{ name: string }>
  routers?: Array<{ name: string }>
} | null

/** Entities the command palette can open a detail panel for; empty until the dashboard has data. */
export function paletteEntities(
  overview: OverviewSource,
  usage: { users: Array<{ id: string }> } | null,
  targets: { mcpServers: Array<{ name: string }>; agents: Array<{ name: string }> } | null,
): PaletteEntity[] {
  const out: PaletteEntity[] = []
  for (const u of usage?.users ?? []) out.push({ kind: 'user', id: u.id, label: u.id })
  for (const g of overview?.groups ?? []) out.push({ kind: 'group', id: g.name, label: g.name })
  for (const p of overview?.providers ?? []) {
    out.push({ kind: 'provider', id: p.name, label: p.name })
    for (const m of p.models) out.push({ kind: 'model', id: `${p.name}/${m}`, label: `${p.name}/${m}` })
  }
  for (const r of overview?.routers ?? []) out.push({ kind: 'router', id: r.name, label: r.name })
  for (const t of [...(targets?.mcpServers ?? []), ...(targets?.agents ?? [])]) out.push({ kind: 'target', id: t.name, label: t.name })
  return out
}
