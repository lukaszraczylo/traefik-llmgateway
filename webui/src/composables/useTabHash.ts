// Feature C (v0.22) introduced hashToTab/tabToHash below, plus a
// useTabHash() composable wrapping them. F9 (dashboard-plan.md) supersedes
// that composable with useHashState() (composables/useHashState.ts):
// App.vue now needs a single hash that carries BOTH the active tab AND
// each tab's own query-string state (scope/window/tab/metric for Charts, q
// for Usage, kind/user for Events — lib/hash-state.ts), not the tab alone,
// so useTabHash() itself was removed as the now-superseded, unreachable
// half of this file. hashToTab/tabToHash stay: useHashState.ts still
// builds the OUTER "#<tab>" portion of the combined hash directly on top
// of these same two pure functions, and this module's own spec exercises
// them directly (vite.config.ts's `test` block runs in a Node environment
// — no `window`, no component mount).
//
// TAB_VALUES/TabValue also live here — the single source of truth for the
// app's tab union (vue.md: "share types once, import everywhere"):
// stores/nav.ts's `activeTab`, composables/useHashState.ts, and App.vue's
// own Tabs all import the SAME union rather than each redeclaring it.
export const TAB_VALUES = ['providers', 'usage', 'charts', 'targets', 'events'] as const
export type TabValue = (typeof TAB_VALUES)[number]

/**
 * hashToTab maps a raw location.hash string (with or without its leading
 * "#") to one of validTabs. An absent, empty, or unrecognized hash
 * returns defaultTab — never throws, and never returns a value outside
 * validTabs. Matching is a plain string comparison: a tab value is
 * already a fixed lowercase identifier (App.vue's TabsTrigger value=
 * strings), never re-cased anywhere in this pipeline.
 *
 * Two intentional URL-normalization quirks (folded review minors, v0.22
 * review round), both worth naming rather than leaving implicit:
 *
 * 1. Case sensitivity: "#Providers" does NOT match "providers" — it falls
 *    back to defaultTab like any other unrecognized hash, rather than
 *    being normalized case-insensitively. A URL a person typed or edited
 *    by hand with different casing is treated as invalid, not corrected.
 * 2. An invalid or absent hash is normalized only in memory (this
 *    function's own return value), never in the address bar itself:
 *    useHashState's initial read (composables/useHashState.ts) never
 *    calls history.replaceState on its own — only a LATER state change
 *    does (see its own doc comment) — so loading "#bogus" shows the
 *    Providers tab while the address bar still reads "#bogus" until the
 *    reader switches tabs, or something else changes, at least once.
 */
export function hashToTab<T extends string>(hash: string, validTabs: readonly T[], defaultTab: T): T {
  const raw = hash.startsWith('#') ? hash.slice(1) : hash
  return (validTabs as readonly string[]).includes(raw) ? (raw as T) : defaultTab
}

/** tabToHash is hashToTab's inverse: the URL hash (leading "#" included) one tab value maps to. */
export function tabToHash(tab: string): string {
  return `#${tab}`
}
