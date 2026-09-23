import { defineStore } from 'pinia'

/** ToastKind is one toast's visual/urgency tier — 'error' gets the longer default timeout (TOAST_TIMEOUT_MS) and an assertive aria-live region in ToastViewport.vue; 'success'/'info' share the shorter default and a polite region. */
export type ToastKind = 'success' | 'error' | 'info'

/** One toast entry ToastViewport.vue renders — `id` is this store's own monotonic counter, never reused, so a dismiss/timer always targets the exact entry it was scheduled for even after other toasts have come and gone. */
export interface Toast {
  id: number
  kind: ToastKind
  message: string
  timeoutMs: number
}

/** TOAST_TIMEOUT_MS is each kind's default auto-dismiss delay (states-plan.md item 3: "auto-dismiss default 3s success/info, 6s error") — a caller may override per-push via the optional `timeoutMs` argument. */
export const TOAST_TIMEOUT_MS: Record<ToastKind, number> = {
  success: 3000,
  info: 3000,
  error: 6000,
}

/** MAX_VISIBLE_TOASTS caps how many toasts ToastViewport.vue shows at once (states-plan.md item 3: "max 3 visible") — pushing a 4th silently drops the OLDEST (FIFO), matching a notification stack's usual behavior: the newest, most relevant toast always stays. */
export const MAX_VISIBLE_TOASTS = 3

export interface PushToastInput {
  kind: ToastKind
  message: string
  /** Overrides TOAST_TIMEOUT_MS[kind] for this one toast. */
  timeoutMs?: number
}

/**
 * useToastsStore is the panel's one in-house toast queue (states-plan.md
 * item 3) — copy-to-clipboard, CSV export, snippet copy, API key
 * generation, and background-refresh-failure callers all push through
 * this single store; ToastViewport.vue (mounted once in App.vue) is the
 * only reader. Kept deliberately tiny — no external toast library, this
 * panel's whole toast surface is "a short queue with auto-dismiss
 * timers" — rather than pulling in a dependency for it.
 */
export const useToastsStore = defineStore('toasts', {
  state: () => ({
    toasts: [] as Toast[],
    nextId: 1,
    /** timers is keyed by toast id, not held on the Toast object itself — a setTimeout handle is not serializable/comparable state a component template should ever touch. */
    timers: {} as Record<number, ReturnType<typeof setTimeout>>,
  }),
  actions: {
    /**
     * push enqueues one toast, deduping it against the MOST RECENTLY
     * pushed toast only (states-plan.md item 3: "dedupe identical
     * consecutive messages") — an identical kind+message pushed again
     * later, after other toasts have appeared in between, is NOT a
     * duplicate of the earlier one and is shown again (e.g. the same
     * background-refresh-failure message recurring after a brief
     * recovery is worth re-surfacing, not silently swallowed forever).
     */
    push({ kind, message, timeoutMs }: PushToastInput): void {
      const mostRecent = this.toasts[this.toasts.length - 1]
      if (mostRecent && mostRecent.kind === kind && mostRecent.message === message) return

      const id = this.nextId++
      const toast: Toast = { id, kind, message, timeoutMs: timeoutMs ?? TOAST_TIMEOUT_MS[kind] }
      this.toasts.push(toast)

      if (this.toasts.length > MAX_VISIBLE_TOASTS) {
        const dropped = this.toasts.shift()
        if (dropped) this.clearTimer(dropped.id)
      }

      this.timers[id] = setTimeout(() => this.dismiss(id), toast.timeoutMs)
    },
    /** dismiss removes one toast by id (the Retry/close button's own click handler, or a timer firing) and clears its pending auto-dismiss timer, if any. */
    dismiss(id: number): void {
      this.clearTimer(id)
      this.toasts = this.toasts.filter((t) => t.id !== id)
    },
    /** clearTimer cancels and forgets one toast's pending setTimeout — shared by dismiss() and push()'s own over-capacity eviction, so a dropped or dismissed toast never fires its timer against an id no longer in `toasts`. */
    clearTimer(id: number): void {
      const timer = this.timers[id]
      if (timer === undefined) return
      clearTimeout(timer)
      delete this.timers[id]
    },
  },
})
