<script setup lang="ts">
import { faArrowRotateRight, faTriangleExclamation } from '@fortawesome/free-solid-svg-icons'

import { Button } from '@/components/ui/button'

function reload(): void {
  window.location.reload()
}
</script>

<template>
  <div role="alert" class="flex flex-col items-center gap-2 px-4 py-16 text-center">
    <FontAwesomeIcon :icon="faTriangleExclamation" class="size-6 text-destructive" aria-hidden="true" />
    <p class="text-sm font-medium text-destructive">This page failed to load.</p>
    <p class="max-w-md text-sm text-muted-foreground">The gateway may have been updated since this tab opened. Reload to fetch the current version.</p>
    <Button type="button" variant="outline" size="sm" class="mt-1" @click="reload">
      <FontAwesomeIcon :icon="faArrowRotateRight" class="size-3.5" aria-hidden="true" />
      Reload
    </Button>
  </div>
</template>
