import { defineStore } from 'pinia'

import { comparison, DEFAULT_CMP, DEFAULT_RANGE, RANGE_PRESETS } from '@/lib/range'
import type { CmpMode, ComparisonResult, RangeKey } from '@/lib/range'
import type { HistoryWindow } from '@/types/api'

/** DEFAULT_SCOPE is the global filter bar's "no scope narrowing" value (redesign-plan.md section 3.1: `scope=all|group:x|user:x|provider:x`, default `all`). */
export const DEFAULT_SCOPE = 'all'

/**
 * useFiltersStore holds the three GLOBAL filters every page reads from
 * the address bar (redesign-plan.md section 3.1/3.3): the time range,
 * whether to compare against the previous equal-length period, and an
 * optional scope narrowing (a single group/user/provider, or 'all').
 * Kept as one small store, not folded into stores/nav.ts, because these
 * three are page-INDEPENDENT — every page's own store (spend, consumers,
 * models, reliability) reads them the same way, unlike nav.ts's `params`,
 * which are page-specific and opaque to this store.
 */
export const useFiltersStore = defineStore('filters', {
  state: () => ({
    range: DEFAULT_RANGE as RangeKey,
    cmp: DEFAULT_CMP as CmpMode,
    scope: DEFAULT_SCOPE,
  }),
  getters: {
    /** window is the current range's bucket resolution (lib/range.ts's RANGE_PRESETS) — every page-store fetch reads this instead of re-deriving it from `range` itself. */
    window: (state): HistoryWindow => RANGE_PRESETS[state.range].window,
    /** span is the current range's bucket count. */
    span: (state): number => RANGE_PRESETS[state.range].span,
    /**
     * cmpSpec folds `cmp` and lib/range.ts's comparison(range) into one
     * read: `requested` is true only when the reader asked for a
     * comparison (`cmp === 'prev'`) AND the current range actually
     * supports one (comparison().available) — a page-store's fetch
     * checks `requested` alone rather than re-deriving that AND itself
     * every time, and a stale `cmp: 'prev'` left over from a range that
     * DID support it survives a switch to one that does not without
     * silently firing an invalid request.
     */
    cmpSpec: (state): ComparisonResult & { requested: boolean } => {
      const result = comparison(state.range)
      return { ...result, requested: state.cmp === 'prev' && result.available }
    },
  },
  actions: {
    /** setFilters applies any subset of range/cmp/scope in one call — omitted fields keep their current value, the same "apply a partial update, one atomic write" convention the pre-redesign stores/history.ts's own setSelection (deleted; this store replaces it) used, and stores/spend.ts's own setSelection still follows today. */
    setFilters(next: { range?: RangeKey; cmp?: CmpMode; scope?: string }): void {
      if (next.range !== undefined) this.range = next.range
      if (next.cmp !== undefined) this.cmp = next.cmp
      if (next.scope !== undefined) this.scope = next.scope
    },
  },
})
