<script setup lang="ts">
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'

/**
 * SkeletonTable is a placeholder shaped exactly like DataTable.vue's own
 * rendered table — the same Table/TableRow/TableHead/TableCell
 * primitives, `rows` x `cols` pulsing cells — so a table view swapping
 * this in for its first load causes no layout shift once real rows
 * arrive. `cols` defaults to 4, the panel's most common table width;
 * callers with a narrower or wider table pass their own column count.
 */
withDefaults(defineProps<{ rows?: number; cols?: number }>(), { rows: 5, cols: 4 })
</script>

<template>
  <Table>
    <TableHeader>
      <TableRow>
        <TableHead v-for="c in cols" :key="c">
          <Skeleton class="h-4 w-16" />
        </TableHead>
      </TableRow>
    </TableHeader>
    <TableBody>
      <TableRow v-for="r in rows" :key="r">
        <TableCell v-for="c in cols" :key="c">
          <Skeleton class="h-4 w-full max-w-32" />
        </TableCell>
      </TableRow>
    </TableBody>
  </Table>
</template>
