import type { AttentionLink } from '@/lib/attention'
import { EVENT_KINDS } from '@/lib/events-filter'
import { formatSel, PANEL_PAGE } from '@/lib/panel-selection'
import type { PanelKind } from '@/lib/panel-selection'
import type { AdminEventView } from '@/types/api'

function panelLink(kind: PanelKind, id: string): AttentionLink {
  return { page: PANEL_PAGE[kind], sel: formatSel(kind, id) }
}

/** Where an event row leads: the user, group or provider it names, else the events page; null for a kind this build does not know. */
export function eventLink(event: AdminEventView): AttentionLink | null {
  if (!(EVENT_KINDS as readonly string[]).includes(event.kind)) return null
  if (event.user) return panelLink('user', event.user)
  if (event.group) return panelLink('group', event.group)
  if (event.provider) return panelLink('provider', event.provider)
  return { page: 'events' }
}
