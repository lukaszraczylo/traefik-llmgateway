<script setup lang="ts">
import { Badge, Button, DescriptionItem, DescriptionList, Heatmap, StatBar } from '@perfect-design/ui'
import { computed, ref, watch } from 'vue'

import RouterForm from '@/components/forms/RouterForm.vue'
import { decisionView } from '@/lib/decision-view'
import { formatCompactCount, formatExactInt } from '@/lib/format'
import { formatSel } from '@/lib/panel-selection'
import { routerPanelModel } from '@/lib/router-panel'
import { useModelsStore } from '@/stores/models'
import { useNavStore } from '@/stores/nav'
import DetailPanel from '@/widgets/DetailPanel.vue'
import ModelName from '@/widgets/ModelName.vue'
import PanelSection from '@/widgets/PanelSection.vue'

const OUTCOME_TONE = { ok: 'success', slow: 'warning', escalated: 'warning', bad: 'danger', error: 'danger' } as const

const props = defineProps<{ id: string }>()
const emit = defineEmits<{ close: [] }>()

const models = useModelsStore()
const nav = useNavStore()

const view = computed(() => models.routers.find((r) => r.name === props.id))
const model = computed(() => (view.value ? routerPanelModel(view.value) : null))
const decision = computed(() => (view.value ? decisionView(view.value) : null))
const known = computed(() => new Set(models.rows.map((r) => r.id)))
const requests = computed(() => view.value?.stats?.requests ?? 0)
const scope = computed(() => (view.value?.stats?.scope === 'shared' ? 'all replicas, this month (UTC)' : 'this gateway instance, since start'))
const escalations = computed(() => view.value?.stats?.escalations ?? [])
const reasonTotal = computed(() => model.value?.reasons.reduce((a, r) => a + r.count, 0) ?? 0)
const agreement = computed(() => {
  const m = model.value?.shadow
  if (!m) return null
  const total = m.values.flat().reduce((a, b) => a + b, 0)
  const same = m.values.reduce((a, row, i) => a + row[i], 0)
  return total > 0 ? Math.round((same * 100) / total) : null
})

const tierValue = (count: number, pct: number): string => `${formatCompactCount(count)} · ${pct}%`

function openModel(id: string): void {
  nav.goTo('models', { sel: formatSel('model', id) }, { replace: false })
}

const snippetOpen = ref(false)
watch(() => props.id, () => (snippetOpen.value = false))
</script>

