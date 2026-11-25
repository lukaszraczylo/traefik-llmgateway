import type { AttentionLink } from '@/lib/attention'
import { PANEL_PAGE, parseSel } from '@/lib/panel-selection'
import type { PanelKind } from '@/lib/panel-selection'
import { useNavStore } from '@/stores/nav'

/** Navigation helpers for rows that link to a directory page or one of its detail panels. */
export function useEntityLinks() {
  const nav = useNavStore()

  function openEntity(kind: PanelKind, id: string): void {
    nav.goTo(PANEL_PAGE[kind])
    nav.openPanel(kind, id)
  }

  function openLink(link: AttentionLink): void {
    const sel = parseSel(link.sel)
    if (!sel) {
      nav.goTo(link.page, {}, { replace: false })
      return
    }
    nav.goTo(link.page)
    nav.openPanel(sel.kind, sel.id)
  }

  return { openEntity, openLink }
}
