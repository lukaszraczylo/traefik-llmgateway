// F1 (usage-vs-limit bars): pure ratio math for the Consumers page's
// per-scope budget bars (UsageBar.vue), kept out of the component the
// same way lib/provider-rate.ts keeps ProviderHealthPanel.vue's own
// success-rate math out of it — vitest's node-environment config
// (vite.config.ts's `test` block) exercises it directly, no component
// mount required.
import type { AdminUsageEntryView } from '@/types/api'

/** BAR_AMBER_THRESHOLD is the used/limit ratio at and above which a bar reads amber ("approaching the limit") — below it, a bar reads its normal (healthy) tier. */
export const BAR_AMBER_THRESHOLD = 0.8

/** BAR_RED_THRESHOLD is the used/limit ratio at and above which a bar reads red ("at or over the limit") — a scope can exceed 1.0 (its usage window rolled forward past the limit before the next request was rejected), so this is a floor, not a ceiling. */
export const BAR_RED_THRESHOLD = 1.0

/** RatioTier is UsageBar.vue's own color tier, derived purely from a used/limit ratio — 'normal' below BAR_AMBER_THRESHOLD, 'amber' from there up to BAR_RED_THRESHOLD, 'red' at or above it. */
export type RatioTier = 'normal' | 'amber' | 'red'

/** ratioTier classifies a used/limit ratio into UsageBar.vue's three color tiers. A negative or NaN ratio (should not happen — every caller here derives ratio from non-negative usage/limit numbers) reads as 'normal', the safest default. */
export function ratioTier(ratio: number): RatioTier {
  if (!Number.isFinite(ratio) || ratio < BAR_AMBER_THRESHOLD) return 'normal'
  if (ratio < BAR_RED_THRESHOLD) return 'amber'
  return 'red'
}

/**
 * BudgetRatio is one configured limit's current used/limit reading for a
 * scope — `id` names WHICH limit ("reqMin", "reqDay", "tokDay", "tokMonth",
 * "costDay", "costMonth", matching usage-columns.ts's own column ids for
 * the identical underlying figure), `used`/`limit` are the raw comparable
 * numbers (requests, tokens, or micro-USD — never mixed units within one
 * BudgetRatio), and `ratio` is `used / limit`.
 */
export interface BudgetRatio {
  id: string
  used: number
  limit: number
  ratio: number
}

/**
 * usdToMicros / microsToUsd (reuse-audit.md F8): moved to lib/format.ts
 * (formatCost's own home — an "odd home" here, the audit's words, for
 * conversions every cost-limit needs), re-exported here so every existing
 * `from '@/lib/usage-bars'` import site (lib/forecast.ts, lib/kpi.ts, lib/
 * usage-csv.ts, lib/scope.ts, stores/spend.ts, pages/HomePage.vue,
 * components/UserDetail.vue) keeps working unchanged. P11 review fix
 * (DRY) history: the underlying MICROS_PER_USD constant used to be
 * declared independently three times (here, lib/forecast.ts, lib/usage-
 * columns.ts) before the first consolidation onto this file; F8 is the
 * second consolidation, onto lib/format.ts. MICROS_PER_USD itself is not
 * re-exported here — nothing imports it from this module, only from lib/
 * format.ts directly.
 */
export { usdToMicros, microsToUsd } from '@/lib/format'
import { formatCost, formatExactInt, usdToMicros } from '@/lib/format'

/**
 * BUDGET_RATIO_LABEL (P10) names each BudgetRatio id in accurate,
 * accessible text — shared by ConsumerDirectory.vue's group-detail-grid
 * bars, usage-columns.ts's own per-column bars, and UserDetail.vue's
 * headroom bars (UsageBar's `label` prop, its aria-label). tokDay/
 * tokMonth spell out "(in+out)" explicitly: the
 * underlying ratio combines both directions (see budgetRatios' own doc
 * comment below), and unlike a sighted reader — who sees the separate
 * tokIn/tokOut columns beside it for context — a screen-reader user
 * hearing just "tokIn/day, 1,500 / 2,000" next to a cell showing 500 has
 * no other cue that the meter is measuring the COMBINED total, not that
 * column's own count.
 */
export const BUDGET_RATIO_LABEL: Record<string, string> = {
  reqMin: 'req/min',
  reqDay: 'req/day',
  tokDay: 'tokens (in+out)/day',
  tokMonth: 'tokens (in+out)/month',
  costDay: 'cost/day',
  costMonth: 'cost/month',
}

