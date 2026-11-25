<script setup lang="ts">
import { faChevronDown, faChevronRight } from '@fortawesome/free-solid-svg-icons'
import { Button, Callout, Card, CardAction, CardContent, CardHeader, CardTitle, SearchInput, YamlViewer } from '@perfect-design/ui'
import { computed, nextTick, onMounted, ref, watch } from 'vue'

import { copyText } from '@/lib/clipboard'
import { filterSections, initialFold, splitConfigSections } from '@/lib/config-sections'
import { formatHelperSel, isStaleHelperSel, parseHelperSel } from '@/lib/helper-forms'
import type { HelperId } from '@/lib/helper-forms'
import { loadState } from '@/lib/load-state'
import { emitYamlDocument, toYamlValue } from '@/lib/yaml-emit'
import type { YamlValue } from '@/lib/yaml-emit'
import ChangeHelperPanel from '@/panels/ChangeHelperPanel.vue'
import { useConfigStore } from '@/stores/config'
import { useConsumersStore } from '@/stores/consumers'
import { useDashboardStore } from '@/stores/dashboard'
import { useNavStore } from '@/stores/nav'
import { useToastsStore } from '@/stores/toasts'
import StateView from '@/widgets/StateView.vue'
import type { ViewState } from '@/widgets/StateView.vue'

const config = useConfigStore()
const consumers = useConsumersStore()
const dashboard = useDashboardStore()
const nav = useNavStore()
const toasts = useToastsStore()

onMounted(() => {
  void config.ensureConfig()
  void consumers.ensureConsumers()
})

// The server redacts secrets before this response; the page renders it as received and never rewrites values.
const yamlText = computed(() => (config.data ? emitYamlDocument(toYamlValue(config.data.config) as Record<string, YamlValue | undefined>) : ''))
const sections = computed(() => splitConfigSections(yamlText.value))
const query = ref('')
const shown = computed(() => filterSections(sections.value, query.value))
const collapsed = ref<Set<string>>(new Set())
const target = computed(() => nav.params.section)

// Default fold: big sections closed, small ones, filter matches and the hash target open.
function resetFold(): void {
  collapsed.value = initialFold(sections.value, { filter: query.value, target: target.value })
}
watch([sections, query], resetFold, { immediate: true })

function toggle(key: string): void {
  const next = new Set(collapsed.value)
  if (!next.delete(key)) next.add(key)
  collapsed.value = next
}

function setAll(open: boolean): void {
  collapsed.value = open ? new Set() : new Set(sections.value.map((s) => s.key))
}

async function jump(key: string): Promise<void> {
  const next = new Set(collapsed.value)
  next.delete(key)
  query.value = ''
  await nextTick()
  collapsed.value = next
  await nextTick()
  document.getElementById(`cfg-${key}`)?.scrollIntoView({ block: 'start' })
}

const state = computed<ViewState>(() => {
  const s = loadState({ loading: config.loading, hasData: config.data !== null, error: config.error })
  if (s === 'skeleton') return 'loading'
  if (s === 'error') return 'error'
  return sections.value.length === 0 ? 'empty' : 'ready'
})

const warnings = computed(() => config.data?.warnings ?? [])
const warningsDropped = computed(() => dashboard.overview?.warningsDropped ?? 0)

async function copyConfig(): Promise<void> {
  const result = await copyText(yamlText.value)
  toasts.push(result === 'copied' ? { kind: 'success', message: 'Config copied.' } : { kind: 'error', message: 'Could not copy the config.' })
}

// --- Change helper sheet: lives in the hash as sel=helper:<form> ---
const helperForm = computed(() => parseHelperSel(nav.params.sel))
// This page has no entity panels, so any other `sel` is stale: drop it in place, like resolveSel does elsewhere.
watch(
  () => nav.params.sel,
  (sel) => {
    if (!isStaleHelperSel(sel)) return
    const { sel: _sel, ...rest } = nav.params
    nav.goTo('config', rest)
  },
  { immediate: true },
)
const groupNames = computed(() => (consumers.data?.groups ?? []).map((g) => g.name))
const userNames = computed(() => (consumers.data?.users ?? []).map((u) => u.name))

