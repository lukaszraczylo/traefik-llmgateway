import type { PageId } from '@/lib/pages'
import { formatSel, PANEL_PAGE } from '@/lib/panel-selection'
import type { PanelKind } from '@/lib/panel-selection'

export type AttentionSeverity = 'danger' | 'warning' | 'info'

export interface AttentionLink {
  page: PageId
  sel?: string
  /** Page params when no panel opens, e.g. a search. */
  params?: Record<string, string>
}

export interface AttentionItem {
  severity: AttentionSeverity
  text: string
  link: AttentionLink
}

export interface AttentionOverview {
  providers: Array<{ name: string; healthState: string; lastErr?: string }>
  pricing: { unpriced: number }
  redis: { configured: boolean; lastErr?: string }
}
export interface AttentionUsage {
  users: Array<{ id: string }>
}
export interface AttentionEvents {
  source: string
  events: Array<{ kind: string; user?: string }>
}
export interface AttentionRouter {
  name: string
  stats?: { escalations: Array<{ count: number }> }
}

const SEVERITY_ORDER: Record<AttentionSeverity, number> = { danger: 0, warning: 1, info: 2 }

function panelLink(kind: PanelKind, id: string): AttentionLink {
  return { page: PANEL_PAGE[kind], sel: formatSel(kind, id) }
}

function providerItems(overview: AttentionOverview): AttentionItem[] {
  const out: AttentionItem[] = []
  for (const p of overview.providers) {
    const open = p.healthState === 'open'
    if (!open && !p.lastErr) continue
    const text = open ? `${p.name} breaker open${p.lastErr ? `: ${p.lastErr}` : ''}` : `${p.name} failing: ${p.lastErr}`
    out.push({ severity: 'danger', text, link: panelLink('provider', p.name) })
  }
  return out
}

function throttledUserItems(usage: AttentionUsage | null, events: AttentionEvents): AttentionItem[] {
  const counts = new Map<string, number>()
  for (const e of events.events) {
    if (e.kind === 'rate_limit' && e.user) counts.set(e.user, (counts.get(e.user) ?? 0) + 1)
  }
  const known = new Set((usage?.users ?? []).map((u) => u.id))
  return [...counts.entries()]
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .map(([user, n]) => ({
      severity: 'warning' as const,
      text: `${user} rate limited ${n} time${n === 1 ? '' : 's'}`,
      link: known.has(user) ? panelLink('user', user) : { page: 'users' as const },
    }))
}

function routerItems(routers: AttentionRouter[]): AttentionItem[] {
  const out: AttentionItem[] = []
  for (const r of routers) {
    const n = (r.stats?.escalations ?? []).reduce((sum, e) => sum + e.count, 0)
    if (n > 0) out.push({ severity: 'info', text: `${r.name} escalated ${n} request${n === 1 ? '' : 's'} to a higher tier`, link: panelLink('router', r.name) })
  }
  return out
}

/** Lists what an operator should look at first, most severe first; empty when the fleet is healthy. */
export function deriveAttention(
  overview: AttentionOverview | null,
  usage: AttentionUsage | null,
  events: AttentionEvents | null,
  routers: AttentionRouter[],
): AttentionItem[] {
  const items: AttentionItem[] = []
  if (overview) {
    items.push(...providerItems(overview))
    if (overview.redis.configured && events && events.source !== 'redis') {
      const why = overview.redis.lastErr ? `: ${overview.redis.lastErr}` : ''
      items.push({ severity: 'danger', text: `Redis unreachable, events served from this replica only${why}`, link: { page: 'config' } })
    }
    if (overview.pricing.unpriced > 0) {
      const n = overview.pricing.unpriced
      items.push({ severity: 'info', text: `${n} unpriced model${n === 1 ? '' : 's'}, spend is understated`, link: { page: 'usage' } })
    }
  }
  if (events) items.push(...throttledUserItems(usage, events))
  items.push(...routerItems(routers))
  return items.sort((a, b) => SEVERITY_ORDER[a.severity] - SEVERITY_ORDER[b.severity])
}
