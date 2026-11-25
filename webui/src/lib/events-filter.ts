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
  paced: 'Paced',
  router_escalation: 'Router escalation',
  router_timeout_fallback: 'Router timeout fallback',
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
 * kindLabel (reuse-audit.md F13) renders one event's kind as display text,
 * falling back to the raw kind string for a kind EVENT_KIND_LABEL does not
 * know (a server newer than this webui build) — same forward-compat
 * convention AdminEventView.kind's own `| string` union documents. The ONE
 * shared implementation: lib/events-columns.ts's Events-tab column and
 * HomePage.vue's "Latest events" card used to each hand-roll an identical
 * copy of this exact ternary.
 */
export function kindLabel(kind: string): string {
  return EVENT_KIND_LABEL[kind as AdminEventKind] ?? kind
}

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

/** EventFilter is filterEvents' own query shape — kind is an exact AdminEventKind match ('' means "every kind"); user is matched, case-insensitively, against EITHER the event's user OR its group; provider is an exact match and route a substring match, both case-insensitive and ignored when absent or empty. */
export interface EventFilter {
  kind: string
  user: string
  provider?: string
  route?: string
}

/**
 * filterEvents narrows the (already newest-first) event list to the
 * current selection. `filter.user` is matched pre-normalized by the
 * caller; a defensive trim and lower-casing keeps it correct on raw input.
 */
export function filterEvents(events: AdminEventView[], filter: EventFilter): AdminEventView[] {
  const kind = filter.kind
  const user = filter.user.trim().toLowerCase()
  const provider = (filter.provider ?? '').trim().toLowerCase()
  const route = (filter.route ?? '').trim().toLowerCase()
  return events.filter((e) => {
    if (kind && e.kind !== kind) return false
    if (provider && (e.provider ?? '').toLowerCase() !== provider) return false
    if (route && !e.route.toLowerCase().includes(route)) return false
    if (!user) return true
    const userMatch = e.user?.toLowerCase().includes(user) ?? false
    const groupMatch = e.group?.toLowerCase().includes(user) ?? false
    return userMatch || groupMatch
  })
}

/** The hash params the Events page reads; each maps to one FilterBar field of the same id. */
export const EVENT_FILTER_FIELDS = ['kind', 'user', 'provider', 'route'] as const
export type EventFilterField = (typeof EVENT_FILTER_FIELDS)[number]

type ParamBag = Record<string, string | undefined>

export function eventFilterFromParams(params: ParamBag): Required<EventFilter> {
  const get = (k: EventFilterField): string => (params[k] ?? '').trim()
  return { kind: get('kind'), user: get('user'), provider: get('provider'), route: get('route') }
}

export interface EventFilterItem {
  id: string
  field: string
  value: string | number | boolean | string[]
}

/** One chip per non-empty filter; the chip id is its field so a re-applied filter replaces the old one. */
export function itemsFromFilter(filter: Partial<EventFilter>): EventFilterItem[] {
  return EVENT_FILTER_FIELDS.flatMap((field) => {
    const value = (filter[field] ?? '').trim()
    return value ? [{ id: field, field, value }] : []
  })
}

/** Back to hash params: one value per field (the last chip wins), unknown fields and empty values dropped. */
export function paramsFromItems(items: EventFilterItem[]): Partial<Record<EventFilterField, string>> {
  const out: Partial<Record<EventFilterField, string>> = {}
  for (const item of items) {
    if (!(EVENT_FILTER_FIELDS as readonly string[]).includes(item.field)) continue
    const value = String(item.value).trim()
    if (value) out[item.field as EventFilterField] = value
  }
  return out
}
