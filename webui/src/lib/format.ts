import type { HistoryWindow, LimitsConfig } from '@/types/api'

const ZERO_TIME = '0001-01-01T00:00:00Z'

/**
 * routableModelId joins a provider name and one of its own model ids into
 * the id form the gateway actually routes on (routes_unified.go's
 * "provider/model" prefix rule) — always `${providerName}/${modelId}`,
 * even when modelId itself already contains a slash (a discovered id
 * like "uni/deepseek-v4-flash-0731" on provider "real" becomes
 * "real/uni/deepseek-v4-flash-0731": the FIRST path segment names the
 * provider, everything after it is the model id verbatim, so this stays
 * copy-paste-valid regardless of how many slashes the model id itself
 * carries).
 */
export function routableModelId(providerName: string, modelId: string): string {
  return `${providerName}/${modelId}`
}

/** formatCost renders a micro-USD integer (adminUsageEntryView's convention) as "$1.2345". */
export function formatCost(micros: number): string {
  return `$${(micros / 1_000_000).toFixed(4)}`
}

/** formatExactInt renders a raw integer count with thousands separators ("1,234,567") — the precise value CompactNumber.vue and the usage chart's tooltip carry alongside a compact rendering, never lost. */
export function formatExactInt(n: number): string {
  return n.toLocaleString('en-US')
}

/**
 * formatCompactCount renders a raw token/request count as a compact,
 * SI-style label: below 10,000 the exact integer with thousands
 * separators ("9,999"); at or above that, three significant figures
 * with a k/M/B suffix ("12.3k", "456k", "1.23M", "45.6M", "1.23B"),
 * trailing zeros trimmed. Every digit beyond the third significant one
 * is FLOORED, never rounded — the same honesty rule formatRatePercent
 * already applies to the success-rate badge (lib/provider-rate.ts): a
 * count of 999,950 must never display as "1,000k", which would imply a
 * boundary the raw value has not actually crossed. This is DISPLAY
 * only — every caller pairs it with the exact value via a title/
 * aria-label (CompactNumber.vue's own doc comment), and no accessor/sort
 * key ever calls this; DataTable columns sort on the raw number.
 */
export function formatCompactCount(n: number): string {
  const abs = Math.abs(n)
  if (abs < 10_000) return formatExactInt(n)

  const sign = n < 0 ? '-' : ''
  const [divisor, suffix] = abs < 1_000_000 ? [1_000, 'k'] : abs < 1_000_000_000 ? [1_000_000, 'M'] : [1_000_000_000, 'B']
  const scaled = abs / divisor

  // Three significant figures total: the floored integer part's own
  // digit count (1, 2, or 3) decides how many decimal places are left.
  const intDigits = Math.floor(scaled) >= 100 ? 3 : Math.floor(scaled) >= 10 ? 2 : 1
  const decimals = 3 - intDigits

  const factor = 10 ** decimals
  const floored = Math.floor(scaled * factor) / factor

  let text = floored.toFixed(decimals)
  if (decimals > 0) {
    text = text.replace(/0+$/, '').replace(/\.$/, '')
  }
  return `${sign}${text}${suffix}`
}

/**
 * formatContextWindow renders a token count as a compact "256k"-style
 * label (feature v0.23's context_window/AdminModelMetaView.contextTokens
 * unit — plain token count, not bytes) — binary-K (÷1024), matching the
 * task brief's own worked example (262144 tokens -> "256k"). Plain
 * digits, no suffix, for anything under 1024: a context window that
 * small is rare (LM Studio's smallest observed embedding models run
 * 512-8192) but must still render as a real number, not "0k".
 */
export function formatContextWindow(tokens: number): string {
  if (tokens < 1024) return `${tokens}`
  return `${Math.round(tokens / 1024)}k`
}

/** formatUsdPerMTok renders a USD-per-million-tokens float as "$0.19" (2 decimals) — the shared building block for both the visible cost chip and ModelChip's hover detail. */
function formatUsdPerMTok(usd: number): string {
  return `$${usd.toFixed(2)}`
}

/**
 * formatModelCostHover renders ModelChip's hover-detail cost phrase
 * (feature v0.23, hover-detail refinement): "in $0.19 / out $0.51 per
 * MTok" for a priced model, or "free" when both sides are exactly 0 (a
 * KNOWN zero cost, ModelMetaConfig.Free — distinct from the caller never
 * passing a cost at all, which ModelChip's own metaDetail computed
 * handles by omitting the phrase entirely rather than calling this).
 */
export function formatModelCostHover(inputUsd: number, outputUsd: number): string {
  if (inputUsd === 0 && outputUsd === 0) return 'free'
  return `in ${formatUsdPerMTok(inputUsd)} / out ${formatUsdPerMTok(outputUsd)} per MTok`
}

