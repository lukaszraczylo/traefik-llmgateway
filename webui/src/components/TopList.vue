<script setup lang="ts">
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import EntityLink from '@/components/EntityLink.vue'

/** One TopList.vue row — an entity id/label, its formatted value, and which EntityLink.vue target it opens. */
export interface TopListItem {
  /** The raw entity id EntityLink.vue navigates on (a user id or a canonical model id). */
  id: string
  /** The visible label — usually the same as `id`, but callers may pass a friendlier form. */
  label: string
  /** Already-formatted display value (lib/format.ts) — this component renders it verbatim, never reformats. */
  value: string
  kind: 'user' | 'model'
}

/**
 * TopList (redesign-plan.md section 3.4) is the Home page's ranked-list
 * card: "top 5 users by cost", "top 5 models". Each row is an EntityLink
 * so clicking a user opens Consumers pre-filtered to them, a model opens
 * Models — the SAME "jump to detail" affordance every table's id column
 * (usage-columns.ts, model-table-columns.ts) offers, reused here rather
 * than a plain unlinked list.
 */
defineProps<{
  title: string
  items: TopListItem[]
  /** Shown in place of the list when `items` is empty — e.g. "no traffic in this range yet". */
  emptyText: string
}>()
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>{{ title }}</CardTitle>
    </CardHeader>
    <CardContent>
      <p v-if="items.length === 0" class="text-sm text-muted-foreground">{{ emptyText }}</p>
      <ol v-else class="flex flex-col gap-1.5">
        <li v-for="(item, i) in items" :key="item.id" class="flex items-center justify-between gap-3 text-sm">
          <span class="flex min-w-0 items-center gap-2">
            <span class="w-4 shrink-0 text-right text-xs text-muted-foreground">{{ i + 1 }}</span>
            <EntityLink :label="item.label" :kind="item.kind" :id="item.id" class="min-w-0 truncate" />
          </span>
          <span class="shrink-0 font-medium tabular-nums">{{ item.value }}</span>
        </li>
      </ol>
    </CardContent>
  </Card>
</template>
