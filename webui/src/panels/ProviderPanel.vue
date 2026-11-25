<script setup lang="ts">
import { Button, DescriptionItem, DescriptionList, StatusBadge } from '@perfect-design/ui'
import { computed, ref } from 'vue'

import { copyText } from '@/lib/clipboard'
import { formatAgo, formatCompactCount, formatLatencyMs, formatUntil } from '@/lib/format'
import { breakerRows, providerRateStatus, providerSuccessRate } from '@/lib/provider-rate'
import { useCopyFeedback } from '@/composables/useCopyFeedback'
import { useDashboardStore } from '@/stores/dashboard'
import { useModelsStore } from '@/stores/models'
import { useNavStore } from '@/stores/nav'
import DetailPanel from '@/widgets/DetailPanel.vue'
import PanelSection from '@/widgets/PanelSection.vue'

const MODEL_ROWS = 12
const STATE_TONE = { closed: 'success', open: 'danger', 'half-open': 'warning', degraded: 'warning' } as const

const props = defineProps<{ id: string }>()
const emit = defineEmits<{ close: [] }>()

const dashboard = useDashboardStore()
const models = useModelsStore()
const nav = useNavStore()
const copy = useCopyFeedback()
const urlEl = ref<HTMLElement | null>(null)

const provider = computed(() => dashboard.overview?.providers.find((p) => p.name === props.id))
const breaker = computed(() => breakerRows(dashboard.overview).find((b) => b.provider === props.id))
const status = computed(() => (breaker.value?.degraded ? 'degraded' : (breaker.value?.state ?? 'closed')))
const until = computed(() => formatUntil(breaker.value?.openUntil))

const lastDiscovery = computed(() => formatAgo(provider.value?.lastRefresh).trim().slice(1, -1) || '—')
const perf = computed(() => models.providerPerf.find((r) => r.id === props.id))
const successRate = computed(() => providerSuccessRate(provider.value?.attemptsDay ?? 0, provider.value?.failuresDay ?? 0))

const providerModels = computed(() =>
  models.rows.filter((r) => r.providerName === props.id).sort((a, b) => b.requests - a.requests || a.id.localeCompare(b.id)),
)
const shownModels = computed(() => providerModels.value.slice(0, MODEL_ROWS))
const hiddenModels = computed(() => providerModels.value.length - shownModels.value.length)

async function copyUrl(): Promise<void> {
  copy.set(await copyText(provider.value?.baseUrl ?? '', urlEl.value ?? undefined))
}
const COPY_LABEL = { idle: 'Copy', copied: 'Copied', selected: 'Selected', failed: 'Copy failed' } as const

function showAll(): void {
  nav.goTo('models', { provider: props.id })
}
</script>

<template>
  <DetailPanel kind="provider" :id="id" :title="id" @close="emit('close')">
    <div class="flex flex-col divide-y px-1 pb-4" data-testid="provider-panel">
      <div v-if="!provider" class="text-muted-foreground py-3 text-sm">This provider is not configured.</div>
      <template v-else>
        <PanelSection title="Health">
          <DescriptionList variant="inline" size="sm" density="compact">
            <DescriptionItem term="Breaker">
              <StatusBadge :status="status" :tones="STATE_TONE" size="sm" />
            </DescriptionItem>
            <DescriptionItem v-if="until" term="Next probe"><span class="font-mono tabular-nums">{{ until }}</span></DescriptionItem>
            <DescriptionItem term="Success today">
              <span class="font-mono tabular-nums">
                {{ successRate === null ? 'no traffic' : `${Math.round(successRate * 1000) / 10}% (${providerRateStatus(successRate)})` }}
              </span>
            </DescriptionItem>
            <DescriptionItem term="Attempts / failures">
              <span class="font-mono tabular-nums">{{ formatCompactCount(provider.attemptsDay) }} / {{ formatCompactCount(provider.failuresDay) }}</span>
            </DescriptionItem>
            <DescriptionItem term="Last discovery">{{ provider.discoveryEnabled ? lastDiscovery : 'discovery off' }}</DescriptionItem>
          </DescriptionList>
        </PanelSection>

        <PanelSection title="Last error">
          <p v-if="provider.lastErr" class="text-destructive m-0 text-sm break-words whitespace-pre-wrap" data-testid="provider-last-err">{{ provider.lastErr }}</p>
          <p v-else class="text-muted-foreground m-0 text-sm">No error recorded.</p>
        </PanelSection>

        <PanelSection title="Latency (time to first byte)">
          <DescriptionList v-if="perf?.ttfbP50Ms !== undefined || perf?.ttfbP95Ms !== undefined" variant="inline" size="sm" density="compact">
            <DescriptionItem term="p50"><span class="font-mono tabular-nums">{{ perf?.ttfbP50Ms !== undefined ? formatLatencyMs(perf.ttfbP50Ms) : '—' }}</span></DescriptionItem>
            <DescriptionItem term="p95"><span class="font-mono tabular-nums">{{ perf?.ttfbP95Ms !== undefined ? formatLatencyMs(perf.ttfbP95Ms) : '—' }}</span></DescriptionItem>
          </DescriptionList>
          <p v-else-if="!models.latencyEnabled" class="text-muted-foreground m-0 text-sm">Latency stats are off. Enable admin.stats.latency to record them.</p>
          <p v-else class="text-muted-foreground m-0 text-sm">No latency samples for this provider in the range.</p>
        </PanelSection>

        <PanelSection title="Models">
          <p v-if="providerModels.length === 0" class="text-muted-foreground m-0 text-sm">No models listed for this provider.</p>
          <template v-else>
            <ul class="m-0 flex list-none flex-col p-0" aria-label="Provider models">
              <li v-for="m in shownModels" :key="m.id">
                <button type="button" class="hover:bg-accent flex h-8 w-full items-center justify-between gap-3 rounded px-2 text-left text-sm" :title="m.id" @click="nav.openPanel('model', m.id)">
                  <span class="min-w-0 truncate">{{ m.model }}</span>
                  <span class="shrink-0 font-mono text-xs tabular-nums">{{ formatCompactCount(m.requests) }} req</span>
                </button>
              </li>
            </ul>
            <div v-if="hiddenModels > 0">
              <Button variant="outline" size="sm" @click="showAll">Show all {{ providerModels.length.toLocaleString('en-US') }} in the table</Button>
            </div>
          </template>
        </PanelSection>

        <PanelSection title="Base URL">
          <div class="flex items-center gap-2">
            <code ref="urlEl" class="bg-muted min-w-0 flex-1 rounded px-2 py-1 font-mono text-xs break-all">{{ provider.baseUrl }}</code>
            <Button variant="outline" size="sm" class="shrink-0" :aria-label="`Copy base URL of ${id}`" @click="copyUrl">{{ COPY_LABEL[copy.state.value] }}</Button>
          </div>
        </PanelSection>
      </template>
    </div>
  </DetailPanel>
</template>
