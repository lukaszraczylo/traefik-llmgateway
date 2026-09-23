import { defineStore } from 'pinia'

import type { TabValue } from '@/composables/useTabHash'
import { useHistoryStore } from '@/stores/history'

/**
 * useNavStore (F6) is the one place cross-tab "jump to Charts, pointed at
 * X" navigation lives: every ScopeLink.vue click (usage-columns.ts's id
 * cell, the Groups accordion trigger here, and WP-B2's ProvidersView.vue
 * per-model links) and the Providers tab's own per-provider header link
 * call one of the two actions below, instead of each call site poking
 * history.ts's scope/tab state AND App.vue's own active-tab separately.
 *
 * `activeTab` is typed with TabValue, the SAME union App.vue's Tabs
 * v-model already binds to (composables/useTabHash.ts) — WP-B2 extends
 * that union with 'events' (F3); this store picks the extension up for
 * free with no edit here, since it imports the type rather than
 * redeclaring its own copy (vue.md: "share types once, import
 * everywhere").
 */
export const useNavStore = defineStore('nav', {
  state: () => ({
    activeTab: 'providers' as TabValue,
    /**
     * usageQuery is the Usage tab's own search box value (F9's
     * useSearchQuery(external), WP-B2 — composables/useSearchQuery.ts
     * gains an optional external ref so UsageView.vue can bind this
     * instead of owning a local ref), lifted up here rather than kept
     * local to that component so it survives a navigation away from and
     * back to the Usage tab, and so #<tab>?q=... hash state (F9,
     * lib/hash-state.ts) has one canonical place to read/write it from.
     */
    usageQuery: '',
  }),
  actions: {
    /**
     * goToCharts switches to the Charts tab with `scope` selected —
     * "total" | "user:{id}" | "group:{id}" | "model:{id}" (history.ts's
     * own scope string convention, unchanged here). If the Charts view is
     * currently showing the Models ranking — which has no scope picker at
     * all (ChartsView.vue's own template branch) — it is switched back to
     * the Requests tab FIRST, so the newly-set scope is immediately
     * visible rather than silently inert behind the ranking view.
     *
     * P12 review fix: both fields are applied through ONE
     * history.setSelection() call rather than two separate setTab()/
     * setScope() calls — each of THOSE independently fires its own
     * refresh(), so switching off the Models tab AND changing scope in the
     * same click used to fire two fetches back to back, the first always
     * discarded (seriesReqId's own latest-request-wins guard) the instant
     * the second landed.
     */
    goToCharts(scope: string): void {
      const history = useHistoryStore()
      history.setSelection(history.tab === 'models' ? { tab: 'requests', scope } : { scope })
      this.activeTab = 'charts'
    },
    /**
     * goToModels switches to the Charts tab's Models ranking, pre-filtered
     * to `prefix` (ProvidersView.vue's per-provider header link passes
     * `${provider.name}/`) — history.ts's own modelFilter/setModelFilter
     * (WP-B2, F6 charts+providers side).
     *
     * P12 review fix: `tab` and `modelFilter` are applied through ONE
     * history.setSelection() call — setSelection's own field-ordering
     * (tab's own modelFilter reset happens BEFORE this call's `modelFilter`
     * field is applied, see its doc comment) preserves the exact ordering
     * this used to need two separate calls for (setTab('models') resetting
     * modelFilter, THEN setModelFilter(prefix) applying the real value),
     * while firing exactly one ranking fetch instead of one per call.
     */
    goToModels(prefix: string): void {
      const history = useHistoryStore()
      history.setSelection({ tab: 'models', modelFilter: prefix })
      this.activeTab = 'charts'
    },
  },
})
