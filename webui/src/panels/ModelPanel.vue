<script setup lang="ts">
import { Badge, Button, DescriptionItem, DescriptionList, Sparkline } from '@perfect-design/ui'
import { computed, onMounted, ref, watch } from 'vue'

import ModelMetaForm from '@/components/forms/ModelMetaForm.vue'
import PricingForm from '@/components/forms/PricingForm.vue'
import { adminFetch, isAuthRejection, messageOf } from '@/lib/api'
import { formatCompactCount, formatContextWindow, formatLatencyMs, formatModelCostHover } from '@/lib/format'
import { priceStatus } from '@/lib/models-view'
import { formatSel } from '@/lib/panel-selection'
import { providerRateStatus, providerSuccessRate } from '@/lib/provider-rate'
import { seriesUrl } from '@/lib/range'
import { routersByModel } from '@/lib/router-index'
import { useFiltersStore } from '@/stores/filters'
import { useModelsStore } from '@/stores/models'
import { useNavStore } from '@/stores/nav'
import type { AdminSeriesResponse } from '@/types/api'
import DetailPanel from '@/widgets/DetailPanel.vue'
import PanelSection from '@/widgets/PanelSection.vue'
import StateView from '@/widgets/StateView.vue'
import type { ViewState } from '@/widgets/StateView.vue'

const MIN_SPARK_POINTS = 2
const SPARK_WIDTH = 440
const SPARK_HEIGHT = 40
const PRICE_SOURCE_NOTE: Record<string, string> = {
  override: 'pricing override in config',
  builtin: 'built-in price table',
  litellm: 'LiteLLM price table',
  free: 'configured free (modelMeta.free)',
  unpriced: 'no pricing rule matched, billed as $0',
}
type Snippet = 'meta' | 'price' | null

const props = defineProps<{ id: string }>()
const emit = defineEmits<{ close: [] }>()

const filters = useFiltersStore()
const models = useModelsStore()
const nav = useNavStore()

const row = computed(() => models.rows.find((r) => r.id === props.id))
const routers = computed(() => {
  const r = row.value
  if (!r) return []
  const index = routersByModel(models.routers)
  return [...new Set([...(r.routers ?? []), ...(index.get(r.id) ?? [])])].sort()
})
const status = computed(() => (row.value ? priceStatus(row.value) : 'none'))
const priced = computed(() => row.value?.inputPerMTokUsd !== undefined && row.value?.outputPerMTokUsd !== undefined)
const successRate = computed(() => (row.value ? providerSuccessRate(row.value.attempts, row.value.failures) : null))

// --- Usage over the range ----------------------------------------------------------

const points = ref<number[]>([])
const seriesLoading = ref(true)
const seriesError = ref('')
let reqId = 0

async function loadSeries(): Promise<void> {
  const id = ++reqId
  seriesLoading.value = true
  try {
    const res = await adminFetch<AdminSeriesResponse>(seriesUrl({ scope: [`model:${props.id}`], metric: 'req', window: filters.window, span: filters.span }))
    if (id !== reqId) return
    points.value = res.series.find((s) => s.scope === `model:${props.id}`)?.points ?? []
    seriesError.value = ''
  } catch (err) {
    if (id !== reqId || isAuthRejection(err)) return
    points.value = []
    seriesError.value = messageOf(err)
  } finally {
    if (id === reqId) seriesLoading.value = false
  }
}
const total = computed(() => points.value.reduce((a, v) => a + v, 0))
const sparkState = computed<ViewState>(() => {
  if (seriesLoading.value && points.value.length === 0) return 'loading'
  if (seriesError.value) return 'error'
  return points.value.length >= MIN_SPARK_POINTS && total.value > 0 ? 'ready' : 'empty'
})

watch(() => [props.id, filters.range], () => void loadSeries())
onMounted(() => void loadSeries())

// --- Snippets ------------------------------------------------------------------------

const snippet = ref<Snippet>(null)
watch(() => props.id, () => (snippet.value = null))

const usd = (v: number): string => `$${v.toFixed(2)}`

function openRouter(name: string): void {
  nav.goTo('routers', { sel: formatSel('router', name) }, { replace: false })
}
</script>

