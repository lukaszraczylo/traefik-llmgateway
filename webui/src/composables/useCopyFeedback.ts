import { onUnmounted, ref, type Ref } from 'vue'

/** RESET_MS is how long a copy-feedback state (`copied`/`selected`/`failed`) stays visible before reverting to `idle` — SnippetBlock.vue's and ModelChip.vue's own identical 1500ms. */
const RESET_MS = 1500

/** CopyFeedbackState mirrors lib/clipboard.ts's copyText own return union, plus the resting `idle` state neither SnippetBlock.vue nor ModelChip.vue had a name for before this composable. */
export type CopyFeedbackState = 'idle' | 'copied' | 'selected' | 'failed'

export interface UseCopyFeedback {
  /** The current feedback state — drives a caller's own icon/label swap. */
  state: Ref<CopyFeedbackState>
  /** Records a copyText() outcome and schedules the reset-to-idle timer, clearing any still-pending one first (a rapid double-click restarts the window rather than stacking two timers). */
  set: (result: 'copied' | 'selected' | 'failed') => void
}

/**
 * useCopyFeedback (reuse-audit.md F10) is the ONE "show a transient copy
 * outcome, then revert to idle after 1500ms" composable SnippetBlock.vue
 * and ModelChip.vue used to each hand-roll — the same `state` ref/
 * `resetTimer`/`clearTimeout`-on-unmount triple, with the identical
 * 1500ms magic number declared independently in both. Calls
 * `onUnmounted` itself (this composable is only ever invoked from a
 * component's own `setup()`/`<script setup>`, same as every other
 * composable in this panel), so a caller no longer needs its own cleanup
 * hook just for this timer.
 */
export function useCopyFeedback(): UseCopyFeedback {
  const state = ref<CopyFeedbackState>('idle')
  let resetTimer: ReturnType<typeof setTimeout> | undefined

  function set(result: 'copied' | 'selected' | 'failed'): void {
    state.value = result
    clearTimeout(resetTimer)
    resetTimer = setTimeout(() => {
      state.value = 'idle'
    }, RESET_MS)
  }

  onUnmounted(() => clearTimeout(resetTimer))

  return { state, set }
}
