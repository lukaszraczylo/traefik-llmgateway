import {
  faCoins,
  faCubes,
  faHeartPulse,
  faHouse,
  faPlug,
  faSliders,
  faUsers,
} from '@fortawesome/free-solid-svg-icons'
import type { IconDefinition } from '@fortawesome/free-solid-svg-icons'

/**
 * PAGE_IDS is the sidebar redesign's page union (redesign-plan.md section
 * 3.1) — replaces composables/useTabHash.ts's old flat TAB_VALUES. The
 * single source of truth for every "which page" check: SidebarNav.vue's
 * link list, stores/nav.ts's `page` state, and lib/hash-state.ts's
 * outer "#<page>" segment all import PAGE_IDS/PageId rather than each
 * redeclaring their own copy.
 */
export const PAGE_IDS = ['home', 'spend', 'consumers', 'models', 'reliability', 'config', 'targets'] as const

export type PageId = (typeof PAGE_IDS)[number]

/** The landing page when the hash names nothing recognized, or is absent (first load). */
export const DEFAULT_PAGE: PageId = 'home'

/** One sidebar entry: the page it navigates to, its visible label, and its icon (redesign-plan.md section 3.1's fixed icon list). */
export interface PageNavEntry {
  id: PageId
  label: string
  icon: IconDefinition
}

/** PAGE_NAV drives SidebarNav.vue's link list, in display order. */
export const PAGE_NAV: readonly PageNavEntry[] = [
  { id: 'home', label: 'Home', icon: faHouse },
  { id: 'spend', label: 'Spend', icon: faCoins },
  { id: 'consumers', label: 'Consumers', icon: faUsers },
  { id: 'models', label: 'Models', icon: faCubes },
  { id: 'reliability', label: 'Reliability', icon: faHeartPulse },
  { id: 'config', label: 'Config', icon: faSliders },
  { id: 'targets', label: 'MCP & Agents', icon: faPlug },
]

/** isPageId is a runtime type guard against PAGE_IDS — the only place that string union is checked against untrusted input (a hash, a hand-typed URL). */
export function isPageId(value: string): value is PageId {
  return (PAGE_IDS as readonly string[]).includes(value)
}