<template>
  <DetailPanel kind="model" :id="id" :title="id" @close="emit('close')">
    <div class="flex flex-col divide-y px-1 pb-4" data-testid="model-panel">
      <div v-if="!row" class="text-muted-foreground py-3 text-sm">This model is no longer in the catalog.</div>
      <template v-else>
        <PanelSection title="Meta">
          <DescriptionList variant="inline" size="sm" density="compact">
            <DescriptionItem term="Provider">
              <button type="button" class="hover:underline font-mono" @click="nav.openPanel('provider', row.providerName)">{{ row.providerName }}</button>
            </DescriptionItem>
            <DescriptionItem term="Model"><span class="font-mono break-all">{{ row.model }}</span></DescriptionItem>
            <DescriptionItem term="Kind">{{ row.kind || '—' }}</DescriptionItem>
            <DescriptionItem v-if="row.endpoint" term="Type">endpoint path, not a routable model</DescriptionItem>
            <DescriptionItem term="Context window">
              <span class="font-mono tabular-nums">{{ row.contextTokens !== undefined ? formatContextWindow(row.contextTokens) : '—' }}</span>
            </DescriptionItem>
            <DescriptionItem term="Aliases">
              <span v-if="row.aliases.length === 0" class="text-muted-foreground">none</span>
              <span v-else class="flex flex-wrap justify-end gap-1">
                <Badge v-for="a in row.aliases" :key="a" tone="neutral" variant="outline" size="sm" class="font-mono">{{ a }}</Badge>
              </span>
            </DescriptionItem>
          </DescriptionList>
        </PanelSection>

        <PanelSection title="Price">
          <DescriptionList variant="inline" size="sm" density="compact">
            <DescriptionItem term="Source">
              <span class="flex flex-col items-end text-right">
                <Badge :tone="status === 'unpriced' ? 'warning' : 'neutral'" variant="outline" size="sm">{{ row.priceSource }}</Badge>
                <span class="text-muted-foreground text-xs">{{ PRICE_SOURCE_NOTE[row.priceSource] }}</span>
              </span>
            </DescriptionItem>
            <DescriptionItem term="Input per MTok">
              <span class="font-mono tabular-nums">{{ priced ? usd(row.inputPerMTokUsd as number) : '—' }}</span>
            </DescriptionItem>
            <DescriptionItem term="Output per MTok">
              <span class="font-mono tabular-nums">{{ priced ? usd(row.outputPerMTokUsd as number) : '—' }}</span>
            </DescriptionItem>
          </DescriptionList>
          <p v-if="priced" class="text-muted-foreground m-0 text-xs">{{ formatModelCostHover(row.inputPerMTokUsd as number, row.outputPerMTokUsd as number) }}</p>
        </PanelSection>

        <PanelSection title="Usage in range">
          <DescriptionList variant="inline" size="sm" density="compact">
            <DescriptionItem term="Requests"><span class="font-mono tabular-nums">{{ formatCompactCount(row.requests) }}</span></DescriptionItem>
            <DescriptionItem term="Tokens in / out">
              <span class="font-mono tabular-nums">{{ formatCompactCount(row.tokensIn) }} / {{ formatCompactCount(row.tokensOut) }}</span>
            </DescriptionItem>
            <DescriptionItem term="Latency p50 / p95">
              <span class="font-mono tabular-nums">{{ row.p50Ms !== undefined ? formatLatencyMs(row.p50Ms) : '—' }} / {{ row.p95Ms !== undefined ? formatLatencyMs(row.p95Ms) : '—' }}</span>
            </DescriptionItem>
            <DescriptionItem term="Success rate">
              <span class="font-mono tabular-nums">{{ successRate === null ? '—' : `${Math.round(successRate * 1000) / 10}% (${providerRateStatus(successRate)})` }}</span>
            </DescriptionItem>
          </DescriptionList>
          <StateView :state="sparkState" empty-title="No traffic in this range" :error-message="seriesError" :rows="1" @retry="loadSeries()">
            <Sparkline :data="points" variant="area" :width="SPARK_WIDTH" :height="SPARK_HEIGHT" class="h-10 w-full" label="Requests per bucket over the selected range" />
          </StateView>
        </PanelSection>

        <PanelSection title="Routers">
          <p v-if="routers.length === 0" class="text-muted-foreground m-0 text-sm">No smart router targets this model.</p>
          <ul v-else class="m-0 flex list-none flex-col p-0" aria-label="Routers using this model">
            <li v-for="r in routers" :key="r" class="flex h-8 items-center">
              <button type="button" class="hover:bg-accent w-full truncate rounded px-2 text-left font-mono text-sm" @click="openRouter(r)">{{ r }}</button>
            </li>
          </ul>
        </PanelSection>

        <PanelSection title="Change model settings">
          <div class="flex flex-wrap gap-2">
            <Button variant="outline" size="sm" :aria-expanded="snippet === 'meta'" @click="snippet = snippet === 'meta' ? null : 'meta'">
              {{ snippet === 'meta' ? 'Hide metadata snippet' : 'Generate metadata snippet' }}
            </Button>
            <Button variant="outline" size="sm" :aria-expanded="snippet === 'price'" @click="snippet = snippet === 'price' ? null : 'price'">
              {{ snippet === 'price' ? 'Hide price snippet' : 'Generate price override snippet' }}
            </Button>
          </div>
          <ModelMetaForm v-if="snippet === 'meta'" :preset-model-id="id" />
          <PricingForm v-else-if="snippet === 'price'" :preset-model-id="id" />
        </PanelSection>
      </template>
    </div>
  </DetailPanel>
</template>
