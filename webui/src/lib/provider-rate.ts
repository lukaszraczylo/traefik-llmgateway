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
 * rate as a 0-1 fraction, or null when attempts is 0 or negative — the
 * "no traffic yet" case a caller must render as its own state, never as a
 * fraction (a 0/0 division would otherwise read as NaN, or worse, as a
 * misleading 100%).
 */
export function providerSuccessRate(attempts: number, failures: number): number | null {
  if (attempts <= 0) return null
  return (attempts - failures) / attempts
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

/** formatRatePercent renders a 0-1 fraction as a rounded whole-percent string, e.g. 0.994 -> "99%", 1 -> "100%". */
export function formatRatePercent(rate: number): string {
  return `${Math.round(rate * 100)}%`
}

/**
 * minuteRateTitle builds the badge's title attribute — the "right now"
 * minute-window detail the spec asks for beside the day-window badge text
 * itself, e.g. "12 attempts, 1 failure in the last minute (92%)". Singular/
 * plural wording follows the actual count, and a zero-attempt minute reads
 * as its own sentence rather than a "0%" that would misread as a total
 * outage.
 */
export function minuteRateTitle(attemptsMinute: number, failuresMinute: number): string {
  if (attemptsMinute <= 0) return 'no traffic in the last minute'
  const rate = providerSuccessRate(attemptsMinute, failuresMinute)
  const pct = rate === null ? '' : ` (${formatRatePercent(rate)})`
  const attemptWord = attemptsMinute === 1 ? 'attempt' : 'attempts'
  const failureWord = failuresMinute === 1 ? 'failure' : 'failures'
  return `${attemptsMinute} ${attemptWord}, ${failuresMinute} ${failureWord} in the last minute${pct}`
}
