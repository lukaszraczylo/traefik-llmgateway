<script setup lang="ts">
import { computed } from 'vue'

import EmptyState from '@/components/EmptyState.vue'
import EntityLink from '@/components/EntityLink.vue'
import ErrorState from '@/components/ErrorState.vue'
import SkeletonList from '@/components/SkeletonList.vue'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { loadState } from '@/lib/load-state'

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
 * than a plain unlinked list. The optional `controls` slot renders below
 * the title (e.g. HomePage.vue's top-users Cost/Requests/Tokens Tabs
 * switch, lib/top-users.ts) — omitted by callers with no mode switch of
 * their own, like the plain top-models card, so this stays one shared
 * component rather than a near-duplicate per card.
 *
 * States (states-plan.md item 1/3, verify-ui-states.md #1/#3/#4,
 * verify-ui-states-2.md #2) are decided via lib/load-state.ts's own
 * loadState off `loading`/`error`/`loaded` — NOT a bare
 * `loading && items.length === 0` check, and NOT `items.length > 0`
 * either: `hasData` here means "a fetch has completed for the CURRENT
 * selection" (the `loaded` prop, e.g. HomePage.vue's own per-mode
 * topUsersCache presence), not "there happen to be rows right now". A
 * caller's genuinely empty, already-settled selection must keep reading
 * as empty across a background poll's own `loading` flips (skeleton is
 * for a selection's FIRST fetch only) — using `items.length` for
 * `hasData` used to flip a settled EmptyState back to a skeleton on
 * every poll tick for exactly that case. Once `state` is not 'skeleton'
 * or 'error', the template below decides EmptyState vs. the real list by
 * checking `items.length` directly, the same way UserDetail.vue's own
 * "Models used" table does.
 */
const props = withDefaults(
  defineProps<{
    title: string
    items: TopListItem[]
    /** Shown in place of the list when `items` is empty (and not loading, not erroring) — e.g. "No traffic in this range." */
    emptyText: string
    loading?: boolean
    /**
     * True once a fetch has completed for the CURRENT selection (mode/
     * range), even if it resolved with zero items — the caller's own
     * per-selection flag (verify-ui-states-2.md #2), reset whenever the
     * caller's selection genuinely changes. Distinct from `items.length >
     * 0`: see this component's own States doc comment above.
     */
    loaded?: boolean
    /** The current fetch's error message, '' (default) when there is none — mirrors lib/load-state.ts's own LoadStateInput.error. A background refresh's failure with `items` already populated never reaches ErrorState (loadState's 'ready' precedence) — stores/toasts.ts covers that case instead. */
    error?: string
    /** Renders a "Retry" button on ErrorState — the caller's own refetch action. */
    onRetry?: () => void | Promise<void>
    /**
     * A distinct, higher-precedence notice (e.g. HomePage.vue's top-users
     * "too many configured users for this range", lib/top-users.ts's
     * topUsersCapExceeded) that takes over the card's content entirely —
     * it names a KNOWN reason no data will load until the reader acts
     * (narrow the range), not a transient loading/error/empty state, so it
     * is checked before all three.
     */
    notice?: string
    noticeDescription?: string
  }>(),
  { loading: false, loaded: false, error: '' },
)

const state = computed(() => loadState({ loading: props.loading, hasData: props.loaded, error: props.error ?? '' }))
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle>{{ title }}</CardTitle>
      <slot name="controls" />
    </CardHeader>
    <CardContent>
      <EmptyState v-if="notice" :title="notice" :description="noticeDescription" />
      <SkeletonList v-else-if="state === 'skeleton'" :rows="5" />
      <ErrorState v-else-if="state === 'error'" :message="error ?? ''" :on-retry="onRetry" />
      <EmptyState v-else-if="items.length === 0" :title="emptyText">
        <slot name="empty-action" />
      </EmptyState>
      <ol v-else class="flex flex-col gap-1.5">
        <li v-for="(item, i) in items" :key="item.id" class="flex items-center justify-between gap-3 text-sm">
          <span class="flex min-w-0 items-center gap-2">
            <span class="w-4 shrink-0 text-right text-xs text-muted-foreground">{{ i + 1 }}</span>
            <EntityLink :label="item.label" :kind="item.kind" :id="item.id" class="min-w-0" label-class="truncate" />
          </span>
          <span class="shrink-0 font-medium tabular-nums">{{ item.value }}</span>
        </li>
      </ol>
    </CardContent>
  </Card>
</template>
