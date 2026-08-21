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
