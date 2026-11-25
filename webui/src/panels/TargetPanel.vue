<script setup lang="ts">
import { Button, DescriptionItem, DescriptionList, StatusBadge } from '@perfect-design/ui'
import { computed, ref, watch } from 'vue'

import { useCopyFeedback } from '@/composables/useCopyFeedback'
import { useEntityLinks } from '@/composables/useEntityLinks'
import { adminFetch, messageOf } from '@/lib/api'
import { copyText } from '@/lib/clipboard'
import { formatCompactCount, formatElapsedAgo, formatLatencyMs } from '@/lib/format'
import { dayOrMonthClampNotice, dayOrMonthWindow, totalsUrl } from '@/lib/range'
import { parseTargetCallerId, resolveTargetRef } from '@/lib/target-columns'
import { findTarget, hostOf } from '@/lib/target-rows'
import { useDashboardStore } from '@/stores/dashboard'
import { useFiltersStore } from '@/stores/filters'
import type { AdminTotalsResponse } from '@/types/api'
import DetailPanel from '@/widgets/DetailPanel.vue'
import PanelSection from '@/widgets/PanelSection.vue'
import StateView from '@/widgets/StateView.vue'
import type { ViewState } from '@/widgets/StateView.vue'

const CALLER_ROWS = 8
const HEALTH_TONE = { healthy: 'success', unhealthy: 'danger', unknown: 'neutral' } as const
const COPY_LABEL = { idle: 'Copy', copied: 'Copied', selected: 'Selected', failed: 'Copy failed' } as const

const props = defineProps<{ id: string }>()
const emit = defineEmits<{ close: [] }>()

const dashboard = useDashboardStore()
const filters = useFiltersStore()
const { openEntity } = useEntityLinks()
const copy = useCopyFeedback()
const urlEl = ref<HTMLElement | null>(null)

const target = computed(() => findTarget(dashboard.targets, props.id))
const targetRef = computed(() => {
  const res = resolveTargetRef(props.id, dashboard.targets)
  return res.kind === 'ref' ? res.ref : null
})
const title = computed(() => target.value?.name ?? props.id)
const kindLabel = computed(() => (targetRef.value?.startsWith('agent/') ? 'Agent' : 'MCP server'))
const health = computed(() => target.value?.health)
const healthState = computed(() => (health.value?.state === 'healthy' || health.value?.state === 'unhealthy' ? health.value.state : 'unknown'))
const checkedAgo = computed(() => formatElapsedAgo(health.value?.lastCheck))
const access = computed(() => {
  const a = target.value?.access
  if (a == null) return 'every group'
  return a.length === 0 ? 'no group' : a.join(', ')
})

async function copyUrl(): Promise<void> {
  copy.set(await copyText(target.value?.url ?? '', urlEl.value ?? undefined))
}

const clampNote = computed(() => dayOrMonthClampNotice(filters.window, filters.span))
const callers = ref<AdminTotalsResponse['rows']>([])
const callersState = ref<ViewState>('loading')
const callersError = ref('')
let reqId = 0

async function loadCallers(): Promise<void> {
  const requestId = ++reqId
  const reference = targetRef.value
  if (!reference) return
  callersState.value = 'loading'
  callersError.value = ''
  try {
    const { window, span } = dayOrMonthWindow(filters.window, filters.span)
    const res = await adminFetch<AdminTotalsResponse>(totalsUrl({ kind: 'targetcaller', window, span, metrics: ['req'], limit: CALLER_ROWS, target: reference }))
    if (requestId !== reqId) return
    callers.value = res.rows
    callersState.value = res.rows.length === 0 ? 'empty' : 'ready'
  } catch (err) {
    if (requestId !== reqId) return
    callersError.value = messageOf(err)
    callersState.value = 'error'
  }
}
watch([targetRef, () => filters.window, () => filters.span], () => void loadCallers(), { immediate: true })

const callerRows = computed(() =>
  callers.value
    .map((r) => ({ id: r.id, caller: parseTargetCallerId(r.id).caller || r.id, requests: r.values.req ?? 0 }))
    .sort((a, b) => b.requests - a.requests || a.caller.localeCompare(b.caller)),
)
</script>

