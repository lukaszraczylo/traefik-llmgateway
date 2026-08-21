<script setup lang="ts">
import { faCheck, faEye, faEyeSlash, faKey } from '@fortawesome/free-solid-svg-icons'
import { computed, nextTick, onMounted, ref } from 'vue'

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'

const auth = useAuthStore()
const dashboard = useDashboardStore()
const keyInput = ref('')
const showKey = ref(false)
const keyInputEl = ref<InstanceType<typeof Input> | null>(null)

/** Disable submit until there's non-whitespace to send — matches the store's
 * own `if (!key) return` guard in submit(), just surfaced to the button
 * instead of silently no-op'ing on click. */
const canSubmit = computed(() => keyInput.value.trim() !== '')

function submit(): void {
  const value = keyInput.value
  keyInput.value = ''
  auth.submit(value)
  // Fetch immediately rather than waiting for the next 5s poll tick
  // (dashboard.startPolling's own interval, already running from App.vue's
  // onMounted) — otherwise a freshly authenticated dashboard can sit blank
  // for up to 5s after key entry.
  void dashboard.refresh()
}

onMounted(() => {
  // Autofocus the key field — this is the only control on the gate screen,
  // so a keyboard/screen-reader user should land straight in it.
  void nextTick(() => keyInputEl.value?.$el?.focus())
})
</script>

<template>
  <div class="mx-auto flex min-h-[70vh] max-w-md flex-col justify-center px-4">
    <Card>
      <CardHeader class="flex flex-col items-center gap-3 text-center">
        <div class="flex size-12 items-center justify-center rounded-full bg-primary/10 text-primary">
          <FontAwesomeIcon :icon="faKey" class="size-5" aria-hidden="true" />
        </div>
        <div class="flex flex-col items-center gap-1">
          <span class="text-sm font-semibold text-foreground">LLM Gateway</span>
          <CardTitle>Admin key required</CardTitle>
        </div>
        <CardDescription>Enter an admin API key to continue.</CardDescription>
      </CardHeader>
      <CardContent class="flex flex-col gap-4">
        <form class="flex flex-col gap-3" @submit.prevent="submit">
          <div class="relative">
            <Input
              ref="keyInputEl"
              v-model="keyInput"
              :type="showKey ? 'text' : 'password'"
              autocomplete="off"
              placeholder="API key"
              aria-label="Admin API key"
              class="h-10 pr-10 text-base"
              :class="{ 'authgate-shake': auth.error }"
            />
            <Button
              type="button"
              variant="ghost"
              size="icon-xs"
              class="absolute top-1/2 right-1.5 -translate-y-1/2 text-muted-foreground hover:text-foreground"
              :aria-label="showKey ? 'Hide key' : 'Show key'"
              @click="showKey = !showKey"
            >
              <FontAwesomeIcon :icon="showKey ? faEyeSlash : faEye" class="size-3.5" aria-hidden="true" />
            </Button>
          </div>
          <Button type="submit" class="w-full" :disabled="!canSubmit">
            Continue
          </Button>
        </form>
        <Alert v-if="auth.error" variant="destructive">
          <AlertTitle>Authentication failed</AlertTitle>
          <AlertDescription>{{ auth.error }}</AlertDescription>
        </Alert>
        <ul class="flex flex-col gap-1.5 text-xs text-muted-foreground">
          <li class="flex items-start gap-1.5">
            <FontAwesomeIcon :icon="faCheck" class="mt-0.5 size-3 shrink-0" aria-hidden="true" />
            <span>Kept only in this tab's session storage</span>
          </li>
          <li class="flex items-start gap-1.5">
            <FontAwesomeIcon :icon="faCheck" class="mt-0.5 size-3 shrink-0" aria-hidden="true" />
            <span>Never written to disk</span>
          </li>
          <li class="flex items-start gap-1.5">
            <FontAwesomeIcon :icon="faCheck" class="mt-0.5 size-3 shrink-0" aria-hidden="true" />
            <span>Sent only to this page's own /admin/api/* requests</span>
          </li>
        </ul>
      </CardContent>
    </Card>
  </div>
</template>

<style scoped>
/*
 * First scoped <style> block in webui/src/components — no other component
 * needs one yet (grep confirms) but nothing in the Vite/@vitejs/plugin-vue
 * pipeline forbids it, and the alternative (Tailwind-only) has no shake
 * keyframe to compose from: Tailwind v4 core ships only pulse/bounce/spin/
 * ping (node_modules/tailwindcss/theme.css), and tw-animate-css adds
 * accordion/collapsible/enter/exit/caret-blink, none of which reads as
 * "rejected". Contained to this file per the task's file-scope constraint.
 *
 * Applied only while auth.error is set (a failed submit), not on every
 * render, so it communicates state rather than decorating the page.
 */
@keyframes authgate-shake {
  10%, 90% { transform: translateX(-1px); }
  20%, 80% { transform: translateX(2px); }
  30%, 50%, 70% { transform: translateX(-4px); }
  40%, 60% { transform: translateX(4px); }
}

@media (prefers-reduced-motion: no-preference) {
  .authgate-shake {
    animation: authgate-shake 0.4s ease-in-out;
  }
}
</style>
