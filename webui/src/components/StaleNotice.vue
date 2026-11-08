<script setup lang="ts">
import { faTriangleExclamation } from '@fortawesome/free-solid-svg-icons'

import { Alert, AlertDescription } from '@/components/ui/alert'

defineProps<{ message: string | null }>()
</script>

<template>
  <Alert v-if="message" variant="warn" role="status">
    <FontAwesomeIcon :icon="faTriangleExclamation" aria-hidden="true" />
    <AlertDescription>{{ message }}</AlertDescription>
  </Alert>
</template>
