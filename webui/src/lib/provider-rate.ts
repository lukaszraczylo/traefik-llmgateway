// Feature A (v0.22): per-provider / per-(provider,model) success-rate math
// for the Providers tab's badges (ProviderRateBadge.vue). Kept as pure
// functions in lib/, not inline in the component — the same "share once,
// import everywhere" discipline lib/format.ts and lib/search-expand.ts
// already apply, and it lets vitest's node-environment config (vite.config.
// ts's `test` block) exercise the threshold math directly, with no
// component mount required.

/**
 * PROVIDER_RATE_AMBER_THRESHOLD is the day-window success-rate fraction
 * (0-1) at and above which a provider or model reads as healthy ("muted"
 * badge, spec: "100% muted"). Below it, the badge turns amber
 * ("degraded", spec: "<99% amber") down to PROVIDER_RATE_DESTRUCTIVE_
 * THRESHOLD.
 */
export const PROVIDER_RATE_AMBER_THRESHOLD = 0.99

/**
 * PROVIDER_RATE_DESTRUCTIVE_THRESHOLD is the day-window success-rate
 * fraction below which the badge turns destructive (spec: "<90%
 * destructive") — a provider or model failing more than 1 in 10 attempts
 * today.
 */
export const PROVIDER_RATE_DESTRUCTIVE_THRESHOLD = 0.9

/**
 * ProviderRateStatus is the Providers tab's badge tier a success rate maps
 * to: 'no-traffic' (attempts === 0 — never a misleadingly perfect 100%),
 * 'healthy' (>= PROVIDER_RATE_AMBER_THRESHOLD, rendered muted), 'degraded'
 * (below the amber threshold, at or above the destructive one), or
 * 'severe' (below PROVIDER_RATE_DESTRUCTIVE_THRESHOLD).
 */
export type ProviderRateStatus = 'no-traffic' | 'healthy' | 'degraded' | 'severe'

/**
 * providerSuccessRate computes the (attempts-failures)/attempts success
 * rate as a 0-1 fraction, or null when attempts is 0, negative, or not a
 * finite number — the "no traffic yet" case a caller must render as its
 * own state, never as a fraction (a 0/0 division would otherwise read as
 * NaN, or worse, as a misleading 100%). failures is defensively treated
 * as 0 when it is not a finite number (folded review minor, v0.22 review
 * round): every real caller sources both counters from the same
 * admin.go response, so this only guards a version-skew or malformed-
 * response case, not a path normal operation reaches.
 */
export function providerSuccessRate(attempts: number, failures: number): number | null {
  if (!Number.isFinite(attempts) || attempts <= 0) return null
  const safeFailures = Number.isFinite(failures) ? failures : 0
  return (attempts - safeFailures) / attempts
}

/**
 * providerRateStatus classifies a providerSuccessRate result into the
 * Providers tab's badge tiers (see ProviderRateStatus). A null rate (no
 * traffic) always maps to 'no-traffic', regardless of the threshold
 * constants.
 */
export function providerRateStatus(rate: number | null): ProviderRateStatus {
  if (rate === null) return 'no-traffic'
  if (rate < PROVIDER_RATE_DESTRUCTIVE_THRESHOLD) return 'severe'
  if (rate < PROVIDER_RATE_AMBER_THRESHOLD) return 'degraded'
  return 'healthy'
}

/**
 * isModelDegraded reports whether a model's OWN day-window rate should
 * show a badge beside its ModelChip at all — the spec's "only when that
 * model is degraded (<99% w/ attempts>0)" rule: a healthy or no-traffic
 * model gets no badge, so the accordion is not cluttered with a badge
 * beside every single model.
 */
export function isModelDegraded(attemptsDay: number, failuresDay: number): boolean {
  const rate = providerSuccessRate(attemptsDay, failuresDay)
  return rate !== null && rate < PROVIDER_RATE_AMBER_THRESHOLD
}

/**
 * formatRatePercent renders a 0-1 fraction as a whole-percent string,
 * FLOORED rather than rounded (folded review minor, v0.22 review round):
 * only a true 100% rate — zero failures — ever renders "100%"; a rate
 * like 0.996 (unrounded, sub-1% failure) renders "99%", not a
 * round-tripped "100%" that would read as flawless when it is not quite.
 * A rate at or above 1 (the only way to reach exactly 100%, given
 * providerSuccessRate's own (attempts-failures)/attempts shape) is
 * special-cased rather than left to Math.floor's own rounding, purely for
 * clarity at the boundary.
 */
export function formatRatePercent(rate: number): string {
  if (rate >= 1) return '100%'
  return `${Math.floor(rate * 100)}%`
}