/** formatAgo renders an ISO timestamp as "(Ns ago)", or "" for the unset zero-time sentinel. */
export function formatAgo(iso: string | undefined): string {
  if (!iso || iso === ZERO_TIME) return ''
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return ''
  const secs = Math.max(0, Math.round((Date.now() - then) / 1000))
  return ` (${secs}s ago)`
}

/** formatTimestamp renders an ISO timestamp for display, or "never" for the unset zero-time sentinel. */
export function formatTimestamp(iso: string | undefined): string {
  if (!iso || iso === ZERO_TIME) return 'never'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString()
}

/**
 * refreshState is the Providers tab's three-state refresh classification
 * (Feature B, v0.22 — last-refresh label honesty), shared by refreshLabel
 * (the accordion trigger's compact text) and refreshDetailLabel (the
 * accordion content's "Last refresh" detail row — folded review minor,
 * v0.22 review round: that row still read raw formatTimestamp before this
 * existed, so a discovery-off provider's expanded detail said "never"
 * right next to a trigger that correctly said "discovery off"):
 * discovery disabled reads 'off' regardless of lastRefresh (a disabled
 * provider's lastRefresh is always the unset sentinel, registry.go's own
 * maybeRefresh/warmFill never call finishRefresh for it — but this checks
 * discoveryEnabled first anyway, not lastRefresh's value, so it stays
 * correct even if that invariant ever changes); discovery enabled but
 * never yet refreshed reads 'pending'; otherwise 'refreshed'.
 */
function refreshState(discoveryEnabled: boolean, lastRefresh: string | undefined): 'off' | 'pending' | 'refreshed' {
  if (!discoveryEnabled) return 'off'
  return formatAgo(lastRefresh) ? 'refreshed' : 'pending'
}

/**
 * refreshLabel renders refreshState as the Providers tab's compact
 * trigger-row text: "discovery off", "pending", or the existing
 * "refreshed (Ns ago)" relative-time text.
 */
export function refreshLabel(discoveryEnabled: boolean, lastRefresh: string | undefined): string {
  switch (refreshState(discoveryEnabled, lastRefresh)) {
    case 'off':
      return 'discovery off'
    case 'pending':
      return 'pending'
    case 'refreshed':
      return `refreshed${formatAgo(lastRefresh)}`
  }
}

/**
 * refreshDetailLabel renders refreshState as the Providers tab's
 * accordion-content "Last refresh" detail row: "discovery off", "pending",
 * or the full formatTimestamp date/time (this row's own pre-existing
 * convention — refreshLabel's relative "(Ns ago)" belongs on the compact
 * trigger row only).
 */
export function refreshDetailLabel(discoveryEnabled: boolean, lastRefresh: string | undefined): string {
  switch (refreshState(discoveryEnabled, lastRefresh)) {
    case 'off':
      return 'discovery off'
    case 'pending':
      return 'pending'
    case 'refreshed':
      return formatTimestamp(lastRefresh)
  }
}

/** formatLimits renders a LimitsConfig as a short comma-joined summary, or "none" when unset. */
export function formatLimits(limits: LimitsConfig | undefined): string {
  if (!limits) return 'none'
  const parts: string[] = []
  if (limits.requestsPerMinute) parts.push(`req/min ${limits.requestsPerMinute}`)
  if (limits.requestsPerDay) parts.push(`req/day ${limits.requestsPerDay}`)
  if (limits.tokensPerDay) parts.push(`tok/day ${limits.tokensPerDay}`)
  if (limits.tokensPerMonth) parts.push(`tok/month ${limits.tokensPerMonth}`)
  if (limits.costPerDayUSD) parts.push(`cost/day $${limits.costPerDayUSD}`)
  if (limits.costPerMonthUSD) parts.push(`cost/month $${limits.costPerMonthUSD}`)
  return parts.length ? parts.join(', ') : 'none'
}

/**
 * historyBucketFormat maps each history window to the fixed-width bucket
 * key format serveAdminUsageHistory echoes back (limits.go's windowKey:
 * hour="2006010215", day="20060102", month="200601" — Go reference-time
 * layouts, all digits, no separators).
 */
export function formatBucketLabel(bucket: string, window: HistoryWindow): string {
  switch (window) {
    case 'hour': {
      const hour = bucket.slice(8, 10)
      return `${hour}:00`
    }
    case 'day': {
      const year = Number(bucket.slice(0, 4))
      const month = Number(bucket.slice(4, 6))
      const day = Number(bucket.slice(6, 8))
      return new Date(year, month - 1, day).toLocaleDateString(undefined, {
        month: 'short',
        day: 'numeric',
      })
    }
    case 'month': {
      const year = Number(bucket.slice(0, 4))
      const month = Number(bucket.slice(4, 6))
      return new Date(year, month - 1, 1).toLocaleDateString(undefined, {
        month: 'short',
        year: 'numeric',
      })
    }
  }
}
