import { hashToTab, TAB_VALUES, tabToHash, type TabValue } from '@/composables/useTabHash'
import type { ChartTab, ModelMetric } from '@/stores/history'
import type { HistoryWindow } from '@/types/api'

/**
 * DEFAULT_TAB is the app's landing tab when the hash names nothing
 * recognized — parseHashState's own fallback, mirroring the old
 * useTabHash's defaultTab parameter (which every call site passed
 * 'providers' for anyway, so this needed no parameter of its own once
 * folded into one composable, composables/useHashState.ts).
 */
export const DEFAULT_TAB: TabValue = 'providers'

export interface HashState {
  tab: TabValue
  query: URLSearchParams
}

/**
 * parseHashState splits a raw location.hash ("#charts?window=day&scope=
 * user:alice") into its outer tab — validated against TAB_VALUES via
 * hashToTab, falling back to DEFAULT_TAB exactly like the pre-F9 useTabHash
 * did for an absent/unrecognized/bogus tab — and the remaining query
 * string, parsed once into a URLSearchParams every per-tab validator below
 * reads from. A hash with no "?" (a bare "#usage", or the very first load
 * with no hash at all) parses to an empty query, never throws.
 */
export function parseHashState(hash: string): HashState {
  const raw = hash.startsWith('#') ? hash.slice(1) : hash
  const qIndex = raw.indexOf('?')
  const tabPart = qIndex === -1 ? raw : raw.slice(0, qIndex)
  const queryPart = qIndex === -1 ? '' : raw.slice(qIndex + 1)
  const tab = hashToTab(tabPart, TAB_VALUES, DEFAULT_TAB)
  return { tab, query: new URLSearchParams(queryPart) }
}

/**
 * buildHash re-serializes a tab plus an already-default-stripped param
 * record back into a "#<tab>?<query>" string (F9) — mirrors tabToHash for
 * the tab-only portion, adding a "?<query>" suffix only when `params` has
 * at least one entry, so the common "nothing but the default selection"
 * case round-trips to a bare "#<tab>" rather than a noisy "#<tab>?".
 *
 * "Omit defaults" is the CALLER's job, not this function's: what counts
 * as default differs per field (window's default is 'hour', scope's is
 * 'total', ...), so composables/useHashState.ts's own computed simply
 * never puts a default-valued key into `params` in the first place.
 * buildHash itself only defends against an explicitly empty-string value
 * slipping through, dropping it the same way.
 */
export function buildHash(tab: TabValue, params: Record<string, string>): string {
  const entries = Object.entries(params).filter(([, v]) => v !== '')
  if (entries.length === 0) return tabToHash(tab)
  return `${tabToHash(tab)}?${new URLSearchParams(entries).toString()}`
}

const HISTORY_WINDOWS: readonly HistoryWindow[] = ['hour', 'day', 'month']
const CHART_TABS: readonly ChartTab[] = ['requests', 'tokens', 'cost', 'models']
const MODEL_METRICS: readonly ModelMetric[] = ['req', 'tokin', 'tokout', 'cost']

/** SCOPE_PATTERN mirrors history.ts's own scope string convention: "total", or one of the three "{kind}:{id}" forms — an id may itself contain almost anything (a model id can contain slashes), so each kind only checks its own prefix plus a non-empty remainder, never the id's own shape. */
const SCOPE_PATTERN = /^(total|user:.+|group:.+|model:.+)$/

export interface ChartsHashParams {
  window?: HistoryWindow
  tab?: ChartTab
  metric?: ModelMetric
  scope?: string
  /**
   * The Models tab's provider-prefix filter (stores/history.ts's
   * modelFilter), round-tripped through the hash so a provider-header
   * link (nav.goToModels) or a hand-edited `#charts?tab=models&filter=`
   * URL survives a reload or a hashchange — free text, not an enum, same
   * convention as `scope` below and Usage's own `q` param.
   */
  filter?: string
}

/**
 * parseChartsParams validates the Charts tab's own hash query params.
 * Each field is independently OMITTED (never throws, never substitutes a
 * fabricated default itself) when absent or not one of its own known
 * values — a hand-edited or stale-version URL with `window=fortnight` or
 * `metric=bogus` degrades to "use whatever this field already is"
 * (composables/useHashState.ts's own caller decides the actual default),
 * rather than failing the whole hash or resetting every OTHER, valid
 * field alongside the one bad one.
 */
export function parseChartsParams(query: URLSearchParams): ChartsHashParams {
  const result: ChartsHashParams = {}
  const window = query.get('window')
  if (window && (HISTORY_WINDOWS as readonly string[]).includes(window)) result.window = window as HistoryWindow
  const tab = query.get('tab')
  if (tab && (CHART_TABS as readonly string[]).includes(tab)) result.tab = tab as ChartTab
  const metric = query.get('metric')
  if (metric && (MODEL_METRICS as readonly string[]).includes(metric)) result.metric = metric as ModelMetric
  const scope = query.get('scope')
  if (scope && SCOPE_PATTERN.test(scope)) result.scope = scope
  const filter = query.get('filter')
  if (filter) result.filter = filter
  return result
}

export interface UsageHashParams {
  q?: string
}

/** parseUsageParams reads the Usage tab's own single `q` param. It is free-text search input, not an enum, so there is nothing to validate beyond "present and non-empty" — any string round-trips as-is. */
export function parseUsageParams(query: URLSearchParams): UsageHashParams {
  const q = query.get('q')
  return q ? { q } : {}
}

export interface EventsHashParams {
  kind?: string
  user?: string
}

/**
 * parseEventsParams reads the Events tab's own kind/user params. `kind` is
 * deliberately NOT validated against AdminEventKind here (unlike Charts'
 * enum fields above): lib/events-filter.ts's filterEvents already treats
 * an unrecognized kind as "matches nothing" harmlessly, and rejecting it
 * here would silently drop a link built against a server-added kind this
 * webui build predates — the same "still carry it through" convention
 * AdminEventView.kind's own `| string` union documents (types/api.ts).
 */
export function parseEventsParams(query: URLSearchParams): EventsHashParams {
  const result: EventsHashParams = {}
  const kind = query.get('kind')
  const user = query.get('user')
  if (kind) result.kind = kind
  if (user) result.user = user
  return result
}