/**
 * providerErrorRate is failures/attempts directly — conceptually
 * providerSuccessRate's complement, but computed as its OWN division
 * rather than `1 - providerSuccessRate(...)`: chaining a subtraction onto
 * an already-divided value compounds binary floating-point error (e.g.
 * 1 - (99/100) computes a hair above 0.01, which then ceils to "2%"
 * instead of "1%" once formatErrorRatePercent multiplies by 100 — a
 * caught review bug, not a hypothetical one). Returns null (never a
 * fabricated 0%) when there is no traffic to compute a rate from at all.
 * Shared by the Models page's per-model error-rate column
 * (lib/model-table-columns.ts) and the Reliability page's own bucket-by-
 * bucket series (which instead derives its ratio via lib/series.ts's
 * ratioSeries, but formats it through formatErrorRatePercent below) — the
 * ONE place "attempts/failures -> error rate" is computed for a single
 * snapshot value, as opposed to a time series.
 */
export function providerErrorRate(attempts: number, failures: number): number | null {
  if (!Number.isFinite(attempts) || attempts <= 0) return null
  const safeFailures = Number.isFinite(failures) ? failures : 0
  return safeFailures / attempts
}

/**
 * formatErrorRatePercent renders a 0-1(+) error-rate fraction CEILING
 * rather than floored — the opposite honesty direction from
 * formatRatePercent above (a SUCCESS rate must never overstate itself as
 * 100%; an ERROR rate must never round down to a misleadingly clean 0%).
 * `null` (no traffic to compute a rate from — providerErrorRate's own
 * "nothing to report" case) renders as `nullLabel` (default "no data"),
 * never "0%" — reuse-audit.md F13: the `nullLabel` param lets HomePage.vue's
 * fleet-wide reading pass "no traffic" instead of hand-rolling an identical
 * copy of this function with a different null wording. Not clamped to
 * 100%: a rate above 1 should not occur with real counters, but this
 * function reports whatever the caller computed rather than silently
 * capping it (lib/series.ts's own ratioSeries makes the identical choice).
 */
export function formatErrorRatePercent(rate: number | null, nullLabel = 'no data'): string {
  if (rate === null) return nullLabel
  if (rate <= 0) return '0%'
  return `${Math.ceil(rate * 100)}%`
}

/** countWord returns singular when n is exactly 1, plural otherwise — shared by minuteRateTitle and dayRateTitle so their wording never drifts apart. */
function countWord(n: number, singular: string, plural: string): string {
  return n === 1 ? singular : plural
}

/**
 * minuteRateTitle builds the provider-level badge's title/aria-label —
 * the "right now" minute-window detail the spec asks for beside the
 * day-window badge text itself, e.g. "12 attempts, 1 failure in the last
 * minute (91%)" (formatRatePercent floors, not rounds — SHOULD-B, v0.22
 * review round, round 2: this example previously said "(92%)", stale
 * against the floor fix below it in this same file). Singular/plural
 * wording follows the actual count, and a zero-attempt minute reads as
 * its own sentence rather than a "0%" that would misread as a total
 * outage. Provider-level only — a per-model badge no longer has minute-
 * window counters to build this from at all (SHOULD-2, v0.22 review
 * round: see dayRateTitle, its own fallback).
 */
export function minuteRateTitle(attemptsMinute: number, failuresMinute: number): string {
  if (attemptsMinute <= 0) return 'no traffic in the last minute'
  const rate = providerSuccessRate(attemptsMinute, failuresMinute)
  const pct = rate === null ? '' : ` (${formatRatePercent(rate)})`
  return `${attemptsMinute} ${countWord(attemptsMinute, 'attempt', 'attempts')}, ${failuresMinute} ${countWord(failuresMinute, 'failure', 'failures')} in the last minute${pct}`
}

/**
 * dayRateTitle is minuteRateTitle's day-window equivalent — the per-model
 * badge's title/aria-label (SHOULD-2, v0.22 review round): a model
 * scope's minute-window counters are no longer fetched anywhere
 * (admin.go's buildAdminOverview, limits.go's providerCounterKeys), so a
 * per-model badge cannot build minuteRateTitle's "in the last minute"
 * sentence — this is the "today" equivalent, built from the same day
 * counters the badge's own visible percent already uses.
 */
export function dayRateTitle(attemptsDay: number, failuresDay: number): string {
  if (attemptsDay <= 0) return 'no traffic today'
  const rate = providerSuccessRate(attemptsDay, failuresDay)
  const pct = rate === null ? '' : ` (${formatRatePercent(rate)})`
  return `${attemptsDay} ${countWord(attemptsDay, 'attempt', 'attempts')}, ${failuresDay} ${countWord(failuresDay, 'failure', 'failures')} today${pct}`
}
