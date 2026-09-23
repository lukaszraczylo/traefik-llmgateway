<script setup lang="ts">
import { computed } from 'vue'

import AccessMatrix from '@/components/AccessMatrix.vue'
import ConsumerDirectory from '@/components/ConsumerDirectory.vue'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import UserDetail from '@/components/UserDetail.vue'
import { parseMatrixKind } from '@/lib/access-matrix'
import type { MatrixKind } from '@/lib/access-matrix'
import { useNavStore } from '@/stores/nav'

/**
 * ConsumersPage (redesign-plan.md section 3.4) is the Consumers page's own
 * router: `user=<name>` (set by an EntityLink kind="user" click anywhere in
 * the panel) opens UserDetail; otherwise `view=directory|matrix` (default
 * directory) switches between ConsumerDirectory and AccessMatrix, and
 * (P4 three-view split) `matrix=models|mcp|agents` (default models) picks
 * AccessMatrix's own column-group tab. All three params live in nav.params
 * (stores/nav.ts's own opaque per-page record), never a private local ref —
 * a bookmarked/shared #consumers?user=... link must restore the same view.
 */
const nav = useNavStore()

const userParam = computed(() => nav.params.user)
const view = computed<'directory' | 'matrix'>(() => (nav.params.view === 'matrix' ? 'matrix' : 'directory'))
const matrixKind = computed<MatrixKind>(() => parseMatrixKind(nav.params.matrix))

function setView(next: unknown): void {
  if (next !== 'directory' && next !== 'matrix') return
  const { user: _user, ...rest } = nav.params
  nav.goTo('consumers', { ...rest, view: next })
}

/**
 * setMatrixKind writes the AccessMatrix Tabs selection into nav.params:
 * omit `matrix` when 'models'; `view` is always written.
 */
function setMatrixKind(next: MatrixKind): void {
  const { user: _user, matrix: _matrix, ...rest } = nav.params
  if (next === 'models') {
    nav.goTo('consumers', { ...rest, view: 'matrix' })
    return
  }
  nav.goTo('consumers', { ...rest, view: 'matrix', matrix: next })
}

function closeUserDetail(): void {
  const { user: _user, ...rest } = nav.params
  nav.goTo('consumers', rest)
}
</script>

<template>
  <UserDetail v-if="userParam" :user-id="userParam" @close="closeUserDetail" />
  <div v-else class="flex flex-col gap-4">
    <Tabs :model-value="view" @update:model-value="setView">
      <TabsList>
        <TabsTrigger value="directory">Directory</TabsTrigger>
        <TabsTrigger value="matrix">Access matrix</TabsTrigger>
      </TabsList>
    </Tabs>
    <ConsumerDirectory v-if="view === 'directory'" />
    <AccessMatrix v-else :kind="matrixKind" @update:kind="setMatrixKind" />
  </div>
</template>
