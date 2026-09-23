// Consumers page (redesign-plan.md section 3.4): pure relative-time
// formatting for AdminUsageEntryView.lastSeen — a fleet-wide, throttled
// (60s/replica/scope) unix-seconds timestamp (types/api.ts's own doc
// comment). Kept out of usage-columns.ts/ConsumerDirectory.vue the same way
// every other pure display helper in this panel (lib/format.ts, lib/
// usage-bars.ts, lib/forecast.ts) is — vitest's node-environment config
// exercises it directly, no component mount or fake timers required (every
// function here takes `now` as an explicit argument, never reads Date.now()
// itself, matching lib/forecast.ts's own determinism convention).

const SECONDS_PER_MINUTE = 60
const SECONDS_PER_HOUR = 3600
const SECONDS_PER_DAY = 86400
/** MONTH_THRESHOLD_DAYS is where formatLastSeen switches from "Nd ago" to "Nmo ago" — 2 months' worth of days, so a scope last seen 6 weeks ago still reads as a legible day count rather than rounding down to "1mo ago" (a needlessly coarse figure for something that recent). */
const MONTH_THRESHOLD_DAYS = 60
const DAYS_PER_MONTH_APPROX = 30

/**
 * formatLastSeen renders a scope's last-seen unix-seconds timestamp as a
 * short relative-time label: "just now" under a minute, "Nm ago" under an
 * hour, "Nh ago" under a day, "Nd ago" under MONTH_THRESHOLD_DAYS, "Nmo
 * ago" beyond it. `undefined` or `0` means "never seen, or admin.stats.
 * lastSeen is off" (types/api.ts's own doc comment: "never render 0 as an
 * epoch date") — rendered as the literal "never", never "0s ago" or a
 * fabricated 1970 date. A timestamp in the future (clock skew between this
 * replica and the reader's own clock) clamps to "just now" rather than a
 * nonsensical negative duration.
 */
export function formatLastSeen(seconds: number | undefined, now: Date = new Date()): string {
  if (!seconds) return 'never'
  const nowSeconds = Math.floor(now.getTime() / 1000)
  const delta = Math.max(0, nowSeconds - seconds)
  if (delta < SECONDS_PER_MINUTE) return 'just now'
  if (delta < SECONDS_PER_HOUR) return `${Math.floor(delta / SECONDS_PER_MINUTE)}m ago`
  if (delta < SECONDS_PER_DAY) return `${Math.floor(delta / SECONDS_PER_HOUR)}h ago`
  const days = Math.floor(delta / SECONDS_PER_DAY)
  if (days < MONTH_THRESHOLD_DAYS) return `${days}d ago`
  return `${Math.floor(days / DAYS_PER_MONTH_APPROX)}mo ago`
}
