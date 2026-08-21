<script setup lang="ts">
import { faKey } from '@fortawesome/free-solid-svg-icons'
import { ref } from 'vue'

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { useAuthStore } from '@/stores/auth'

const auth = useAuthStore()
const keyInput = ref('')

function submit(): void {
  const value = keyInput.value
  keyInput.value = ''
  auth.submit(value)
}
</script>

<template>
  <div class="mx-auto flex min-h-[70vh] max-w-md flex-col justify-center px-4">
    <Card>
      <CardHeader>
        <CardTitle class="flex items-center gap-2">
          <FontAwesomeIcon :icon="faKey" class="size-4 text-primary" />
          Admin key required
        </CardTitle>
        <CardDescription>
          Enter an admin API key. The browser keeps it only in this tab's session storage: it is
          never written to disk and is sent only to this page's own /admin/api/* requests.
        </CardDescription>
      </CardHeader>
      <CardContent class="flex flex-col gap-4">
        <form class="flex flex-col gap-3" @submit.prevent="submit">
          <Input
            v-model="keyInput"
            type="password"
            autocomplete="off"
            placeholder="API key"
            aria-label="Admin API key"
          />
          <Button type="submit" class="w-full">Continue</Button>
        </form>
        <Alert v-if="auth.error" variant="destructive">
          <AlertTitle>Authentication failed</AlertTitle>
          <AlertDescription>{{ auth.error }}</AlertDescription>
        </Alert>
      </CardContent>
    </Card>
  </div>
</template>
