<script setup lang="ts">
import { Badge } from '@/components/ui/badge'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import type { AdminRouterView } from '@/types/api'

/**
 * SmartRoutersCard is the Models page's read-only view of every configured
 * smart router (admin_routers.go): tiers cheapest first with their score
 * floor and models, plus selection, attempt cap and keyword rules. Config
 * lives in the plugin config; use the Config page's router helper to build a
 * snippet.
 */
defineProps<{
  routers: AdminRouterView[]
}>()
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>Smart routers</CardTitle>
      <CardDescription>Virtual models that pick a tier by request complexity and escalate on failure.</CardDescription>
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
        </div>
        <ol class="flex flex-col gap-1 text-sm">
          <li v-for="tier in router.tiers" :key="tier.name" class="flex flex-wrap items-baseline gap-2">
            <span class="font-medium">{{ tier.name }}</span>
            <span class="text-xs text-muted-foreground">from score {{ tier.minScore }}</span>
            <span class="font-mono text-xs break-all">{{ tier.models.join(', ') }}</span>
          </li>
        </ol>
        <p v-for="(rule, index) in router.keywordRules" :key="index" class="text-xs text-muted-foreground">
          Keywords to <span class="font-medium">{{ rule.tier }}</span
          >: <span class="font-mono">{{ rule.keywords.join(', ') }}</span>
        </p>
      </section>
    </CardContent>
  </Card>
</template>
