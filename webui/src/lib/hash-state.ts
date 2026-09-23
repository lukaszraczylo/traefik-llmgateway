import { DEFAULT_PAGE, isPageId } from '@/lib/pages'
import type { PageId } from '@/lib/pages'
import { isCmpMode, isRangeKey } from '@/lib/range'
import type { CmpMode, RangeKey } from '@/lib/range'

/**
 * LEGACY_PAGE_MAP redirects the pre-redesign flat-tab hash names to their
 * redesign home (redesign-plan.md section 3.1's legacy mapping): a
 * bookmark or shared link built against the old #providers/#usage/
 * #charts/#events/#targets shell still lands somewhere sensible instead
 * of falling back to the default page. `targets` keeps its own name —
 * the MCP & Agents page is the one page that did not move.
 *
 * The OLD hash's query params never carry over (see parseHashState below)
 * — they were shaped for the old tab (e.g. #charts's `tab=models&metric=
 * cost`), and none of that maps cleanly onto the new page's own param
 * names, so a legacy hash lands on a clean instance of its mapped page
 * rather than misapplying a stale param.
 */
const LEGACY_PAGE_MAP: Record<string, PageId> = {
  providers: 'models',
  usage: 'consumers',
  charts: 'spend',
  events: 'reliability',
  targets: 'targets',
}

export interface HashState {
  page: PageId
  query: URLSearchParams
}

/**
 * parseHashState splits a raw location.hash ("#spend?range=30d&by=model")
 * into its outer page and the remaining query string. Three cases:
 *
 * 1. The page segment is a current PageId (lib/pages.ts) — parse the
 *    query normally.
 * 2. The page segment is a legacy tab name (LEGACY_PAGE_MAP) — map to the
 *    new page, with an EMPTY query (old params dropped, see the map's own
 *    doc comment).
 * 3. Anything else (absent, empty, or unrecognized) — DEFAULT_PAGE, query
 *    parsed as-is (so a malformed page segment does not also swallow an
 *    otherwise-valid query string).
 *
 * Never throws.
 */
export function parseHashState(hash: string): HashState {
  const raw = hash.startsWith('#') ? hash.slice(1) : hash
  const qIndex = raw.indexOf('?')
  const pagePart = qIndex === -1 ? raw : raw.slice(0, qIndex)
  const queryPart = qIndex === -1 ? '' : raw.slice(qIndex + 1)

  if (isPageId(pagePart)) return { page: pagePart, query: new URLSearchParams(queryPart) }

  const legacy = LEGACY_PAGE_MAP[pagePart]
  if (legacy) return { page: legacy, query: new URLSearchParams() }

  return { page: DEFAULT_PAGE, query: new URLSearchParams(queryPart) }
}

/**
 * buildHash re-serializes a page plus an already-default-stripped param
 * record (global filters and/or page params merged by the caller) back
 * into a "#<page>?<query>" string. "Omit defaults" is the CALLER's job —
 * composables/useHashState.ts's own computed decides what counts as
 * default for range/cmp/scope; buildHash itself only defends against an
 * explicitly empty-string value slipping through, dropping it the same
 * way. No query suffix at all when `params` ends up empty, so the common
 * "nothing but defaults" case round-trips to a bare "#<page>".
 */
export function buildHash(page: PageId, params: Record<string, string>): string {
  const entries = Object.entries(params).filter(([, v]) => v !== '')
  if (entries.length === 0) return `#${page}`
  return `#${page}?${new URLSearchParams(entries).toString()}`
}

export interface GlobalHashParams {
  range?: RangeKey
  cmp?: CmpMode
  scope?: string
}

/** SCOPE_PATTERN mirrors the global filter bar's own scope convention (redesign-plan.md section 3.1): "all", or one of the three "{kind}:{id}" forms — an id may contain almost anything, so each kind only checks its own prefix plus a non-empty remainder. Exported so GlobalFilterBar.vue validates its own scope input against the exact same rule this module uses to parse it back out of the hash. */
export const SCOPE_PATTERN = /^(all|group:.+|user:.+|provider:.+)$/

/**
 * parseGlobalParams validates the three page-independent hash params
 * (range/cmp/scope) every page's hash shares. Each field is independently
 * OMITTED (never throws) when absent or not a recognized value — a
 * hand-edited or stale-version URL with `range=fortnight` degrades to
 * "use whatever this field already is" (the caller decides the actual
 * default), rather than failing the whole hash or resetting the other,
 * valid fields alongside the one bad one.
 */
export function parseGlobalParams(query: URLSearchParams): GlobalHashParams {
  const result: GlobalHashParams = {}
  const range = query.get('range')
  if (range && isRangeKey(range)) result.range = range
  const cmp = query.get('cmp')
  if (cmp && isCmpMode(cmp)) result.cmp = cmp
  const scope = query.get('scope')
  if (scope && SCOPE_PATTERN.test(scope)) result.scope = scope
  return result
}

/** GLOBAL_PARAM_KEYS names the three query keys parseGlobalParams reads — parsePageParams below excludes exactly these, and nothing else, from a page's own param record. */
const GLOBAL_PARAM_KEYS: ReadonlySet<string> = new Set(['range', 'cmp', 'scope'])

/**
 * parsePageParams returns every hash query param EXCEPT the three global
 * ones, verbatim, as a plain record. Deliberately opaque (no per-page
 * validation here — see stores/nav.ts's own doc comment for why): each
 * page owns interpreting its own param names/values, and a key this
 * build does not recognize (an older link, or one from a newer page)
 * simply passes through unused rather than being rejected.
 */
export function parsePageParams(query: URLSearchParams): Record<string, string> {
  const result: Record<string, string> = {}
  for (const [key, value] of query.entries()) {
    if (GLOBAL_PARAM_KEYS.has(key)) continue
    if (value === '') continue
    result[key] = value
  }
  return result
}
