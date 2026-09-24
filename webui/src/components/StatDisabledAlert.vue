<script setup lang="ts">
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { useNavStore } from '@/stores/nav'

/**
 * StatDisabledAlert (reuse-audit.md F3) is the ONE shared "a fleet
 * statistic is switched off in the middleware config" hint: a warn Alert
 * with a config-key `<code>` reference and an "Open Config" button that
 * navigates to the Config page. Replaces the 5 hand-rolled copies in
 * ProviderHealthPanel, ModelCatalogTable, ReliabilityPage,
 * AttributionDrilldown and UserDetail — those differed only in title,
 * config key and trailing purpose text (some dynamic, e.g. naming the
 * selected provider), which is exactly what these three props carry.
 */
const props = defineProps<{
  /** e.g. "Latency statistics are off" or "Per-user-model statistics are off" */
  title: string
  /** dotted config key shown as inline code, e.g. "admin.stats.latency" */
  configKey: string
  /** trailing sentence after "Enable <code>{configKey}</code> in the middleware config ", e.g. "to see fleet p50/p95." */
  purpose: string
}>()

const nav = useNavStore()
</script>

<template>
  <Alert variant="warn">
    <AlertTitle>{{ props.title }}</AlertTitle>
    <AlertDescription class="flex flex-wrap items-center gap-2">
      <span>Enable <code class="font-mono text-xs">{{ props.configKey }}</code> in the middleware config {{ props.purpose }}</span>
      <Button type="button" variant="outline" size="sm" @click="nav.goTo('config')">Open Config</Button>
    </AlertDescription>
  </Alert>
</template>
