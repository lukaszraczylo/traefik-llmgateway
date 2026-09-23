import { defineStore } from 'pinia'

import { DEFAULT_PAGE } from '@/lib/pages'
import type { PageId } from '@/lib/pages'

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
  }),
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
    goTo(page: PageId, params: Record<string, string> = {}): void {
      this.page = page
      this.params = params
    },
  },
})
