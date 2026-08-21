<script setup lang="ts">
import { formatCost, formatLimits } from '@/lib/format'
import {
  Table,
  TableBody,
  TableCell,
  TableEmpty,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import type { AdminUsageEntryView } from '@/types/api'

/**
 * UsageTable renders one scope kind's usage rows (users or groups) — the
 * shared table shape both need (vue.md: extract before you paste it
 * twice). The second column differs per kind: a user's row shows the group
 * it belongs to; a group's row shows its member count (looked up from the
 * Overview response, which is the only place member counts live).
 */
defineProps<{
  idLabel: string
  entries: AdminUsageEntryView[]
  secondaryColumnLabel: string
  secondaryValue: (entry: AdminUsageEntryView) => string
}>()
</script>

<template>
  <Table>
    <TableHeader>
      <TableRow>
        <TableHead>{{ idLabel }}</TableHead>
        <TableHead>{{ secondaryColumnLabel }}</TableHead>
        <TableHead>Limits</TableHead>
        <TableHead class="text-right">req/min</TableHead>
        <TableHead class="text-right">req/day</TableHead>
        <TableHead class="text-right">tokIn/day</TableHead>
        <TableHead class="text-right">tokOut/day</TableHead>
        <TableHead class="text-right">tokIn/month</TableHead>
        <TableHead class="text-right">tokOut/month</TableHead>
        <TableHead class="text-right">cost/day</TableHead>
        <TableHead class="text-right">cost/month</TableHead>
      </TableRow>
    </TableHeader>
    <TableBody>
      <TableEmpty v-if="entries.length === 0" :colspan="11" class="text-muted-foreground">
        none
      </TableEmpty>
      <TableRow
        v-for="entry in entries"
        :key="entry.id"
        :class="entry.storeDown ? 'bg-destructive/10' : undefined"
      >
        <TableCell class="font-medium">{{ entry.id }}</TableCell>
        <TableCell class="text-muted-foreground">{{ secondaryValue(entry) }}</TableCell>
        <TableCell class="text-muted-foreground">{{ formatLimits(entry.limits) }}</TableCell>
        <TableCell class="text-right tabular-nums">
          {{ entry.storeDown ? '?' : entry.requestsPerMinute }}
        </TableCell>
        <TableCell class="text-right tabular-nums">
          {{ entry.storeDown ? '?' : entry.requestsPerDay }}
        </TableCell>
        <TableCell class="text-right tabular-nums">
          {{ entry.storeDown ? '?' : entry.tokensInPerDay }}
        </TableCell>
        <TableCell class="text-right tabular-nums">
          {{ entry.storeDown ? '?' : entry.tokensOutPerDay }}
        </TableCell>
        <TableCell class="text-right tabular-nums">
          {{ entry.storeDown ? '?' : entry.tokensInPerMonth }}
        </TableCell>
        <TableCell class="text-right tabular-nums">
          {{ entry.storeDown ? '?' : entry.tokensOutPerMonth }}
        </TableCell>
        <TableCell class="text-right tabular-nums">
          {{ entry.storeDown ? '?' : formatCost(entry.costPerDayMicroUsd) }}
        </TableCell>
        <TableCell class="text-right tabular-nums">
          {{ entry.storeDown ? '?' : formatCost(entry.costPerMonthMicroUsd) }}
        </TableCell>
      </TableRow>
    </TableBody>
  </Table>
</template>
