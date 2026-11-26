import {
  faBolt,
  faChartLine,
  faCubes,
  faGaugeHigh,
  faHeartPulse,
  faPlug,
  faRoute,
  faSliders,
  faUsers,
} from '@fortawesome/free-solid-svg-icons'
import type { IconDefinition } from '@fortawesome/free-solid-svg-icons'

import type { PageId } from '@/lib/pages'
import type { AdminOverviewResponse, AdminTargetsResponse, AdminUsageResponse } from '@/types/api'

export type NavCountKey = 'users' | 'models' | 'routers' | 'targets'

export interface NavItem {
  id: PageId
  label: string
  icon: IconDefinition
  countKey?: NavCountKey
}

export interface NavGroup {
  label: string | null
  items: NavItem[]
}

/** Sidebar structure; every PageId appears exactly once. */
export const NAV_GROUPS: NavGroup[] = [
  {
    label: null,
    items: [
      { id: 'overview', label: 'Overview', icon: faGaugeHigh },
      { id: 'usage', label: 'Usage & spend', icon: faChartLine },
      { id: 'reliability', label: 'Reliability', icon: faHeartPulse },
    ],
  },
  {
    label: 'Directory',
    items: [
      { id: 'users', label: 'Users & groups', icon: faUsers, countKey: 'users' },
      { id: 'models', label: 'Models & providers', icon: faCubes, countKey: 'models' },
      { id: 'routers', label: 'Smart routers', icon: faRoute, countKey: 'routers' },
      { id: 'targets', label: 'MCP & agents', icon: faPlug, countKey: 'targets' },
    ],
  },
  {
    label: null,
    items: [
      { id: 'events', label: 'Events', icon: faBolt },
      { id: 'config', label: 'Config', icon: faSliders },
    ],
  },
]

type OverviewCounts = { providers?: Array<{ models: unknown[] }>; routers?: unknown[] } | null
type UsageCounts = { users: unknown[] } | null
type TargetCounts = { mcpServers: unknown[]; agents: unknown[] } | null

/** Sidebar badge counts; a key is omitted until its source has loaded. */
export function navCounts(
  overview: OverviewCounts | AdminOverviewResponse,
  usage: UsageCounts | AdminUsageResponse,
  targets: TargetCounts | AdminTargetsResponse,
): Partial<Record<NavCountKey, number>> {
  const counts: Partial<Record<NavCountKey, number>> = {}
  if (usage) counts.users = usage.users.length
  if (overview?.providers) counts.models = overview.providers.reduce((n, p) => n + p.models.length, 0)
  if (overview?.routers) counts.routers = overview.routers.length
  if (targets) counts.targets = targets.mcpServers.length + targets.agents.length
  return counts
}
