<script setup lang="ts">
import { computed } from 'vue'

import SnippetBlock from '@/components/SnippetBlock.vue'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { emitYamlDocument, toYamlValue } from '@/lib/yaml-emit'
import type { YamlValue } from '@/lib/yaml-emit'

/**
 * ConfigTree (redesign-plan.md section 3.4) renders GET /admin/api/config's
 * already-redacted `config` (admin_config.go's redactConfig — every
 * apiKey/password literal replaced with "[redacted]", an "env:NAME"/
 * "file:/path" indirection kept, every baseUrl/url stripped of userinfo)
 * as read-only YAML via lib/yaml-emit.ts's generic recursive renderer —
 * safe to render as-is, never re-sanitized client-side (types/api.ts's
 * AdminConfigResponse doc comment).
 */
const props = defineProps<{
  config: Record<string, unknown>
  warnings: string[]
}>()

const yamlText = computed(() => {
  const value = toYamlValue(props.config) as Record<string, YamlValue | undefined>
  return emitYamlDocument(value)
})
</script>

<template>
  <div class="flex flex-col gap-4">
    <Alert v-for="(warning, index) in warnings" :key="index" variant="warn">
      <AlertTitle>Config warning</AlertTitle>
      <AlertDescription>{{ warning }}</AlertDescription>
    </Alert>
    <SnippetBlock label="Redacted running config (read-only)" :text="yamlText" />
  </div>
</template>
