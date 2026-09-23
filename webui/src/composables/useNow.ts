import { ref, type Ref } from 'vue'

import { createVisibilityPoller, type VisibilityPoller } from '@/lib/polling'

/**
 * How often the shared clock ticks — coarse enough that a cost-forecast
 * projection or elapsed-month fraction (lib/forecast.ts) is never
 * meaningfully stale, without re-deriving the Usage tab's whole column set
 * once a second for no visible benefit.
 */
const NOW_TICK_MS = 60_000

export interface UseNow {
  /** The shared clock — every time-dependent read on the Consumers page (CostForecastCard's elapsedFraction, usageColumns()'s own `now` parameter, ConsumerDirectory.vue's groupCostProjection, UserDetail.vue's runOutDate) reads THIS ref, never its own `new Date()`. */
  now: Ref<Date>
  /** Idempotent, mirrors createVisibilityPoller's own start() — call from the owning component's onMounted. */
  start: () => void
  /** Idempotent, mirrors createVisibilityPoller's own stop() — call from the owning component's onUnmounted so the interval is cleaned up on navigation away. */
  stop: () => void
}

/**
 * useNow (P1) is the ONE reactive clock every time-dependent read across
 * the admin UI shares (today: SpendPage.vue's cost forecast, UserDetail.vue's
 * run-out date, ConsumerDirectory.vue's group cost projection — see this
 * file's own UseNow doc comment for the full current-caller list), instead
 * of each freezing its own `new Date()` at mount or re-deriving a fresh one
 * on every render:
 *
 * - CostForecastCard.vue used `withDefaults(..., { now: () => new Date() })`
 *   — Vue caches a prop's default once per component instance
 *   (propsDefaults), so a long-open tab's projection froze at whatever
 *   instant the card happened to mount, never advancing again.
 * - usage-columns.ts's usageColumns() takes `now` as a plain parameter,
 *   evaluated once per call — UsageTable.vue's `computed(() =>
 *   usageColumns(...))` and the pre-redesign Usage tab's own headless
 *   Groups toolbar columns (UsageView.vue, deleted) both called it with no
 *   reactive dependency on the current time, so neither ever recomputed
 *   either.
 * - The pre-redesign Usage tab's own groupCostProjection (UsageView.vue,
 *   deleted) called `new Date()` fresh on every invocation instead, so the
 *   group grid could disagree with the above two on what "now" even was,
 *   mid-render.
 *
 * Driven by the shared createVisibilityPoller (lib/polling.ts — the same
 * primitive every store's own poll timer uses): ticks once immediately on
 * start(), then every NOW_TICK_MS while the tab is visible, pausing while
 * hidden and catching up the moment it is looked at again (and, per P13,
 * never ticking at all if started while already hidden).
 *
 * This composable does NOT call onMounted/onUnmounted itself — each of its
 * three current callers (SpendPage.vue, UserDetail.vue,
 * ConsumerDirectory.vue) owns its OWN clock instance's lifecycle
 * explicitly via the returned start/stop (`onMounted(startClock)` /
 * `onUnmounted(stopClock)`), the same reason lib/polling.ts itself takes
 * no lifecycle-hook dependency: it keeps this composable callable, and
 * testable, outside a mounted component.
 */
export function useNow(intervalMs = NOW_TICK_MS): UseNow {
  const now = ref(new Date())
  let poller: VisibilityPoller | undefined

  function start(): void {
    if (poller) return
    poller = createVisibilityPoller({
      intervalMs,
      tick: () => {
        now.value = new Date()
      },
    })
    poller.start()
  }

  function stop(): void {
    poller?.stop()
    poller = undefined
  }

  return { now, start, stop }
}
