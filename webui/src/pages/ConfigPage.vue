<script setup lang="ts">
import { computed, onMounted } from 'vue'

import ChangeHelper from '@/components/ChangeHelper.vue'
import ConfigTree from '@/components/ConfigTree.vue'
import ErrorState from '@/components/ErrorState.vue'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
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
    <Card v-if="config.error && !config.data">
      <CardHeader>
        <CardTitle>Config</CardTitle>
      </CardHeader>
      <CardContent>
        <ErrorState :message="config.error" :on-retry="config.fetchConfig" />
      </CardContent>
    </Card>
    <!-- Mirrors ConfigTree's own final SnippetBlock shape (a label line above a bordered block) so it causes no layout shift once the real config arrives. -->
    <Card v-else-if="config.loading && !config.data">
      <CardContent class="flex flex-col gap-1.5">
        <Skeleton class="h-3 w-56" />
        <Skeleton class="h-64 w-full rounded-md" />
      </CardContent>
    </Card>
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
