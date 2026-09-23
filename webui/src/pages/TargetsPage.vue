<script setup lang="ts">
import { computed, reactive } from 'vue'

import TargetCallers from '@/components/TargetCallers.vue'
import TargetsView from '@/components/TargetsView.vue'
import { useNavStore } from '@/stores/nav'

/**
 * TargetsPage (redesign-plan.md section 3.4, "MCP & Agents") is
 * TargetsView.vue's pre-redesign listing (unchanged: MCP servers/agents,
 * their access lists, and request counters), plus the per-target caller
 * breakdown ("TargetCallers.vue expander") and a fleet-wide "top callers"
 * card, both reading GET /admin/api/usage/totals?kind=targetcaller.
 *
 * `target` (hash param) deep-links directly to one target's own callers —
 * read as a computed straight off nav.params (P2 item 10: a local ref set
 * only once on mount ignored a LATER hashchange — browser back/forward, a
 * hand-edited URL — while the page stayed mounted) and kept in sync
 * afterwards via nav.goTo so a reader who clicks a different target gets a
 * bookmarkable/shareable URL, mirroring EntityLink.vue's own
 * one-param-at-a-time convention.
 */
const nav = useNavStore()

const selectedTargetId = computed<string>(() => nav.params.target ?? '')

/**
 * knownLabels remembers the real display name TargetsView.vue's own click
 * handed selectTarget, keyed by composed id — selectedTargetLabel below
 * falls back to the raw id (TargetCallers.vue's own card title) whenever
 * the CURRENT selectedTargetId has no recorded label, e.g. a fresh deep
 * link that never went through a click (the label is cosmetic only —
 * TargetCallers.vue's own card title, not a lookup key — so this never
 * blocks on the targets listing having loaded).
 */
const knownLabels = reactive<Record<string, string>>({})
const selectedTargetLabel = computed<string>(() => knownLabels[selectedTargetId.value] ?? selectedTargetId.value)

function selectTarget(id: string, label: string): void {
  knownLabels[id] = label
  nav.goTo('targets', { ...nav.params, target: id })
}
</script>

<template>
  <div class="flex flex-col gap-6">
    <TargetsView @select="selectTarget" />

    <TargetCallers v-if="selectedTargetId" :target="selectedTargetId" :title="`Callers of ${selectedTargetLabel}`" />

    <TargetCallers title="Top callers, across every target" />
  </div>
</template>
