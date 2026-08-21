/**
 * Pure expand/collapse state for a search-filterable shadcn-vue Accordion —
 * shared by OverviewView.vue (providers) and UsageView.vue (groups), and
 * generalized (originally provider-expand.ts) so both consume ONE tested
 * implementation instead of two copies of the same logic (vue.md: "if
 * you've written it twice, you owe an abstraction"). Extracted out of the
 * component so the exact toggle sequence a review flagged (see
 * toggleItemExpand's own comment) is tested in isolation, without mounting
 * Vue — see search-expand.spec.ts (vitest), which exercises this module
 * directly, including the full flagged sequence.
 *
 * Two sets track state, keyed on each accordion item's own string id (a
 * provider name in Overview, a group id in Usage — this module has no
 * opinion on what the id means or on what counts as a "match"; callers
 * decide that and pass the resulting id list in):
 * - manuallyExpanded: items the user explicitly opened, independent of any
 *   search query. This is the state a cleared search returns to.
 * - manuallyCollapsed: items the user explicitly closed WHILE a search
 *   query was auto-expanding them. Only ever consulted while a query is
 *   active (computeExpandedItems below) — once the query clears, effective
 *   state is manuallyExpanded alone, so entries here become inert.
 *   clearExpandOverrides drops them anyway, so a stale collapse from one
 *   search session cannot silently suppress a match in an unrelated later
 *   one.
 */
export interface ExpandState {
  manuallyExpanded: Set<string>
  manuallyCollapsed: Set<string>
}

/**
 * computeExpandedItems returns the effective set of expanded item ids:
 * manuallyExpanded verbatim when no query is active; with a query active,
 * manuallyExpanded plus every id in matchingIds that is not in
 * manuallyCollapsed (auto-expand every match, unless the user explicitly
 * closed that one). The caller decides what "matching" means for its own
 * view, and this module has no opinion on it either way — OverviewView.vue
 * passes every provider that itself matched at all (every match there
 * implies at least one matching model, so every match auto-expands);
 * UsageView.vue instead passes only the subset of its own matches that
 * matched via a member rather than via the group's own name (see that
 * view's own doc comment for why it draws that narrower distinction).
 */
export function computeExpandedItems(state: ExpandState, hasQuery: boolean, matchingIds: string[]): Set<string> {
  if (!hasQuery) return new Set(state.manuallyExpanded)
  const expanded = new Set(state.manuallyExpanded)
  for (const id of matchingIds) {
    if (!state.manuallyCollapsed.has(id)) expanded.add(id)
  }
  return expanded
}

/**
 * toggleItemExpand mutates state for one item, given whether it is
 * CURRENTLY effectively expanded (computeExpandedItems' own result for
 * that id — the caller passes this, rather than the function re-deriving
 * it, so callers stay in charge of what "current" means for their own
 * render).
 *
 * This is the exact fix for a review-flagged bug: the original
 * implementation toggled manuallyExpanded.has(id) directly — the RAW
 * manual flag, not the effective (possibly auto-expanded) one. Clicking a
 * row that was only auto-expanded (matching the search, never manually
 * opened) read manuallyExpanded.has(id) as false and ADDED it to
 * manuallyExpanded — invisible immediately (it was already showing via
 * auto-expand), but wrongly left it expanded after the query cleared,
 * since manuallyExpanded now contained an item the user never actually
 * chose to open. Toggling against the EFFECTIVE state fixes this in both
 * directions:
 * - Expanded only via auto-match, clicked to close: manuallyExpanded never
 *   gains the entry (nothing to remove); manuallyCollapsed gains it,
 *   suppressing the auto-expand for as long as the query stays active.
 *   Clearing the query drops back to manuallyExpanded, which never had it
 *   — correctly collapsed.
 * - Expanded via a manual open from BEFORE a search started, still
 *   matches, clicked to close during the search: manuallyExpanded loses it
 *   (the user's own most recent action wins) and manuallyCollapsed gains
 *   it, so it also stays closed after the query clears — treating the
 *   in-search click as the user's current intent, not the older,
 *   pre-search one.
 */
export function toggleItemExpand(state: ExpandState, id: string, currentlyExpanded: boolean): void {
  if (currentlyExpanded) {
    state.manuallyExpanded.delete(id)
    state.manuallyCollapsed.add(id)
  } else {
    state.manuallyCollapsed.delete(id)
    state.manuallyExpanded.add(id)
  }
}

/** clearExpandOverrides drops every manuallyCollapsed entry — called when the search query becomes empty, so a suppression from one search session never carries into an unrelated later one (manuallyCollapsed is otherwise never consulted outside an active query anyway; this only matters for a FUTURE query). */
export function clearExpandOverrides(state: ExpandState): void {
  state.manuallyCollapsed.clear()
}
