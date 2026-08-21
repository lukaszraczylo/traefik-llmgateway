<script setup lang="ts">
import { faCircleCheck, faCircleXmark, faTriangleExclamation } from '@fortawesome/free-solid-svg-icons'
import { computed } from 'vue'

import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from '@/components/ui/card'
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { formatAgo, formatTimestamp } from '@/lib/format'
import { useDashboardStore } from '@/stores/dashboard'

const dashboard = useDashboardStore()
const overview = computed(() => dashboard.overview)
</script>

<template>
  <div class="flex flex-col gap-6">
    <div class="grid gap-4 sm:grid-cols-3">
      <Card>
        <CardHeader>
          <CardTitle class="flex items-center gap-2 text-sm font-medium text-muted-foreground">
            <FontAwesomeIcon
              :icon="overview?.redis.configured ? faCircleCheck : faCircleXmark"
              :class="overview?.redis.configured ? 'text-chart-requests' : 'text-muted-foreground'"
              class="size-3.5"
            />
            Redis
          </CardTitle>
        </CardHeader>
        <CardContent class="flex flex-col gap-1 text-sm">
          <p class="font-medium">{{ overview?.redis.configured ? 'Configured' : 'Not configured' }}</p>
          <p v-if="overview?.redis.lastErr" class="flex items-start gap-1.5 text-destructive">
            <FontAwesomeIcon :icon="faTriangleExclamation" class="mt-0.5 size-3.5 shrink-0" />
            <span>{{ overview.redis.lastErr }}{{ formatAgo(overview.redis.lastErrAt) }}</span>
          </p>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle class="flex items-center gap-2 text-sm font-medium text-muted-foreground">
            <FontAwesomeIcon
              :icon="overview?.cache.enabled ? faCircleCheck : faCircleXmark"
              :class="overview?.cache.enabled ? 'text-chart-requests' : 'text-muted-foreground'"
              class="size-3.5"
            />
            Response cache
          </CardTitle>
        </CardHeader>
        <CardContent class="text-sm">
          <p class="font-medium">{{ overview?.cache.enabled ? 'Enabled' : 'Disabled' }}</p>
          <p v-if="overview?.cache.enabled" class="text-muted-foreground">ttl {{ overview.cache.ttl }}</p>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle class="flex items-center gap-2 text-sm font-medium text-muted-foreground">
            <FontAwesomeIcon
              :icon="overview?.retry.enabled ? faCircleCheck : faCircleXmark"
              :class="overview?.retry.enabled ? 'text-chart-requests' : 'text-muted-foreground'"
              class="size-3.5"
            />
            Retry
          </CardTitle>
        </CardHeader>
        <CardContent class="text-sm">
          <p class="font-medium">{{ overview?.retry.enabled ? 'Enabled' : 'Disabled' }}</p>
          <p v-if="overview?.retry.enabled" class="text-muted-foreground">
            attempts {{ overview.retry.attempts }}, backoff {{ overview.retry.backoff }}
          </p>
        </CardContent>
      </Card>
    </div>

    <Card>
      <CardHeader>
        <CardTitle>Providers</CardTitle>
        <CardDescription>Every configured upstream and its last discovery refresh.</CardDescription>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Type</TableHead>
              <TableHead>Base URL</TableHead>
              <TableHead class="text-right">Models</TableHead>
              <TableHead>Last refresh</TableHead>
              <TableHead>Last error</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            <TableEmpty v-if="!overview?.providers.length" :colspan="6" class="text-muted-foreground">
              none
            </TableEmpty>
            <TableRow v-for="p in overview?.providers" :key="p.name">
              <TableCell class="font-medium">{{ p.name }}</TableCell>
              <TableCell class="text-muted-foreground">{{ p.type }}</TableCell>
              <TableCell class="text-muted-foreground">{{ p.baseUrl }}</TableCell>
              <TableCell class="text-right tabular-nums">{{ p.modelCount }}</TableCell>
              <TableCell class="text-muted-foreground">{{ formatTimestamp(p.lastRefresh) }}</TableCell>
              <TableCell class="text-destructive">{{ p.lastErr ?? '' }}</TableCell>
            </TableRow>
          </TableBody>
        </Table>
      </CardContent>
    </Card>

    <Card>
      <CardHeader>
        <CardTitle>Model aliases</CardTitle>
        <CardDescription>Operator-defined alias &rarr; target model mappings.</CardDescription>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Alias</TableHead>
              <TableHead>Target</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            <TableEmpty v-if="!overview?.aliases.length" :colspan="2" class="text-muted-foreground">
              none configured
            </TableEmpty>
            <TableRow v-for="a in overview?.aliases" :key="a.alias">
              <TableCell class="font-medium">{{ a.alias }}</TableCell>
              <TableCell class="text-muted-foreground">{{ a.target }}</TableCell>
            </TableRow>
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  </div>
</template>
