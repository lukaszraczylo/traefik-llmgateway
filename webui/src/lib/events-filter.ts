import type { AdminEventKind, AdminEventView } from '@/types/api'

/** EVENT_KIND_LABEL is the Events view's display text for each AdminEventKind — both the kind-filter Select's option labels and each row's Kind badge. */
export const EVENT_KIND_LABEL: Record<AdminEventKind, string> = {
  rate_limit: 'Rate limit',
  budget: 'Budget',
  store_down: 'Store down',
  upstream: 'Upstream error',
  timeout: 'Timeout',
  unpriced: 'Unpriced model',
  capacity: 'Capacity',
}

/**
 * EVENT_KINDS (P11 review fix, DRY) is the ONE ordered list of every known
 * AdminEventKind, derived from EVENT_KIND_LABEL's own keys rather than a
 * third independent literal array — this codebase had the kind set spelled
 * out three times (the AdminEventKind union itself, types/api.ts;
 * EVENT_KIND_LABEL's keys, above; and EventsView.vue's own local
 * KIND_OPTIONS array), which is exactly the vue.md "if you've written it
 * twice, you owe an abstraction" case. `Object.keys` on a `Record<K,
 * string>` widens to `string[]`, so this re-asserts the same union type
 * the object literal above was already checked against — never a runtime
 * risk of drifting from it, since adding/removing a kind requires editing
 * EVENT_KIND_LABEL either way (a missing/extra key is a compile error
 * there first).
 */
export const EVENT_KINDS = Object.keys(EVENT_KIND_LABEL) as AdminEventKind[]

/**
 * eventKindVariant maps a kind to the shadcn-vue Badge variant its row
 * renders (lib/events-columns.ts): destructive for the three kinds where a
 * request was actually DENIED (rate_limit, budget, capacity — money or
 * quota was protected), secondary for an infra/upstream failure that is
 * not itself a policy decision (store_down, upstream, timeout), outline
 * for the purely informational unpriced kind (nothing was denied or
 * failed) — and outline again as the fallback for a kind this webui build
 * does not recognize (a server newer than this client), mirroring
 * target-columns.ts's healthCell "unrecognized reads as the neutral
 * state, never the alarming one" convention.
 */
export function eventKindVariant(kind: string): 'destructive' | 'secondary' | 'outline' {
  switch (kind) {
    case 'rate_limit':
    case 'budget':
    case 'capacity':
      return 'destructive'
    case 'store_down':
    case 'upstream':
    case 'timeout':
      return 'secondary'
    default:
      return 'outline'
  }
}

/** EventFilter is filterEvents' own query shape — kind is an exact AdminEventKind match ('' means "every kind"); user is matched, case-insensitively, against EITHER the event's user OR its group (a group-scoped event has no user, and vice versa — see AdminEventView's own doc comment). */
export interface EventFilter {
  kind: string
  user: string
}

/**
 * filterEvents narrows the (already newest-first) event list to the
 * current kind/user selection. `filter.user` is matched pre-normalized —
 * callers pair this with useSearchQuery()'s own `normalized` the same way
 * every other search filter in this panel does (ProvidersView.vue's
 * modelMatches, lib/usage-search.ts) — this function itself does no
 * trimming or casing of its own beyond a defensive `.trim().toLowerCase()`
 * so it is still correct if a caller passes the raw query directly.
 */
export function filterEvents(events: AdminEventView[], filter: EventFilter): AdminEventView[] {
  const kind = filter.kind
  const user = filter.user.trim().toLowerCase()
  return events.filter((e) => {
    if (kind && e.kind !== kind) return false
    if (!user) return true
    const userMatch = e.user?.toLowerCase().includes(user) ?? false
    const groupMatch = e.group?.toLowerCase().includes(user) ?? false
    return userMatch || groupMatch
  })
}