<template>
  <DetailPanel kind="router" :id="id" :title="id" @close="emit('close')">
    <div class="flex flex-col divide-y px-1 pb-4" data-testid="router-panel">
      <div v-if="!view || !model" class="text-muted-foreground py-3 text-sm">This router is no longer configured.</div>
      <template v-else>
        <PanelSection title="Tiers">
          <ul class="m-0 flex list-none flex-col gap-2 p-0" aria-label="Router tiers">
            <li v-for="t in model.tiers" :key="t.name" class="flex flex-col gap-1" data-testid="router-tier">
              <div class="flex h-6 items-center gap-2 text-sm">
                <span class="font-mono font-medium">{{ t.name }}</span>
                <Badge v-if="t.isDefault" tone="neutral" variant="outline" size="sm">default</Badge>
                <span class="text-muted-foreground ml-auto font-mono text-xs tabular-nums">min score {{ formatExactInt(t.minScore) }}</span>
              </div>
              <ul class="m-0 flex list-none flex-col p-0">
                <li v-for="m in t.models" :key="m" class="flex min-h-8 items-center">
                  <button
                    v-if="known.has(m)"
                    type="button"
                    class="hover:bg-accent focus-visible:ring-ring flex w-full min-w-0 items-center justify-between gap-2 rounded px-2 text-left font-mono text-sm underline decoration-dotted underline-offset-4 focus-visible:ring-2 focus-visible:outline-none"
                    @click="openModel(m)"
                  >
                    <ModelName :id="m" class="min-w-0 truncate" />
                    <span aria-hidden="true" class="text-muted-foreground shrink-0 no-underline">→</span>
                  </button>
                  <ModelName v-else :id="m" class="min-w-0 truncate px-2 font-mono text-sm" />
                </li>
              </ul>
            </li>
          </ul>
        </PanelSection>

        <PanelSection :title="`Tier split · ${formatExactInt(requests)} requests`">
          <p v-if="requests === 0" class="text-muted-foreground m-0 text-sm">No routed requests yet.</p>
          <div v-else class="flex flex-col gap-3">
            <StatBar
              v-for="s in model.split"
              :key="s.name"
              :label="s.name"
              :value="s.count"
              :max="Math.max(1, model.split.reduce((a, b) => a + b.count, 0))"
              :value-text="tierValue(s.count, s.pct)"
              tone="primary"
              size="md"
            />
          </div>
          <p class="text-muted-foreground m-0 text-xs">Counts: {{ scope }}</p>
        </PanelSection>

        <PanelSection title="Routing reasons">
          <p v-if="model.reasons.length === 0" class="text-muted-foreground m-0 text-sm">No reasons recorded.</p>
          <DescriptionList v-else variant="inline" size="sm" density="compact">
            <DescriptionItem v-for="r in model.reasons" :key="r.name" :term="r.name">
              <span class="font-mono tabular-nums">{{ formatExactInt(r.count) }}<span class="text-muted-foreground"> · {{ reasonTotal > 0 ? Math.round((r.count * 100) / reasonTotal) : 0 }}%</span></span>
            </DescriptionItem>
          </DescriptionList>
        </PanelSection>

        <PanelSection title="Outcomes">
          <p v-if="model.outcomes.length === 0" class="text-muted-foreground m-0 text-sm">No finished requests yet.</p>
          <div v-else class="flex flex-col gap-3">
            <StatBar
              v-for="o in model.outcomes"
              :key="o.outcome"
              :label="o.outcome"
              :value="o.count"
              :max="Math.max(1, model.outcomes.reduce((a, b) => a + b.count, 0))"
              :value-text="tierValue(o.count, o.pct)"
              :tone="OUTCOME_TONE[o.outcome as keyof typeof OUTCOME_TONE] ?? 'primary'"
              size="md"
            />
          </div>
        </PanelSection>

        <PanelSection v-if="escalations.length > 0" title="Escalations">
          <DescriptionList variant="inline" size="sm" density="compact">
            <DescriptionItem v-for="e in escalations" :key="`${e.from}>${e.to}`" :term="`${e.from} → ${e.to}`">
              <span class="font-mono tabular-nums">{{ formatExactInt(e.count) }}</span>
            </DescriptionItem>
          </DescriptionList>
        </PanelSection>

        <PanelSection v-if="model.shadow" title="Shadow classifier">
          <p class="text-muted-foreground m-0 text-xs">
            Rows: live tier. Columns: shadow tier.<template v-if="agreement !== null"> Agreement <span class="font-mono tabular-nums">{{ agreement }}%</span>.</template>
          </p>
          <div data-wb-ignore-numeric>
            <Heatmap
              :values="model.shadow.values"
              :row-labels="model.shadow.rows"
              :col-labels="model.shadow.cols"
              tone="info"
              :levels="5"
              fit
              :min="0"
              show-legend
              label="Live tier by shadow tier request counts"
              :cell-title="(v: number | null, r: string, c: string) => `live ${r}, shadow ${c}: ${formatExactInt(v ?? 0)}`"
            />
          </div>
        </PanelSection>

        <PanelSection v-if="decision" title="Decision model">
          <p class="text-muted-foreground m-0 font-mono text-xs" data-testid="decision-summary">{{ decision.summary }}</p>
          <p v-if="decision.grid" class="text-muted-foreground m-0 text-xs">Rows: heuristic tier. Columns: model tier.</p>
          <div v-if="decision.grid" data-wb-ignore-numeric>
            <Heatmap
              :values="decision.grid.values"
              :row-labels="decision.grid.rows"
              :col-labels="decision.grid.cols"
              tone="info"
              :levels="5"
              fit
              :min="0"
              show-legend
              label="Heuristic tier by model tier request counts"
              :cell-title="(v: number | null, r: string, c: string) => `heuristic ${r}, model ${c}: ${formatExactInt(v ?? 0)}`"
            />
          </div>
          <p v-else class="text-muted-foreground m-0 text-sm">No decisions recorded yet.</p>
          <DescriptionList variant="inline" size="sm" density="compact">
            <DescriptionItem v-for="o in decision.outcomes" :key="o.name" :term="o.name">
              <span class="font-mono tabular-nums">{{ formatExactInt(o.count) }}</span>
            </DescriptionItem>
          </DescriptionList>
        </PanelSection>

        <PanelSection title="Settings">
          <DescriptionList variant="inline" size="sm" density="compact">
            <DescriptionItem v-for="s in model.settings" :key="s.key" :term="s.label">
              <span class="font-mono tabular-nums">{{ s.value }}</span>
            </DescriptionItem>
          </DescriptionList>
        </PanelSection>

        <PanelSection title="Change router">
          <div class="flex flex-wrap gap-2">
            <Button variant="outline" size="sm" :aria-expanded="snippetOpen" @click="snippetOpen = !snippetOpen">
              {{ snippetOpen ? 'Hide router snippet' : 'Generate router snippet' }}
            </Button>
          </div>
          <RouterForm v-if="snippetOpen" />
        </PanelSection>
      </template>
    </div>
  </DetailPanel>
</template>
