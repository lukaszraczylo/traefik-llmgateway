import { defineStore } from 'pinia'

import { DEFAULT_PAGE } from '@/lib/pages'
import type { PageId } from '@/lib/pages'
import { formatSel, parseSel } from '@/lib/panel-selection'
import type { PanelKind, PanelSelection } from '@/lib/panel-selection'

function sameParams(a: Record<string, string>, b: Record<string, string>): boolean {
  const keys = Object.keys(a)
  return keys.length === Object.keys(b).length && keys.every((k) => a[k] === b[k])
}

/**
 * useNavStore is the redesign's single navigation store (redesign-plan.md
 * section 3.3, replacing the old activeTab/usageQuery shape): which page
 * is showing, and that page's own query params, as an opaque string
 * record. "Opaque" is deliberate — nav.ts itself never interprets a
 * param's meaning (a Spend page's `by`/`metric`/`ref`/`drill`, a
 * Consumers page's `q`/`view`/`user`, ...); each page (and its own store,
 * owned by that page's work package) reads `params` itself. This keeps
 * nav.ts, and the shell that wraps it (App.vue, SidebarNav.vue,
 * composables/useHashState.ts), free of any per-page knowledge — adding a
 * new page-specific param never touches this file.
 *
 * Global filters (time range, comparison, scope) are NOT part of
 * `params` — those live in stores/filters.ts, page-independent by
 * design (every page reads the same three).
 */
export const useNavStore = defineStore('nav', {
  state: () => ({
    page: DEFAULT_PAGE as PageId,
    params: {} as Record<string, string>,
    /** Set by openPanel / goTo(replace:false); composables/useHashState.ts consumes it to choose pushState over replaceState. */
    pushPending: false,
    /** True while the current history entry was created by openPanel, so closePanel can go Back instead of stacking another entry. */
    panelPushed: false,
  }),
  getters: {
    /** The open detail panel parsed from the `sel` param, or null. */
    selection: (state): PanelSelection | null => parseSel(state.params.sel),
  },
  actions: {
    /**
     * goTo navigates to `page` with `params` as that page's complete
     * param set (not merged with whatever the previous page's params
     * were — a page's own params are meaningless once you've left it).
     * Every EntityLink.vue click, SidebarNav.vue link, and TopList.vue
     * row calls this instead of poking `page`/`params` separately, so a
     * single click is always one atomic state change (and, downstream,
     * one hash rewrite — composables/useHashState.ts's own watcher).
     */
    goTo(page: PageId, params: Record<string, string> = {}, opts: { replace?: boolean } = {}): void {
      const unchanged = this.page === page && sameParams(this.params, params)
      this.page = page
      this.params = params
      // An unchanged hash never reaches useHashState's watcher, which is what clears this flag.
      this.pushPending = opts.replace === false && !unchanged
      this.panelPushed = false
    },
    /** Opens a detail panel and pushes a history entry, so Back closes it. */
    openPanel(kind: PanelKind, id: string): void {
      this.openSheet(formatSel(kind, id))
    },
    /** Opens a sheet whose `sel` is not an entity panel (e.g. `helper:router`), pushing a history entry like openPanel. */
    openSheet(sel: string): void {
      if (this.params.sel === sel) return
      this.params = { ...this.params, sel }
      this.pushPending = true
      this.panelPushed = true
    },
    /** Switches the open sheet's `sel` in place: no new history entry, and Back still closes it. */
    swapSheet(sel: string): void {
      if (this.params.sel === sel) return
      this.params = { ...this.params, sel }
      this.pushPending = false
    },
    /** Goes Back when the panel owns the last history entry; otherwise drops `sel` in place. */
    closePanel(): void {
      if (this.panelPushed && typeof window !== 'undefined') {
        this.panelPushed = false
        window.history.back()
        return
      }
      const { sel: _sel, ...rest } = this.params
      this.params = rest
      this.pushPending = false
      this.panelPushed = false
    },
  },
})