function ratioOf(id: string, used: number, limit: number): BudgetRatio {
  return { id, used, limit, ratio: used / limit }
}

/**
 * budgetRatios returns one BudgetRatio per limit the entry actually has
 * CONFIGURED (LimitsConfig's own "0/omitted means unlimited" convention —
 * formatLimits, lib/format.ts, applies the identical `if (limits.x)` gate).
 * A scope with no limits configured at all returns an empty array, and
 * UsageBar.vue simply renders nothing for it — no bar implies no limit,
 * never a fabricated 0%.
 *
 * Token limits combine tokensIn + tokensOut into one bar (coordinator
 * decision, dashboard-plan.md section 3: "tokens in+out") — LimitsConfig
 * enforces tokensPerDay/tokensPerMonth against the SUM of in+out
 * (llmgateway.go ground truth), so a bar split by direction would show two
 * numbers that individually stay under the limit while their sum has
 * already crossed it.
 *
 * A storeDown entry returns an empty array too — every counter on a
 * storeDown scope is stale/unknown (usage-columns.ts's own "?" masking
 * convention), so a bar computed from it would show a fabricated,
 * possibly wildly wrong ratio rather than the "unknown" state the rest of
 * this panel already renders for that case.
 */
export function budgetRatios(entry: AdminUsageEntryView): BudgetRatio[] {
  const limits = entry.limits
  if (!limits || entry.storeDown) return []
  const out: BudgetRatio[] = []
  if (limits.requestsPerMinute) out.push(ratioOf('reqMin', entry.requestsPerMinute, limits.requestsPerMinute))
  if (limits.requestsPerDay) out.push(ratioOf('reqDay', entry.requestsPerDay, limits.requestsPerDay))
  if (limits.tokensPerDay) {
    out.push(ratioOf('tokDay', entry.tokensInPerDay + entry.tokensOutPerDay, limits.tokensPerDay))
  }
  if (limits.tokensPerMonth) {
    out.push(ratioOf('tokMonth', entry.tokensInPerMonth + entry.tokensOutPerMonth, limits.tokensPerMonth))
  }
  // P11 review fix: the two USD->micro-USD limit paths used to disagree —
  // this used to gate on the RAW USD value being truthy, then round; a
  // configured limit below 0.5 micro-USD (e.g. costPerDayUSD: 0.0000001)
  // passed that truthy check but rounded to exactly 0 micro-USD, so
  // ratioOf divided by 0 (an Infinity or NaN ratio). usage-columns.ts's own
  // usdLimitMicros rounded FIRST, then checked the ROUNDED result for
  // truthy — matching Go's own convention (limits.go/metrics.go: `limit <=
  // 0` skips the check entirely). Rounding first here too, then gating on
  // the rounded value, makes both paths agree with Go: a limit that rounds
  // to 0 micro-USD is skipped, exactly like an unconfigured one.
  if (limits.costPerDayUSD) {
    const limitMicros = usdToMicros(limits.costPerDayUSD)
    if (limitMicros > 0) out.push(ratioOf('costDay', entry.costPerDayMicroUsd, limitMicros))
  }
  if (limits.costPerMonthUSD) {
    const limitMicros = usdToMicros(limits.costPerMonthUSD)
    if (limitMicros > 0) out.push(ratioOf('costMonth', entry.costPerMonthMicroUsd, limitMicros))
  }
  return out
}

// estimatedRunOutDate was removed (P2 item 14, DRY): UserDetail.vue and
// BurnDownChart.vue now both call lib/burndown.ts's own runOutDate, the
// ONE run-out projection in this codebase — it used to disagree with this
// one on an already-exceeded budget (null here vs `now` there) and on day
// 1 of the month (clamped/overstated here vs null there).

/**
 * budgetValueText (reuse-audit.md F7) renders one BudgetRatio's "used /
 * limit" detail text (UsageBar's aria-valuetext/title) — a cost-kind ratio
 * (id starting "cost") formats both sides through formatCost (dollars),
 * every other kind (requests, tokens) through formatExactInt (a plain
 * thousands-grouped integer) — the ONE definition, replacing
 * ConsumerDirectory.vue's and UserDetail.vue's own two copies (which had
 * drifted onto two different signatures for the identical logic).
 */
export function budgetValueText(ratio: BudgetRatio): string {
  if (ratio.id.startsWith('cost')) return `${formatCost(ratio.used)} / ${formatCost(ratio.limit)}`
  return `${formatExactInt(ratio.used)} / ${formatExactInt(ratio.limit)}`
}
