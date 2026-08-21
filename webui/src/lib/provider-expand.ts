/**
 * Pure expand/collapse state for OverviewView.vue's provider-model
 * accordion, extracted out of the component so the exact toggle sequence
 * a review flagged (see toggleProviderExpand's own comment) is tested in
 * isolation, without mounting Vue — see provider-expand.spec.ts (vitest),
 * which exercises this module directly, including the full flagged
 * sequence.
 *
 * Two sets track state:
 * - manuallyExpanded: providers the user explicitly opened, independent
 *   of any search query. This is the state a cleared search returns to.
 * - manuallyCollapsed: providers the user explicitly closed WHILE a
 *   search query was auto-expanding them. Only ever consulted while a
 *   query is active (computeExpandedProviders below) — once the query
 *   clears, effective state is manuallyExpanded alone, so entries here
 *   become inert. clearExpandOverrides drops them anyway, so a stale
 *   collapse from one search session cannot silently suppress a match in
 *   an unrelated later one.
 */
export interface ExpandState {
  manuallyExpanded: Set<string>
  manuallyCollapsed: Set<string>
}

/**
 * computeExpandedProviders returns the effective set of expanded
 * provider names: manuallyExpanded verbatim when no query is active;
 * with a query active, manuallyExpanded plus every name in
 * matchingNames that is not in manuallyCollapsed (auto-expand every
 * match, unless the user explicitly closed that one).
 */
export function computeExpandedProviders(state: ExpandState, hasQuery: boolean, matchingNames: string[]): Set<string> {
  if (!hasQuery) return new Set(state.manuallyExpanded)
  const expanded = new Set(state.manuallyExpanded)
  for (const name of matchingNames) {
    if (!state.manuallyCollapsed.has(name)) expanded.add(name)
  }
  return expanded
}

/**
 * toggleProviderExpand mutates state for one provider, given whether it
 * is CURRENTLY effectively expanded (computeExpandedProviders' own
 * result for that name — the caller passes this, rather than the
 * function re-deriving it, so callers stay in charge of what "current"
 * means for their own render).
 *
 * This is the exact fix for a review-flagged bug: the original
 * implementation toggled manuallyExpanded.has(name) directly — the RAW
 * manual flag, not the effective (possibly auto-expanded) one. Clicking
 * a row that was only auto-expanded (matching the search, never
 * manually opened) read manuallyExpanded.has(name) as false and ADDED
 * it to manuallyExpanded — invisible immediately (it was already
 * showing via auto-expand), but wrongly left it expanded after the
 * query cleared, since manuallyExpanded now contained a provider the
 * user never actually chose to open. Toggling against the EFFECTIVE
 * state fixes this in both directions:
 * - Expanded only via auto-match, clicked to close: manuallyExpanded
 *   never gains the entry (nothing to remove); manuallyCollapsed gains
 *   it, suppressing the auto-expand for as long as the query stays
 *   active. Clearing the query drops back to manuallyExpanded, which
 *   never had it — correctly collapsed.
 * - Expanded via a manual open from BEFORE a search started, still
 *   matches, clicked to close during the search: manuallyExpanded loses
 *   it (the user's own most recent action wins) and manuallyCollapsed
 *   gains it, so it also stays closed after the query clears — treating
 *   the in-search click as the user's current intent, not the older,
 *   pre-search one.
 */
export function toggleProviderExpand(state: ExpandState, name: string, currentlyExpanded: boolean): void {
  if (currentlyExpanded) {
    state.manuallyExpanded.delete(name)
    state.manuallyCollapsed.add(name)
  } else {
    state.manuallyCollapsed.delete(name)
    state.manuallyExpanded.add(name)
  }
}

/** clearExpandOverrides drops every manuallyCollapsed entry — called when the search query becomes empty, so a suppression from one search session never carries into an unrelated later one (manuallyCollapsed is otherwise never consulted outside an active query anyway; this only matters for a FUTURE query). */
export function clearExpandOverrides(state: ExpandState): void {
  state.manuallyCollapsed.clear()
}
