<script setup lang="ts">
import { Badge, Button, DescriptionItem, DescriptionList, StatusDot } from '@perfect-design/ui'
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'

import LimitsForm from '@/components/forms/LimitsForm.vue'
import { useNow } from '@/composables/useNow'
import { providerAccessCell } from '@/lib/access-matrix'
import { kindLabel } from '@/lib/events-filter'
import { formatCompactCount } from '@/lib/format'
import { formatLastSeen } from '@/lib/last-seen'
import { loadState } from '@/lib/load-state'
import { effectiveLimitRows, meterModel } from '@/lib/meters'
import { useConsumersStore } from '@/stores/consumers'
import { useDashboardStore } from '@/stores/dashboard'
import { useEventsStore } from '@/stores/events'
import { useFiltersStore } from '@/stores/filters'
import { useNavStore } from '@/stores/nav'
import DetailPanel from '@/widgets/DetailPanel.vue'
import MeterBar from '@/widgets/MeterBar.vue'
import ModelName from '@/widgets/ModelName.vue'
import PanelSection from '@/widgets/PanelSection.vue'
import StateView from '@/widgets/StateView.vue'
import type { ViewState } from '@/widgets/StateView.vue'

const EVENT_ROWS = 8
const THROTTLE_KINDS: ReadonlySet<string> = new Set(['rate_limit', 'paced'])

const props = defineProps<{ id: string }>()
const emit = defineEmits<{ close: [] }>()

const dashboard = useDashboardStore()
const consumers = useConsumersStore()
const events = useEventsStore()
const filters = useFiltersStore()
const nav = useNavStore()
const { now, start: startClock, stop: stopClock } = useNow()

const entry = computed(() => dashboard.usage?.users.find((u) => u.id === props.id))
const consumer = computed(() => consumers.data?.users.find((u) => u.name === props.id))
const groups = computed(() => entry.value?.groups ?? (entry.value?.groupName ? [entry.value.groupName] : []))

// --- Meters + effective limits ---------------------------------------------

const meters = computed(() => (entry.value ? meterModel(entry.value, entry.value.limits) : []))
const limitRows = computed(() => effectiveLimitRows(entry.value?.limits, entry.value?.limitSources))
const exempt = computed(() => entry.value?.exemptRequestsPerMonth ?? 0)

// --- Models used ------------------------------------------------------------

const userModelStats = computed(() => dashboard.overview?.features.userModelStats ?? false)
const detail = computed(() => consumers.detail[props.id])
const modelRows = computed(() =>
  [...(detail.value?.modelTotals?.rows ?? [])].sort((a, b) => (b.values.req ?? 0) - (a.values.req ?? 0)),
)
const modelsState = computed<ViewState>(() => {
  if (dashboard.overview !== null && (!userModelStats.value || detail.value?.modelTotalsUnavailable)) return 'empty'
  const s = loadState({ loading: detail.value?.loading ?? true, hasData: detail.value?.modelTotals != null, error: detail.value?.modelError ?? '' })
  if (s === 'skeleton') return 'loading'
  if (s === 'error') return 'error'
  return modelRows.value.length === 0 ? 'empty' : 'ready'
})
const modelsEmptyTitle = computed(() => (userModelStats.value ? 'No model traffic in this range' : 'Per-user model statistics are off'))
const modelsEmptyDescription = computed(() => (userModelStats.value ? undefined : 'Enable admin.stats.userModel to see which models this user calls.'))

function loadModels(): Promise<void> {
  return consumers.fetchUserDetail(props.id, filters.window, filters.span, userModelStats.value)
}

// --- Throttle events --------------------------------------------------------

const throttled = computed(() => events.events.filter((e) => e.user === props.id && THROTTLE_KINDS.has(e.kind)).slice(0, EVENT_ROWS))
const eventsState = computed<ViewState>(() => {
  const s = loadState({ loading: events.refreshing, hasData: events.lastUpdated !== null, error: events.error })
  if (s === 'skeleton') return 'loading'
  if (s === 'error') return 'error'
  return throttled.value.length === 0 ? 'empty' : 'ready'
})
const EVENT_TONE: Record<string, 'warning' | 'info'> = { rate_limit: 'warning', paced: 'info' }

function clock(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', hour12: false })
}

// --- Access ----------------------------------------------------------------

const groupRows = computed(() => (consumers.data?.groups ?? []).filter((g) => groups.value.includes(g.name)))
const providers = computed(() => {
  const out = new Map<string, string>()
  for (const g of groupRows.value) {
    for (const p of g.allowedProviders) {
      const cell = providerAccessCell(g, p)
      if (!out.has(p) || (cell.detail && !out.get(p))) out.set(p, cell.detail ?? '')
    }
  }
  return [...out].map(([name, detail]) => ({ name, detail })).sort((a, b) => a.name.localeCompare(b.name))
})
const accessState = computed<ViewState>(() => (consumers.data ? 'ready' : consumers.error ? 'error' : 'loading'))

// --- Snippet -----------------------------------------------------------------

const snippetOpen = ref(false)
const groupNames = computed(() => (consumers.data?.groups ?? []).map((g) => g.name))
const userNames = computed(() => (consumers.data?.users ?? []).map((u) => u.name))

watch([() => props.id, () => filters.range, userModelStats], () => void loadModels())
watch(() => props.id, () => (snippetOpen.value = false))
onMounted(() => {
  startClock()
  events.startPolling()
  void consumers.ensureConsumers()
  void loadModels()
})
onUnmounted(() => {
  stopClock()
  events.stopPolling()
})
</script>

