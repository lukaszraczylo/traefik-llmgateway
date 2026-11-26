<script setup lang="ts">
import { DescriptionItem, DescriptionList } from '@perfect-design/ui'
import { computed } from 'vue'

import { formatCostDisplay } from '@/lib/format'
import { effectiveLimitRows, meterModel } from '@/lib/meters'
import { countOrUnknown, formatCount, STORE_DOWN_HINT } from '@/lib/usage-store'
import { useDashboardStore } from '@/stores/dashboard'
import { useNavStore } from '@/stores/nav'
import DetailPanel from '@/widgets/DetailPanel.vue'
import MeterBar from '@/widgets/MeterBar.vue'
import PanelSection from '@/widgets/PanelSection.vue'

const MEMBER_ROWS = 12

const props = defineProps<{ id: string }>()
const emit = defineEmits<{ close: [] }>()

const dashboard = useDashboardStore()
const nav = useNavStore()

const entry = computed(() => dashboard.usage?.groups.find((g) => g.id === props.id))
const members = computed(() =>
  (dashboard.usage?.users ?? [])
    .filter((u) => (u.groups ?? [u.groupName]).includes(props.id))
    .sort((a, b) => b.requestsPerDay - a.requestsPerDay || a.id.localeCompare(b.id)),
)
const shownMembers = computed(() => members.value.slice(0, MEMBER_ROWS))

const meters = computed(() => (entry.value ? meterModel(entry.value, entry.value.limits) : []))
const aggregateRows = computed(() => effectiveLimitRows(entry.value?.limits, undefined))
const memberRows = computed(() => effectiveLimitRows(entry.value?.memberLimits, undefined))

const usageRows = computed(() => {
  const e = entry.value
  if (!e) return []
  const cost = (micros: number): string => (e.storeDown ? '—' : formatCostDisplay(micros))
  return [
    { term: 'Requests/min', value: formatCount(countOrUnknown(e.requestsPerMinute, e.storeDown)) },
    { term: 'Requests/day', value: formatCount(countOrUnknown(e.requestsPerDay, e.storeDown)) },
    { term: 'Tokens/day', value: formatCount(countOrUnknown(e.tokensInPerDay + e.tokensOutPerDay, e.storeDown)) },
    { term: 'Rejected/day', value: formatCount(countOrUnknown(e.rejectionsPerDay, e.storeDown)) },
    { term: 'Cost/day', value: cost(e.costPerDayMicroUsd) },
    { term: 'Cost/month', value: cost(e.costPerMonthMicroUsd) },
  ]
})
</script>

<template>
  <DetailPanel kind="group" :id="id" :title="id" @close="emit('close')">
    <div class="flex flex-col divide-y px-1 pb-4" data-testid="group-panel">
      <div v-if="!entry" class="text-muted-foreground py-3 text-sm">This group has no usage data yet.</div>
      <template v-else>
        <PanelSection title="Aggregate limits">
          <div v-if="meters.length" class="flex flex-col gap-3">
            <MeterBar v-for="m in meters" :key="m.label" :meter="m" />
          </div>
          <p v-else class="text-muted-foreground m-0 text-sm">No aggregate limits. Members are limited individually.</p>
          <DescriptionList v-if="aggregateRows.length" variant="inline" size="sm" density="compact">
            <DescriptionItem v-for="r in aggregateRows" :key="r.key" :term="r.label"><span :class="r.prose ? '' : 'font-mono tabular-nums'">{{ r.value }}</span></DescriptionItem>
          </DescriptionList>
        </PanelSection>

        <PanelSection title="Per-member limits">
          <DescriptionList v-if="memberRows.length" variant="inline" size="sm" density="compact">
            <DescriptionItem v-for="r in memberRows" :key="r.key" :term="r.label"><span :class="r.prose ? '' : 'font-mono tabular-nums'">{{ r.value }}</span></DescriptionItem>
          </DescriptionList>
          <p v-else class="text-muted-foreground m-0 text-sm">No per-member limits.</p>
        </PanelSection>

        <PanelSection :title="`Members (${members.length.toLocaleString('en-US')})`">
          <ul v-if="shownMembers.length" class="m-0 flex list-none flex-col p-0" aria-label="Group members">
            <li v-for="u in shownMembers" :key="u.id">
              <button type="button" class="hover:bg-accent flex h-8 w-full items-center justify-between gap-3 rounded-md px-2 text-left text-sm" @click="nav.openPanel('user', u.id)">
                <span class="min-w-0 truncate">{{ u.id }}</span>
                <span class="shrink-0 text-right font-mono text-xs tabular-nums">{{ formatCount(countOrUnknown(u.requestsPerDay, u.storeDown)) }} req/day</span>
              </button>
            </li>
          </ul>
          <p v-else class="text-muted-foreground m-0 text-sm">No members.</p>
          <p v-if="members.length > MEMBER_ROWS" class="text-muted-foreground m-0 px-2 text-xs">
            Top {{ MEMBER_ROWS }} by requests.
            <button type="button" class="text-primary underline-offset-2 hover:underline" @click="nav.goTo('users', { group: id }, { replace: false })">
              Show all {{ members.length.toLocaleString('en-US') }} on the Users tab
            </button>
          </p>
        </PanelSection>

        <PanelSection title="Usage">
          <p v-if="entry.storeDown" class="text-muted-foreground m-0 text-xs">{{ STORE_DOWN_HINT }}</p>
          <DescriptionList variant="inline" size="sm" density="compact">
            <DescriptionItem v-for="r in usageRows" :key="r.term" :term="r.term"><span class="font-mono tabular-nums">{{ r.value }}</span></DescriptionItem>
          </DescriptionList>
        </PanelSection>
      </template>
    </div>
  </DetailPanel>
</template>
