<script setup lang="ts">
import { computed } from 'vue'

import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { formatContextWindow } from '@/lib/format'
import type { AdminRouterView } from '@/types/api'

/**
 * SmartRoutersCard is the Models page's read-only view of every configured
 * smart router (admin_routers.go): tiers cheapest first with their score
 * floor and models, plus selection, attempt cap and keyword rules. Config
 * lives in the plugin config; use the Config page's router helper to build a
 * snippet. Counts are fleet-wide per month with Redis, else per process.
 */
const props = defineProps<{
  routers: AdminRouterView[]
}>()

const scopeLabel = computed(() =>
  props.routers.some((r) => r.stats?.scope === 'shared') ? 'Counts: All replicas, this month (UTC)' : 'Counts: This gateway instance, since start',
)

function share(count: number, total: number): number {
  return total > 0 ? Math.round((count / total) * 100) : 0
}
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>Smart routers</CardTitle>
      <CardDescription>Virtual models that pick a tier by request complexity and escalate on failure. {{ scopeLabel }}.</CardDescription>
    </CardHeader>
    <CardContent class="flex flex-col gap-4">
      <p v-if="routers.length === 0" class="text-sm text-muted-foreground">None configured.</p>
      <section v-for="router in routers" :key="router.name" class="flex flex-col gap-2" :aria-label="`Router ${router.name}`">
        <div class="flex flex-wrap items-center gap-2">
          <span class="font-mono text-sm font-medium">{{ router.name }}</span>
          <Badge variant="outline" class="font-normal">default tier: {{ router.defaultTier }}</Badge>
          <Badge variant="outline" class="font-normal">selection: {{ router.selection }}</Badge>
          <Badge variant="outline" class="font-normal">max attempts: {{ router.maxAttempts }}</Badge>
          <Badge variant="outline" class="font-normal">context escalation: {{ router.contextEscalation ? 'on' : 'off' }}</Badge>
          <Badge v-if="router.contextTokens" variant="outline" class="font-normal">
            context window: {{ formatContextWindow(router.contextTokens) }}
          </Badge>
          <Badge variant="secondary" class="font-normal">requests: {{ router.stats?.requests ?? 0 }}</Badge>
        </div>
        <ol class="flex flex-col gap-1 text-sm">
          <li v-for="(tier, index) in router.tiers" :key="tier.name" class="flex flex-col gap-1">
            <div class="flex flex-wrap items-baseline gap-2">
              <span class="font-medium">{{ tier.name }}</span>
              <span class="text-xs text-muted-foreground">from score {{ tier.minScore }}</span>
              <span class="font-mono text-xs break-all">{{ tier.models.join(', ') }}</span>
              <span class="ml-auto text-xs tabular-nums text-muted-foreground">
                {{ router.stats?.tiers[index]?.count ?? 0 }} req ({{ share(router.stats?.tiers[index]?.count ?? 0, router.stats?.requests ?? 0) }}%)
              </span>
            </div>
            <div class="h-1.5 w-full overflow-hidden rounded-full bg-muted" aria-hidden="true">
              <div
                class="h-full rounded-full bg-primary"
                :style="{ width: `${share(router.stats?.tiers[index]?.count ?? 0, router.stats?.requests ?? 0)}%` }"
              />
            </div>
          </li>
        </ol>
        <p v-if="router.stats && router.stats.reasons.length > 0" class="flex flex-wrap items-center gap-1 text-xs text-muted-foreground">
          Routing reasons:
          <Badge v-for="reason in router.stats.reasons" :key="reason.name" variant="outline" class="font-normal">
            {{ reason.name }}: {{ reason.count }}
          </Badge>
        </p>
        <ul v-if="router.stats && router.stats.escalations.length > 0" class="flex flex-col gap-0.5 text-xs text-muted-foreground">
          <li v-for="esc in router.stats.escalations" :key="`${esc.from}>${esc.to}`">
            Escalated <span class="font-medium">{{ esc.from }}</span> to <span class="font-medium">{{ esc.to }}</span>:
            {{ esc.count }}
          </li>
        </ul>
        <p v-for="(rule, index) in router.keywordRules" :key="index" class="text-xs text-muted-foreground">
          Keywords to <span class="font-medium">{{ rule.tier }}</span
          >: <span class="font-mono">{{ rule.keywords.join(', ') }}</span>
        </p>
      </section>
    </CardContent>
  </Card>
</template>
