<script setup lang="ts">
import { computed, reactive } from 'vue'

import TargetCallers from '@/components/TargetCallers.vue'
import TargetsView from '@/components/TargetsView.vue'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { resolveTargetRef } from '@/lib/target-columns'
import { useDashboardStore } from '@/stores/dashboard'
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
 * one-param-at-a-time convention. The raw param is normalized through
 * lib/target-columns.ts's resolveTargetRef before it ever reaches
 * TargetCallers — the backend only accepts "mcp/{name}"/"agent/{name}"
 * (stats_read.go's parseTargetRef), so a hand-typed bare name like
 * `#targets?target=demo-mcp` used to 400 instead of resolving.
 */
const nav = useNavStore()
const dashboard = useDashboardStore()

const selectedTargetId = computed<string>(() => nav.params.target ?? '')

/**
 * targetResolution is selectedTargetId run through resolveTargetRef
 * against the live dashboard.targets listing — null when there is no
 * `target` param at all (resolveTargetRef has no "empty" case of its
 * own; an empty string would otherwise fall through its bare-name
 * lookup and misreport 'notice: No MCP server or agent named "" is
 * configured.', a spurious message on the plain, no-deep-link Targets
 * page — caught live via the states-plan.md visual verification pass).
 * Otherwise: 'ref' renders TargetCallers, 'notice' renders the Alert
 * below instead of calling the API, 'pending' (targets not loaded yet,
 * only possible for a bare-name param) renders neither until the next
 * dashboard poll resolves it.
 */
const targetResolution = computed(() => (selectedTargetId.value === '' ? null : resolveTargetRef(selectedTargetId.value, dashboard.targets)))

/**
 * knownLabels remembers the real display name TargetsView.vue's own click
 * handed selectTarget, keyed by composed id — selectedTargetLabel below
 * falls back to the resolved bare name (targetResolution's own `name`)
 * whenever the CURRENT selectedTargetId has no recorded label, e.g. a
 * fresh deep link that never went through a click (the label is cosmetic
 * only — TargetCallers.vue's own card title, not a lookup key — so this
 * never blocks on the targets listing having loaded).
 */
const knownLabels = reactive<Record<string, string>>({})
const selectedTargetLabel = computed<string>(() => {
  const resolution = targetResolution.value
  return knownLabels[selectedTargetId.value] ?? (resolution?.kind === 'ref' ? resolution.name : selectedTargetId.value)
})

function selectTarget(id: string, label: string): void {
  knownLabels[id] = label
  nav.goTo('targets', { ...nav.params, target: id })
}
</script>

<template>
  <div class="flex flex-col gap-6">
    <TargetsView @select="selectTarget" />

    <TargetCallers v-if="targetResolution?.kind === 'ref'" :target="targetResolution.ref" :title="`Callers of ${selectedTargetLabel}`" />
    <Alert v-else-if="targetResolution?.kind === 'notice'" variant="destructive">
      <AlertDescription>{{ targetResolution.message }}</AlertDescription>
    </Alert>

    <TargetCallers title="Top callers, across every target" />
  </div>
</template>
