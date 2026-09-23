import type { Ref } from 'vue'
import { computed, ref } from 'vue'

/**
 * useSearchQuery is the one raw-query → {normalized, hasQuery} adapter
 * every search filter in this panel uses — ProviderHealthPanel's
 * model/provider search, ConsumerDirectory's user/group search
 * (lib/usage-search.ts), EventsView's user/group filter, TargetsView's
 * target search, each with its own matching logic but the identical
 * query/normalizedQuery/hasQuery pairing; extracted so it exists once
 * (vue.md: "if you've written it twice, you owe an abstraction"). Callers
 * still own their own matching logic entirely — this only normalizes the
 * raw input the same way every call site already agreed on: trimmed,
 * lowercased.
 *
 * No `clear()` here — clearing is just setting `query.value = ''`, and
 * every caller already gets that for free from SearchInput.vue's own
 * clear button emitting `update:modelValue('')` through the `v-model`
 * binding, so a separate clear function would be dead API surface no
 * caller needs.
 *
 * `external` (F9, hash-state) lets a caller supply the ref this composable
 * reads and writes instead of owning a private local one, so that ref's
 * own search text round-trips through the URL hash. Two current callers
 * pass it: ConsumerDirectory.vue binds a local `computed` get/set over
 * `nav.params.q` directly (stores/nav.ts's own `params` is opaque, so the
 * page owns interpreting/writing its own `q` key — see that computed's
 * own doc comment for why this replaced an earlier, page-specific
 * `nav.usageQuery` special case), and EventsView.vue binds
 * `storeToRefs(events).userFilter`, which composables/useHashState.ts
 * reads/writes directly against the same hash. Every OTHER caller
 * (ProviderHealthPanel's model/provider search, TargetsView's target
 * search) omits it and keeps its private, hash-independent ref instead —
 * passing `external` changes nothing about `normalized`/`hasQuery`'s own
 * derivation, only where the raw value itself lives.
 */
export function useSearchQuery(external?: Ref<string>) {
  const query = external ?? ref('')
  const normalized = computed(() => query.value.trim().toLowerCase())
  const hasQuery = computed(() => normalized.value.length > 0)

  return { query, normalized, hasQuery }
}