<template>
  <DetailPanel kind="target" :id="id" :title="title" @close="emit('close')">
    <div class="flex flex-col divide-y px-1 pb-4" data-testid="target-panel">
      <div v-if="!target" class="text-muted-foreground py-3 text-sm">This MCP server or agent is not configured.</div>
      <template v-else>
        <PanelSection title="Health">
          <DescriptionList variant="inline" size="sm" density="compact">
            <DescriptionItem term="Kind">{{ kindLabel }}</DescriptionItem>
            <DescriptionItem term="Status"><StatusBadge :status="healthState" :tones="HEALTH_TONE" size="sm" /></DescriptionItem>
            <DescriptionItem v-if="health?.latencyMs !== undefined" term="Latency">
              <span class="font-mono tabular-nums">{{ formatLatencyMs(health.latencyMs) }}</span>
            </DescriptionItem>
            <DescriptionItem v-if="checkedAgo" term="Last check">{{ checkedAgo }}{{ health?.source ? ` via ${health.source}` : '' }}</DescriptionItem>
            <DescriptionItem v-if="health && health.consecutiveFailures > 0" term="Consecutive failures">
              <span class="font-mono tabular-nums">{{ health.consecutiveFailures }}</span>
            </DescriptionItem>
          </DescriptionList>
          <p v-if="health?.lastError" class="text-destructive m-0 text-sm break-words whitespace-pre-wrap" data-testid="target-last-err">{{ health.lastError }}</p>
          <p v-else-if="healthState === 'unknown'" class="text-muted-foreground m-0 text-sm">Not observed yet: no traffic, and probes are off or have not run.</p>
        </PanelSection>

        <PanelSection title="Endpoint">
          <DescriptionList variant="inline" size="sm" density="compact">
            <DescriptionItem term="Host"><span class="font-mono">{{ hostOf(target.url) }}</span></DescriptionItem>
            <DescriptionItem term="Access">{{ access }}</DescriptionItem>
          </DescriptionList>
          <div class="flex items-center gap-2">
            <code ref="urlEl" class="bg-muted min-w-0 flex-1 rounded px-2 py-1 font-mono text-xs break-all">{{ target.url }}</code>
            <Button variant="outline" size="sm" class="shrink-0" :aria-label="`Copy URL of ${title}`" @click="copyUrl">{{ COPY_LABEL[copy.state.value] }}</Button>
          </div>
        </PanelSection>

        <PanelSection title="Requests">
          <DescriptionList variant="inline" size="sm" density="compact">
            <DescriptionItem term="Per minute"><span class="font-mono tabular-nums">{{ formatCompactCount(target.counters.requestsPerMinute) }}</span></DescriptionItem>
            <DescriptionItem term="Today"><span class="font-mono tabular-nums">{{ formatCompactCount(target.counters.requestsPerDay) }}</span></DescriptionItem>
            <DescriptionItem term="This month"><span class="font-mono tabular-nums">{{ formatCompactCount(target.counters.requestsPerMonth) }}</span></DescriptionItem>
          </DescriptionList>
        </PanelSection>

        <PanelSection title="Top callers">
          <p v-if="clampNote" class="text-muted-foreground m-0 text-xs" data-testid="clamped-range">{{ clampNote }}</p>
          <StateView :state="callersState" empty-title="No callers in this range" :error-message="callersError" :rows="3" @retry="loadCallers()">
            <ul class="m-0 flex list-none flex-col p-0" aria-label="Top callers">
              <li v-for="c in callerRows" :key="c.id">
                <button type="button" class="hover:bg-accent flex h-8 w-full items-center justify-between gap-3 rounded px-2 text-left text-sm" :title="c.caller" @click="openEntity('user', c.caller)">
                  <span class="min-w-0 truncate">{{ c.caller }}</span>
                  <span class="shrink-0 font-mono text-xs tabular-nums">{{ formatCompactCount(c.requests) }} req</span>
                </button>
              </li>
            </ul>
          </StateView>
        </PanelSection>
      </template>
    </div>
  </DetailPanel>
</template>
