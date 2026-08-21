import type { AdminUsageEntryView } from '@/types/api'

/**
 * Shared matching helpers for the "Filter users or groups" search
 * UsageView.vue and ChartsView.vue both offer (operator directive:
 * identical semantics, ONE implementation — no second copy of this logic).
 *
 * Every `query` parameter here is expected to already be trimmed and
 * lowercased by the caller, mirroring ProvidersView.vue's own
 * normalizedQuery convention (modelMatches/aliasMatches there never
 * re-normalize either) — these functions stay pure string/array
 * operations, with the caller owning the single source of truth for "is a
 * query active at all".
 */

/** userMatches: a user matches when its own id (display name) contains query. */
export function userMatches(user: Pick<AdminUsageEntryView, 'id'>, query: string): boolean {
  return user.id.toLowerCase().includes(query)
}

/** groupNameMatches: a group matches by its own id (display name) alone, ignoring its members. */
export function groupNameMatches(group: Pick<AdminUsageEntryView, 'id'>, query: string): boolean {
  return group.id.toLowerCase().includes(query)
}

/**
 * membersOfGroup: group's own member user rows, joined on
 * `groupName === group.id` — a user's row already carries its own group
 * name (admin.go's adminUsageEntryView.GroupName, set in buildAdminUsage),
 * so this is a plain client-side join against the already-polled
 * GET /admin/api/usage response, no extra fetch. UsageView.vue calls this
 * directly (replacing its own former local membersOf helper);
 * matchingMembersOfGroup below builds on it too, which is how
 * ChartsView.vue's groupMatches call ends up depending on this same join
 * — indirectly, through matchingMembersOfGroup — without calling
 * membersOfGroup itself.
 */
export function membersOfGroup(group: Pick<AdminUsageEntryView, 'id'>, users: AdminUsageEntryView[]): AdminUsageEntryView[] {
  return users.filter((u) => u.groupName === group.id)
}

/** matchingMembersOfGroup: group's own members that themselves match query. */
export function matchingMembersOfGroup(
  group: Pick<AdminUsageEntryView, 'id'>,
  users: AdminUsageEntryView[],
  query: string,
): AdminUsageEntryView[] {
  return membersOfGroup(group, users).filter((u) => userMatches(u, query))
}

/**
 * groupMatches: a group matches if its own name contains query OR it has
 * at least one matching member (binding search semantics). Callers that
 * need to distinguish WHICH of the two reasons a group matched — to decide
 * whether to show all of its members or only the matching ones — use
 * groupNameMatches and matchingMembersOfGroup directly instead (see
 * UsageView.vue's own doc comment for that distinction).
 */
export function groupMatches(group: Pick<AdminUsageEntryView, 'id'>, users: AdminUsageEntryView[], query: string): boolean {
  return groupNameMatches(group, query) || matchingMembersOfGroup(group, users, query).length > 0
}