function openHelper(): void {
  nav.openSheet(formatHelperSel(helperForm.value ?? 'limits'))
}
function selectForm(id: HelperId): void {
  nav.swapSheet(formatHelperSel(id))
}
</script>

<template>
  <div class="flex flex-col gap-4">
    <Card size="sm">
      <CardHeader class="items-center">
        <CardTitle>Config</CardTitle>
        <CardAction class="flex items-center gap-2 self-center">
          <span v-if="config.data?.version" class="text-muted-foreground font-mono text-xs">{{ config.data.version }}</span>
          <Button variant="outline" size="sm" :disabled="!yamlText" @click="copyConfig">Copy config</Button>
          <Button size="sm" @click="openHelper">Change helper</Button>
        </CardAction>
      </CardHeader>
      <CardContent class="flex flex-col gap-3">
        <div v-if="warnings.length > 0" class="flex flex-col gap-2" data-testid="config-warnings">
          <Callout v-for="(w, i) in warnings" :key="i" type="warning" size="sm" title="Config warning">
            <span class="font-mono text-xs break-words">{{ w }}</span>
          </Callout>
          <p v-if="warningsDropped > 0" class="text-muted-foreground m-0 text-xs">{{ warningsDropped.toLocaleString('en-US') }} more warnings were dropped by the server cap.</p>
        </div>

        <StateView :state="state" empty-title="No config returned" :error-message="config.error" @retry="config.fetchConfig()">
          <div class="flex flex-col gap-2">
            <SearchInput v-model="query" size="sm" class="max-w-sm" placeholder="Filter sections" aria-label="Filter config sections" />
            <div class="flex flex-wrap items-center gap-1">
              <Button variant="outline" size="xs" @click="setAll(true)">Expand all</Button>
              <Button variant="outline" size="xs" @click="setAll(false)">Collapse all</Button>
            </div>
            <nav class="flex flex-wrap gap-1" aria-label="Config sections">
              <Button v-for="s in sections" :key="s.key" variant="ghost" size="xs" class="font-mono" @click="jump(s.key)">{{ s.key }}</Button>
            </nav>
            <p v-if="shown.length === 0" class="text-muted-foreground m-0 py-3 text-sm">No section matches "{{ query }}".</p>
            <section v-for="s in shown" :id="`cfg-${s.key}`" :key="s.key" class="border-border flex flex-col border-t first:border-t-0" :aria-label="s.key">
              <button
                type="button"
                class="hover:bg-accent focus-visible:ring-ring flex h-8 w-full items-center gap-2 rounded px-2 text-left focus-visible:ring-2 focus-visible:outline-none"
                :aria-expanded="!collapsed.has(s.key)"
                @click="toggle(s.key)"
              >
                <FontAwesomeIcon :icon="collapsed.has(s.key) ? faChevronRight : faChevronDown" class="text-muted-foreground size-3" aria-hidden="true" />
                <span class="font-mono text-sm font-medium">{{ s.key }}</span>
                <span class="text-muted-foreground ml-auto font-mono text-xs tabular-nums">{{ s.lineCount.toLocaleString('en-US') }} {{ s.lineCount === 1 ? 'line' : 'lines' }}</span>
              </button>
              <YamlViewer v-if="!collapsed.has(s.key)" :source="s.text" :start-line="s.startLine" variant="plain" size="sm" line-numbers :foldable="false" :copy="false" :label="`${s.key} config`" />
            </section>
          </div>
        </StateView>
      </CardContent>
    </Card>

    <ChangeHelperPanel v-if="helperForm" :form="helperForm" :group-names="groupNames" :user-names="userNames" @select="selectForm" @close="nav.closePanel()" />
  </div>
</template>
