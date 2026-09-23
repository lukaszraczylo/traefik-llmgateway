// Pure math behind the Home page's KpiTile.vue tiles (redesign-plan.md
// section 3.4) — kept out of the component/page the same way lib/
// provider-rate.ts and lib/usage-bars.ts keep their own tier math out of
// their components, so vitest's node-environment config exercises it
// directly, no component mount required. Deliberately reuses those two
// modules' existing thresholds (BAR_AMBER_THRESHOLD/BAR_RED_THRESHOLD,
// PROVIDER_RATE_AMBER_THRESHOLD/PROVIDER_RATE_DESTRUCTIVE_THRESHOLD)
// rather than declaring a third, independent set of budget/error-rate
// boundaries for the same two underlying concepts (a used/limit ratio,
// a success rate) the rest of this panel already colors consistently.
import { providerRateStatus, providerSuccessRate } from '@/lib/provider-rate'
import { MICROS_PER_USD, ratioTier } from '@/lib/usage-bars'
import type { AdminGroupView, AdminProviderView } from '@/types/api'

/** KpiTier is KpiTile.vue's own three-color scale — 'ok' (normal), 'warn' (approaching a limit / minor degradation), 'critical' (over a limit / severe degradation). */
export type KpiTier = 'ok' | 'warn' | 'critical'

/**
 * sumGroupBudgetMicros sums every configured group's costPerMonthUSD
 * limit, converted to micro-USD (Q2, redesign-plan.md DECISIONS: "Home
 * budget baseline = sum of group costPerMonthUSD"). A group with no
 * costPerMonthUSD configured (0 or undefined) contributes nothing — the
 * caller reads a 0 total as "no budget set" (budgetRatio below), never a
 * genuine zero-dollar budget.
 */
export function sumGroupBudgetMicros(groups: AdminGroupView[]): number {
  let total = 0
  for (const group of groups) {
    const limitUsd = group.limits?.costPerMonthUSD
    if (limitUsd && limitUsd > 0) total += Math.round(limitUsd * MICROS_PER_USD)
  }
  return total
}

/** budgetRatio is `spentMicros / budgetMicros`, or null when budgetMicros is 0 or negative ("no budget set" — Q2) — the fleet has nothing configured to compare month-to-date spend against. */
export function budgetRatio(spentMicros: number, budgetMicros: number): number | null {
  if (budgetMicros <= 0) return null
  return spentMicros / budgetMicros
}

/** budgetTier classifies a budgetRatio result into the Home spend tile's KpiTier, reusing lib/usage-bars.ts's own ratioTier thresholds (0.8 amber, 1.0 red) so a budget tile and a per-scope UsageBar agree on what "approaching the limit" means. A null ratio (no budget set) reads as 'ok' — there is nothing to warn about. */
export function budgetTier(ratio: number | null): KpiTier {
  if (ratio === null) return 'ok'
  switch (ratioTier(ratio)) {
    case 'normal':
      return 'ok'
    case 'amber':
      return 'warn'
    case 'red':
      return 'critical'
  }
}

/** openBreakerCount counts providers whose discovery circuit breaker is not 'closed' (AdminProviderView.healthState) — the Home "open breakers" tile's own count, per-replica like the field it reads (see AdminProviderView.healthState's own doc comment). */
export function openBreakerCount(providers: AdminProviderView[]): number {
  return providers.filter((p) => p.healthState !== 'closed').length
}

/** FleetErrorRate is the Home error-rate tile's fleet-wide reading: summed minute-window attempts/failures across every provider, and the resulting rate. */
export interface FleetErrorRate {
  /** failures/attempts as a 0-1 fraction, or null when there were no attempts in the last minute (never a fabricated 0%). */
  rate: number | null
  attempts: number
  failures: number
}

/** fleetErrorRate sums every provider's attemptsMinute/failuresMinute (AdminProviderView — the SAME per-replica, in-process minute counters ProviderRateBadge.vue's own minuteRateTitle reads) into one fleet-wide error rate. */
export function fleetErrorRate(providers: AdminProviderView[]): FleetErrorRate {
  let attempts = 0
  let failures = 0
  for (const p of providers) {
    attempts += p.attemptsMinute
    failures += p.failuresMinute
  }
  const success = providerSuccessRate(attempts, failures)
  return { rate: success === null ? null : 1 - success, attempts, failures }
}

/** errorRateTier classifies a fleetErrorRate() result into the Home error-rate tile's KpiTier, reusing lib/provider-rate.ts's own success-rate thresholds (99% amber, 90% red) inverted for an error-rate reading — the same boundaries the Providers tab's own badges already use. */
export function errorRateTier(rate: number | null): KpiTier {
  if (rate === null) return 'ok'
  switch (providerRateStatus(1 - rate)) {
    case 'no-traffic':
    case 'healthy':
      return 'ok'
    case 'degraded':
      return 'warn'
    case 'severe':
      return 'critical'
  }
}
