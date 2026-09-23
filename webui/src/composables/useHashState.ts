import { computed, onMounted, onUnmounted, watch } from 'vue'

import {
  buildHash,
  parseChartsParams,
  parseEventsParams,
  parseHashState,
  parseUsageParams,
} from '@/lib/hash-state'
import { useEventsStore } from '@/stores/events'
import { useHistoryStore } from '@/stores/history'
import { useNavStore } from '@/stores/nav'
import type { ChartTab, ModelMetric } from '@/stores/history'
import type { HistoryWindow } from '@/types/api'

/** The five defaults every per-tab param omits from the built hash when the field is already at its default value (buildHash's own "caller decides what's default" contract, lib/hash-state.ts). */
const DEFAULT_WINDOW: HistoryWindow = 'hour'
const DEFAULT_CHART_TAB: ChartTab = 'requests'
/**
 * DEFAULT_METRIC mirrors stores/history.ts's own initial `modelMetric`
 * state (free-models-plan.md: 'req', not 'cost' — a cost-default ranking
 * hides every free model). An omitted `metric` hash param, or one this
 * build does not recognize (parseChartsParams' own independent-field
 * validation, lib/hash-state.ts), falls back to this value here; an
 * explicit `metric=cost` in the hash still parses and applies normally.
 */
const DEFAULT_METRIC: ModelMetric = 'req'
const DEFAULT_SCOPE = 'total'
const DEFAULT_MODEL_FILTER = ''

/**
 * useHashState (F9, dashboard-plan.md) is the single composable that keeps
 * the URL hash and every store it names in sync, both ways:
 *
 * - On creation, it parses the CURRENT hash exactly once and applies it to
 *   stores/nav.ts (activeTab, usageQuery), stores/history.ts (Charts'
 *   scope/window/tab/modelMetric/modelFilter), and stores/events.ts (kindFilter/
 *   userFilter) — "on load: read hash -> restore state" for all three, not
 *   just the active tab the old useTabHash() composable covered.
 * - A single computed re-derives the hash from whichever store the
 *   CURRENT nav.activeTab reads from, and writes it back via
 *   history.replaceState whenever it changes — never pushState (one
 *   browser-history entry per admin session, not per click) and never a
 *   bare `location.hash =` assignment (which would also jump scroll
 *   position to any element whose id happens to match).
 * - It listens for the browser's own `hashchange` event (back/forward
 *   navigation, or a hand-edited URL) and re-parses from scratch,
 *   reapplying the same restore path as the initial read.
 *
 * MUST be called from App.vue's setup, before the `v-if="!auth.
 * isAuthenticated"` branch (dashboard-plan.md's own Risks section:
 * "load-with-hash-then-authenticate") — every store action this touches
 * (history.setScope, etc.) is safe to call before a key is stored (each
 * one's own fetch no-ops on an unauthenticated auth store), so the hash's
 * selection is captured immediately and is already in place the moment
 * the reader authenticates, rather than being lost because this composable
 * was never constructed while the AuthGate was showing.
 *
 * Guarded for a `window`-less environment (vitest's node test
 * environment, vite.config.ts) the same way useTabHash's old
 * implementation was: every DOM/history/location access sits behind one
 * hasWindow check computed once. Being a real Vue lifecycle-hook user
 * (onMounted/onUnmounted) exercised through App.vue in the real browser,
 * not unit-tested directly — lib/hash-state.ts's own pure functions are
 * what hash-state.spec.ts covers.
 */
export function useHashState(): void {
  const nav = useNavStore()
  const history = useHistoryStore()
  const events = useEventsStore()

  function applyFromHash(hash: string): void {
    const { tab, query } = parseHashState(hash)
    nav.activeTab = tab
    if (tab === 'charts') {
      // P7 review fix: this used to apply ONLY the params present in the
      // hash, so a PARTIAL Charts hash on hashchange (back/forward
      // navigation, or the reader hand-editing the URL down to a bare
      // "#charts") left every OMITTED field exactly as it was — buildHash
      // itself treats an omitted param as "use the default" (its own doc
      // comment), but this restore path never actually reset anything to
      // that default, so the address bar and the rendered chart silently
      // disagreed. Every field is now applied on EVERY call — explicitly
      // falling back to its own default when the hash omits it — so a
      // partial or bare hash genuinely resets whatever it doesn't name —
      // including `filter` (P7 review follow-up): the Models tab's
      // provider-prefix filter is a hash param now (lib/hash-state.ts's
      // ChartsHashParams.filter), so it falls back to
      // DEFAULT_MODEL_FILTER exactly like every other field here, rather
      // than being left over from whatever the store already had.
      //
      // P12 review fix: applied through ONE history.setSelection() call
      // (batches the state writes and fires exactly one refresh(), plus
      // one fetchModelOptions() only when window is part of the change)
      // instead of up to four separate setWindow/setTab/setModelMetric/
      // setScope calls, each of which used to fire its OWN refresh() —
      // every fetch but the last immediately discarded.
      const p = parseChartsParams(query)
      history.setSelection({
        window: p.window ?? DEFAULT_WINDOW,
        tab: p.tab ?? DEFAULT_CHART_TAB,
        modelMetric: p.metric ?? DEFAULT_METRIC,
        scope: p.scope ?? DEFAULT_SCOPE,
        modelFilter: p.filter ?? DEFAULT_MODEL_FILTER,
      })
    } else if (tab === 'usage') {
      const p = parseUsageParams(query)
      nav.usageQuery = p.q ?? ''
    } else if (tab === 'events') {
      const p = parseEventsParams(query)
      events.setKindFilter(p.kind ?? '')
      events.setUserFilter(p.user ?? '')
    }
  }

  const hasWindow = typeof window !== 'undefined'
  if (hasWindow) applyFromHash(window.location.hash)

  const currentHash = computed<string>(() => {
    if (nav.activeTab === 'charts') {
      const params: Record<string, string> = {}
      if (history.window !== DEFAULT_WINDOW) params.window = history.window
      if (history.tab !== DEFAULT_CHART_TAB) params.tab = history.tab
      if (history.modelMetric !== DEFAULT_METRIC) params.metric = history.modelMetric
      if (history.scope !== DEFAULT_SCOPE) params.scope = history.scope
      if (history.modelFilter !== DEFAULT_MODEL_FILTER) params.filter = history.modelFilter
      return buildHash(nav.activeTab, params)
    }
    if (nav.activeTab === 'usage') {
      return buildHash(nav.activeTab, nav.usageQuery ? { q: nav.usageQuery } : {})
    }
    if (nav.activeTab === 'events') {
      const params: Record<string, string> = {}
      if (events.kindFilter) params.kind = events.kindFilter
      if (events.userFilter) params.user = events.userFilter
      return buildHash(nav.activeTab, params)
    }
    return buildHash(nav.activeTab, {})
  })

  watch(currentHash, (hash) => {
    if (!hasWindow) return
    if (window.location.hash === hash) return
    window.history.replaceState(null, '', hash)
  })

  function onHashChange(): void {
    applyFromHash(window.location.hash)
  }

  if (hasWindow) {
    onMounted(() => window.addEventListener('hashchange', onHashChange))
    onUnmounted(() => window.removeEventListener('hashchange', onHashChange))
  }
}
