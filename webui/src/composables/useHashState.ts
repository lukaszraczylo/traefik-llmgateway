import { computed, onMounted, onUnmounted, watch } from 'vue'

import { buildHash, parseGlobalParams, parseHashState, parsePageParams } from '@/lib/hash-state'
import { DEFAULT_CMP, DEFAULT_RANGE } from '@/lib/range'
import { DEFAULT_SCOPE, useFiltersStore } from '@/stores/filters'
import { useNavStore } from '@/stores/nav'

/**
 * useHashState (redesign-plan.md section 3.1) is the single composable
 * that keeps the URL hash and the two shell stores it drives — stores/
 * filters.ts (range/cmp/scope) and stores/nav.ts (page/params) — in sync,
 * both ways:
 *
 * - On creation, it parses the CURRENT hash exactly once and applies it:
 *   the three global filters to filters.setFilters, and the page plus
 *   its own opaque param record to nav.goTo. Every per-page STORE
 *   (spend/consumers/models/reliability, each owned by its own work
 *   package) is responsible for reading nav.params itself when its page
 *   mounts — this composable never touches a page-specific store, by
 *   design (stores/nav.ts's own doc comment).
 * - A single computed re-derives the hash from filters' current state
 *   plus nav.page/nav.params, and writes it back via history.
 *   replaceState whenever it changes — never pushState (one browser-
 *   history entry per admin session, not per click) and never a bare
 *   `location.hash =` assignment (which would also jump scroll position
 *   to any element whose id happens to match).
 * - It listens for the browser's own `hashchange` event (back/forward
 *   navigation, or a hand-edited URL) and re-parses from scratch,
 *   reapplying the same restore path as the initial read.
 *
 * MUST be called from App.vue's setup, before the `v-if="!auth.
 * isAuthenticated"` branch — filters.setFilters/nav.goTo are plain
 * synchronous state writes, safe to call before an admin key is stored,
 * so the hash's selection is already restored by the time the reader
 * authenticates.
 *
 * Guarded for a `window`-less environment (vitest's node test
 * environment, vite.config.ts): every DOM/history/location access sits
 * behind one hasWindow check computed once. Exercised through App.vue in
 * the real browser, not unit-tested directly — lib/hash-state.ts's own
 * pure functions are what hash-state.spec.ts covers.
 */
export function useHashState(): void {
  const filters = useFiltersStore()
  const nav = useNavStore()

  function applyFromHash(hash: string): void {
    const { page, query } = parseHashState(hash)
    const globals = parseGlobalParams(query)
    filters.setFilters({
      range: globals.range ?? DEFAULT_RANGE,
      cmp: globals.cmp ?? DEFAULT_CMP,
      scope: globals.scope ?? DEFAULT_SCOPE,
    })
    nav.goTo(page, parsePageParams(query))
  }

  const hasWindow = typeof window !== 'undefined'
  if (hasWindow) applyFromHash(window.location.hash)

  const currentHash = computed<string>(() => {
    const params: Record<string, string> = {}
    if (filters.range !== DEFAULT_RANGE) params.range = filters.range
    if (filters.cmp !== DEFAULT_CMP) params.cmp = filters.cmp
    if (filters.scope !== DEFAULT_SCOPE) params.scope = filters.scope
    // Page params are merged in AFTER the three global keys, so the
    // built query string always lists range/cmp/scope first — readable,
    // and matches redesign-plan.md section 3.1's own example ordering.
    // No current page uses a param named range/cmp/scope (see stores/
    // nav.ts's own doc comment on `params` staying opaque), so this
    // merge never actually overwrites one of the three above in
    // practice; it is a plain Object.assign, not a defensive filter.
    Object.assign(params, nav.params)
    return buildHash(nav.page, params)
  })

  // `immediate: true` (P3 item 24): applyFromHash already normalized
  // filters/nav state from window.location.hash above, synchronously,
  // before this watch is even registered — so currentHash's FIRST
  // computed value is already the canonical hash for whatever the reader
  // arrived with. Without `immediate`, that first value is never written
  // back until some LATER state change happens to fire the watch, so a
  // legacy or garbage hash (`#charts`, `#nope?range=30d`) stayed in the
  // address bar — correct app state, stale URL — until the reader
  // happened to touch a filter.
  watch(
    currentHash,
    (hash) => {
      if (!hasWindow) return
      if (window.location.hash === hash) return
      window.history.replaceState(null, '', hash)
    },
    { immediate: true },
  )

  function onHashChange(): void {
    applyFromHash(window.location.hash)
  }

  if (hasWindow) {
    onMounted(() => window.addEventListener('hashchange', onHashChange))
    onUnmounted(() => window.removeEventListener('hashchange', onHashChange))
  }
}
