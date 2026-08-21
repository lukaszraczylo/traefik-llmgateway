<script setup lang="ts">
import { faCheck, faEye, faEyeSlash, faKey } from '@fortawesome/free-solid-svg-icons'
import { computed, nextTick, onMounted, ref, watch } from 'vue'

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

function focusKeyInput(): void {
  void nextTick(() => keyInputEl.value?.$el?.focus())
}

onMounted(focusKeyInput)

// Explicit refocus on a failed key: auth.submit() optimistically sets
// apiKey before the server has validated it, which flips isAuthenticated
// and makes App.vue unmount AuthGate; the ensuing 401 calls auth.reject()
// (apiKey='', error=message), isAuthenticated flips back, and AuthGate
// remounts — re-running onMounted's focus above as a side effect. That
// chain is real and correct today, but silent: if optimistic auth is ever
// removed, refocus would stop working with no signal. Watching auth.error
// directly makes refocus explicit and no longer dependent on the remount.
watch(() => auth.error, (err) => {
  if (err) focusKeyInput()
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
          <span class="text-xs font-medium text-muted-foreground">LLM Gateway</span>
          <CardTitle class="text-lg font-semibold">Admin key required</CardTitle>
        </div>
        <CardDescription>Enter an admin API key to continue.</CardDescription>
      </CardHeader>
      <CardContent class="flex flex-col gap-4">
        <h2 id="authgate-security-heading" class="sr-only">
          How this key is handled
        </h2>
        <ul aria-labelledby="authgate-security-heading" class="flex flex-col gap-1.5 text-xs text-muted-foreground">
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
        <form class="flex flex-col gap-3" @submit.prevent="submit">
          <div class="relative">
            <!--
              authgate-shake retriggers via the AuthGate remount on a
              failed submit (see the refocus watcher in <script> for the
              exact chain) — a v-if unmount/remount recreates this
              element, which replays the CSS animation. If that remount
              chain ever changes, the shake stops retriggering silently
              along with it (left as-is per review — the mechanism itself
              is verified correct today).
            -->
            <Input
              ref="keyInputEl"
              v-model="keyInput"
              :type="showKey ? 'text' : 'password'"
              autocomplete="off"
              placeholder="API key"
              aria-label="Admin API key"
              :aria-invalid="!!auth.error"
              :aria-describedby="auth.error ? 'authgate-error' : undefined"
              class="h-10 pr-10"
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
        <Alert v-if="auth.error" id="authgate-error" variant="destructive">
          <AlertTitle>Authentication failed</AlertTitle>
          <AlertDescription>{{ auth.error }}</AlertDescription>
        </Alert>
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
