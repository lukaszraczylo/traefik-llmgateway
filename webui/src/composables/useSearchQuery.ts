import { computed, ref } from 'vue'

/**
 * useSearchQuery is the one raw-query → {normalized, hasQuery} adapter
 * every search filter in this panel uses — ProvidersView's model search,
 * UsageView's user/group search, ChartsView's scope-picker search all had
 * their own copy of this identical query/normalizedQuery/hasQuery
 * pairing; extracted so it exists once (vue.md: "if you've written it
 * twice, you owe an abstraction"). Callers still own their own matching
 * logic entirely (ProvidersView's modelMatches/providerMatches,
 * lib/usage-search.ts's helpers) — this only normalizes the raw input the
 * same way every call site already agreed on: trimmed, lowercased.
 *
 * No `clear()` here — clearing is just setting `query.value = ''`, and
 * every caller already gets that for free from SearchInput.vue's own
 * clear button emitting `update:modelValue('')` through the `v-model`
 * binding, so a separate clear function would be dead API surface no
 * caller needs.
 */
export function useSearchQuery() {
  const query = ref('')
  const normalized = computed(() => query.value.trim().toLowerCase())
  const hasQuery = computed(() => normalized.value.length > 0)

  return { query, normalized, hasQuery }
}