<template>
  <DetailPanel kind="user" :id="id" :title="id" @close="emit('close')">
    <div class="flex flex-col divide-y px-1 pb-4" data-testid="user-panel">
      <div v-if="!entry" class="text-muted-foreground py-3 text-sm">No usage scope for this user yet. It appears after their first request.</div>
      <template v-else>
        <div class="flex flex-wrap items-center gap-2 pb-3 text-sm">
          <Badge v-if="consumer?.admin" tone="danger" size="sm">admin</Badge>
          <Badge v-if="consumer?.source" tone="neutral" variant="outline" size="sm">{{ consumer.source }}</Badge>
          <button
            v-for="g in groups"
            :key="g"
            type="button"
            class="hover:bg-accent rounded-md border px-2 py-0.5 text-xs"
            @click="nav.openPanel('group', g)"
          >
            {{ g }}
          </button>
          <span class="text-muted-foreground ml-auto text-xs">last seen {{ formatLastSeen(entry.lastSeen, now) }}</span>
        </div>

        <PanelSection title="Meters">
          <div v-if="meters.length" class="flex flex-col gap-3">
            <MeterBar v-for="m in meters" :key="m.label" :meter="m" />
          </div>
          <p v-else class="text-muted-foreground m-0 text-sm">No limits apply to this user.</p>
        </PanelSection>

        <PanelSection title="Effective limits">
          <DescriptionList v-if="limitRows.length || exempt" variant="inline" size="sm" density="compact">
            <DescriptionItem v-for="r in limitRows" :key="r.key" :term="r.label">
              <span class="font-mono tabular-nums">{{ r.value }}</span>
              <span v-if="r.source" class="text-muted-foreground"> · {{ r.source }}</span>
            </DescriptionItem>
            <DescriptionItem v-if="exempt" term="Exempt requests/month">
              <span class="font-mono tabular-nums">{{ formatCompactCount(exempt) }}</span>
              <span class="text-muted-foreground"> · limitExemptKinds</span>
            </DescriptionItem>
          </DescriptionList>
          <p v-else class="text-muted-foreground m-0 text-sm">No limits set.</p>
        </PanelSection>

        <PanelSection title="Models used">
          <StateView :state="modelsState" :empty-title="modelsEmptyTitle" :empty-description="modelsEmptyDescription" :error-message="detail?.modelError" :rows="3" @retry="loadModels()">
            <ul class="m-0 flex list-none flex-col p-0" aria-label="Models used">
              <li v-for="m in modelRows" :key="m.id" class="flex h-8 items-center justify-between gap-3 px-2 text-sm">
                <ModelName :id="m.id" class="min-w-0" />
                <span class="shrink-0 text-right font-mono text-xs tabular-nums">{{ formatCompactCount(m.values.req ?? 0) }} req</span>
              </li>
            </ul>
          </StateView>
        </PanelSection>

        <PanelSection title="Recent rate-limit and paced events">
          <StateView :state="eventsState" empty-title="No recent throttling" :error-message="events.error" :rows="2" @retry="events.refresh()">
            <ul class="m-0 flex list-none flex-col p-0" aria-label="Recent throttle events">
              <li v-for="(e, i) in throttled" :key="`${e.time}-${i}`" class="flex h-8 items-center gap-2 px-2 text-sm" :title="e.message">
                <StatusDot class="shrink-0" :tone="EVENT_TONE[e.kind] ?? 'neutral'" size="md" />
                <span class="text-muted-foreground shrink-0 font-mono text-xs">{{ clock(e.time) }}</span>
                <span class="text-muted-foreground shrink-0">{{ kindLabel(e.kind) }}</span>
                <span class="min-w-0 flex-1 truncate">{{ e.model || e.provider || e.route }}</span>
              </li>
            </ul>
          </StateView>
        </PanelSection>

        <PanelSection title="Access">
          <StateView :state="accessState" :error-message="consumers.error" :rows="2" @retry="consumers.fetchConsumers()">
            <DescriptionList variant="stacked" size="sm" density="compact">
              <DescriptionItem term="Providers">
                <span v-if="providers.length === 0" class="text-muted-foreground">None granted by group</span>
                <span v-else class="flex flex-wrap gap-1">
                  <Badge v-for="p in providers" :key="p.name" tone="neutral" variant="outline" size="sm" class="font-mono">{{ p.name }}<template v-if="p.detail"> · {{ p.detail }}</template></Badge>
                </span>
              </DescriptionItem>
              <DescriptionItem v-if="consumer?.providers?.length" term="Personal providers">
                <span class="flex flex-wrap gap-1">
                  <Badge v-for="p in consumer.providers" :key="p" tone="neutral" variant="outline" size="sm" class="font-mono">{{ p }}</Badge>
                </span>
              </DescriptionItem>
              <DescriptionItem v-if="consumer?.models?.length" term="Personal models">
                <span class="flex flex-col gap-1">
                  <ModelName v-for="m in consumer.models" :id="m" :key="m" />
                </span>
              </DescriptionItem>
            </DescriptionList>
          </StateView>
        </PanelSection>

        <PanelSection title="Change limits">
          <div>
            <Button variant="outline" size="sm" :aria-expanded="snippetOpen" @click="snippetOpen = !snippetOpen">
              {{ snippetOpen ? 'Hide limits snippet' : 'Generate limits snippet' }}
            </Button>
          </div>
          <LimitsForm v-if="snippetOpen" :group-names="groupNames" :user-names="userNames" preset-kind="user" :preset-name="id" />
        </PanelSection>
      </template>
    </div>
  </DetailPanel>
</template>
