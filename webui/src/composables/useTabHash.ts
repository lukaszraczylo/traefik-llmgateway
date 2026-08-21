import type { Ref } from 'vue'
import { onMounted, onUnmounted, ref, watch } from 'vue'

// Feature C (v0.22): syncs the active dashboard tab to the URL hash
// (#providers, #usage, #charts, #targets — App.vue's own TabsTrigger
// value strings, unchanged) with no router dependency: App.vue's Tabs
// component already just needs a plain string ref, and a full router is
// overkill for one hash-backed value. hashToTab/tabToHash are the pure
// mapping this module's own spec exercises directly (vite.config.ts's
// `test` block runs in a Node environment — no `window`, no component
// mount), so every `window`/`history`/`location` touch below is guarded
// and lives only inside useTabHash itself, never at module scope.

/**
 * hashToTab maps a raw location.hash string (with or without its leading
 * "#") to one of validTabs. An absent, empty, or unrecognized hash
 * returns defaultTab — never throws, and never returns a value outside
 * validTabs. Matching is a plain string comparison: a tab value is
 * already a fixed lowercase identifier (App.vue's TabsTrigger value=
 * strings), never re-cased anywhere in this pipeline.
 */
export function hashToTab<T extends string>(hash: string, validTabs: readonly T[], defaultTab: T): T {
  const raw = hash.startsWith('#') ? hash.slice(1) : hash
  return (validTabs as readonly string[]).includes(raw) ? (raw as T) : defaultTab
}

/** tabToHash is hashToTab's inverse: the URL hash (leading "#" included) one tab value maps to. */
export function tabToHash(tab: string): string {
  return `#${tab}`
}

export interface UseTabHashOptions<T extends string> {
  /** The full set of recognized tab values — App.vue's Tabs value strings. */
  validTabs: readonly T[]
  /** The tab an absent or unrecognized hash falls back to. */
  defaultTab: T
}

/**
 * useTabHash returns a ref<T> wired bidirectionally to the URL hash:
 *
 * - On creation, it reads the CURRENT hash (hashToTab) as its initial
 *   value — the "on load: read hash -> select tab" half of the spec.
 * - On every ref change (a Tabs v-model update from a user click), it
 *   writes the hash back via history.replaceState — never pushState or a
 *   bare `location.hash =` assignment, both of which would grow browser
 *   history with one entry per tab click and, for the latter, jump
 *   scroll position to any element whose id matches the new hash. A
 *   no-op write (the hash already matches) is skipped so a rapid
 *   double-toggle does not spam replaceState.
 * - It listens for the browser's own `hashchange` event (back/forward
 *   navigation, or a hand-edited URL) and updates the ref to match —
 *   invalid/unrecognized hands back defaultTab, exactly like the initial
 *   read.
 *
 * Guarded for a `window`-less environment (vitest's node test
 * environment, vite.config.ts): every DOM/history/location access is
 * behind a single hasWindow check computed once, so importing or calling
 * this module never throws there — though the composable itself, being a
 * real Vue lifecycle hook user, is exercised through App.vue in the real
 * browser, not unit-tested directly; hashToTab/tabToHash above are what
 * this module's own spec covers.
 */
export function useTabHash<T extends string>({ validTabs, defaultTab }: UseTabHashOptions<T>): Ref<T> {
  const hasWindow = typeof window !== 'undefined'
  const activeTab = ref(hasWindow ? hashToTab(window.location.hash, validTabs, defaultTab) : defaultTab) as Ref<T>

  watch(activeTab, (tab) => {
    if (!hasWindow) return
    const nextHash = tabToHash(tab)
    if (window.location.hash === nextHash) return
    window.history.replaceState(null, '', nextHash)
  })

  function onHashChange(): void {
    activeTab.value = hashToTab(window.location.hash, validTabs, defaultTab)
  }

  if (hasWindow) {
    onMounted(() => window.addEventListener('hashchange', onHashChange))
    onUnmounted(() => window.removeEventListener('hashchange', onHashChange))
  }

  return activeTab
}
