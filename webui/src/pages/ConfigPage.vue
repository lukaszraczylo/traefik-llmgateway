<script setup lang="ts">
import { computed, onMounted } from 'vue'

import ChangeHelper from '@/components/ChangeHelper.vue'
import ConfigTree from '@/components/ConfigTree.vue'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { useConfigStore } from '@/stores/config'
import { useConsumersStore } from '@/stores/consumers'
import { useDashboardStore } from '@/stores/dashboard'

/**
 * ConfigPage (redesign-plan.md section 3.4) composes the redacted config
 * tree (ConfigTree.vue, GET /admin/api/config, fetched on demand) above
 * the change helper (ChangeHelper.vue's five snippet-building forms) —
 * group/user names for those forms' target pickers come from the already-
 * polled dashboard store (overview.groups) and the on-demand consumers
 * store (users), so this page fetches config once and reuses whatever the
 * other two stores already have rather than a third redundant call.
 */
const config = useConfigStore()
const dashboard = useDashboardStore()
const consumers = useConsumersStore()

onMounted(() => {
  void config.ensureConfig()
  void consumers.ensureConsumers()
})

const groupNames = computed(() => (dashboard.overview?.groups ?? []).map((g) => g.name))
const userNames = computed(() => (consumers.data?.users ?? []).map((u) => u.name))
</script>

<template>
  <div class="flex flex-col gap-6">
    <Card v-if="config.error">
      <CardHeader>
        <CardTitle>Config</CardTitle>
        <CardDescription class="text-destructive">{{ config.error }}</CardDescription>
      </CardHeader>
    </Card>
    <p v-else-if="config.loading && !config.data" class="text-sm text-muted-foreground">loading…</p>
    <ConfigTree v-else-if="config.data" :config="config.data.config" :warnings="config.data.warnings" />

    <Card>
      <CardHeader>
        <CardTitle>Change helper</CardTitle>
        <CardDescription>Build a paste-ready snippet for middleware.yaml or users.json — nothing here writes to the running config.</CardDescription>
      </CardHeader>
      <CardContent>
        <ChangeHelper :group-names="groupNames" :user-names="userNames" />
      </CardContent>
    </Card>
  </div>
</template>
