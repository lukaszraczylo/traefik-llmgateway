import { computed, ref } from 'vue'

/**
 * useSearchQuery is the one raw-query → {normalized, hasQuery, clear}
 * adapter every search filter in this panel uses — OverviewView's model
 * search, UsageView's user/group search, ChartsView's scope-picker search
 * all had their own copy of this identical query/normalizedQuery/hasQuery/
 * clearQuery quartet; extracted so it exists once (vue.md: "if you've
 * written it twice, you owe an abstraction"). Callers still own their own
 * matching logic entirely (OverviewView's modelMatches/providerMatches,
 * lib/usage-search.ts's helpers) — this only normalizes the raw input the
 * same way every call site already agreed on: trimmed, lowercased.
 */
export function useSearchQuery() {
  const query = ref('')
  const normalized = computed(() => query.value.trim().toLowerCase())
  const hasQuery = computed(() => normalized.value.length > 0)

  function clear(): void {
    query.value = ''
  }

  return { query, normalized, hasQuery, clear }
}
