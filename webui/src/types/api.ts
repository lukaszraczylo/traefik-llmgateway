// Shared response shapes for the three /admin/api/* routes (admin.go). Kept
// as one module so every store/component imports the identical types rather
// than re-declaring them (vue.md: "share types once, import everywhere").

/** Mirrors Go's LimitsConfig (llmgateway.go) — every field is 0/omitted when unlimited. */
export interface LimitsConfig {
  requestsPerMinute?: number
  requestsPerDay?: number
  tokensPerDay?: number
  tokensPerMonth?: number
  costPerDayUSD?: number
  costPerMonthUSD?: number
}

/**
 * One upstream model's current-window attempt/failure counters within its
 * provider (admin.go: adminModelRateView) — Feature A, v0.22 per-model
 * success-rate breakdown. Field shape matches AdminProviderView's own
 * attempts/failures counters below, just scoped to one model.
 */
export interface AdminModelRateView {
  attemptsDay: number
  failuresDay: number
  attemptsMinute: number
  failuresMinute: number
}

export interface AdminProviderView {
  name: string
  type: string
  baseUrl: string
  lastRefresh: string
  lastErr?: string
  /** Sorted explicit∪discovered model ids (admin.go: adminProviderView.Models) — always an array, never omitted, even when empty. */
  models: string[]
  modelCount: number
  /**
   * discoveryEnabled mirrors the provider's own configured Discovery flag
   * (admin.go: adminProviderView.DiscoveryEnabled — Feature B, v0.22): the
   * Providers tab uses it to tell "discovery is off" apart from "discovery
   * is on but has not refreshed yet", both of which otherwise show the same
   * zero lastRefresh.
   */
  discoveryEnabled: boolean
  /**
   * attemptsDay/failuresDay/attemptsMinute/failuresMinute are this
   * provider's current-window upstream-attempt counters (admin.go:
   * adminProviderView's identically-named fields — Feature A, v0.22). See
   * lib/provider-rate.ts for the success-rate math and badge-tier
   * thresholds built on top of these.
   */
  attemptsDay: number
  failuresDay: number
  attemptsMinute: number
  failuresMinute: number
  /** Per-model breakdown of the counters above, keyed by upstream model id — always present (possibly empty), never omitted. */
  modelRates: Record<string, AdminModelRateView>
}

export interface AdminRedisView {
  lastErrAt: string
  lastErr?: string
  configured: boolean
}

export interface AdminCacheView {
  ttl?: string
  enabled: boolean
}

export interface AdminRetryView {
  backoff?: string
  enabled: boolean
  attempts?: number
}

export interface AdminGroupView {
  limits?: LimitsConfig
  name: string
  memberCount: number
}

export interface AdminAliasView {
  alias: string
  target: string
}

/** GET /admin/api/overview (admin.go: adminOverviewResponse). */
export interface AdminOverviewResponse {
  version: string
  providers: AdminProviderView[]
  groups: AdminGroupView[]
  aliases: AdminAliasView[]
  redis: AdminRedisView
  cache: AdminCacheView
  retry: AdminRetryView
}

export interface AdminUsageEntryView {
  limits?: LimitsConfig
  kind: string
  id: string
  groupName?: string
  /**
   * The group's configured access lists (admin.go: adminUsageEntryView's
   * Providers/Models/MCPServers/Agents) — set only on a group entry (kind
   * === 'group'), never on a user or the total entry, mirroring groupName's
   * own user-only convention above. Undefined/empty means unrestricted
   * (GroupConfig's own empty-means-all semantics, llmgateway.go) — this is
   * the group's configured glob list exactly as written, never expanded to
   * the full provider/model catalog.
   */
  providers?: string[]
  models?: string[]
  mcpServers?: string[]
  agents?: string[]
  requestsPerMinute: number
  requestsPerDay: number
  tokensInPerDay: number
  tokensOutPerDay: number
  tokensInPerMonth: number
  tokensOutPerMonth: number
  costPerDayMicroUsd: number
  costPerMonthMicroUsd: number
  storeDown?: boolean
}

/** GET /admin/api/usage (admin.go: adminUsageResponse). */
export interface AdminUsageResponse {
  users: AdminUsageEntryView[]
  groups: AdminUsageEntryView[]
  total: AdminUsageEntryView
}

/** The four metric values GET /admin/api/usage/history accepts. */
export type HistoryMetric = 'req' | 'tokin' | 'tokout' | 'cost'

/** The three window values GET /admin/api/usage/history accepts. */
export type HistoryWindow = 'hour' | 'day' | 'month'

export interface UsageHistoryPoint {
  bucket: string
  value: number
}

/** GET /admin/api/usage/history (admin.go: usageHistoryResponse). */
export interface UsageHistoryResponse {
  scope: string
  metric: string
  window: string
  points: UsageHistoryPoint[]
}

/** One MCP-server or agent target's current-window request counters (admin.go: adminTargetCountersView) — requests only, no tokens or cost. */
export interface AdminTargetCountersView {
  requestsPerMinute: number
  requestsPerDay: number
  requestsPerMonth: number
}

/**
 * One configured MCP server's or A2A agent's row in GET /admin/api/targets
 * (admin.go: adminTargetView). Access is the group names actually allowed
 * to reach this target, computed server-side via the same matching authz
 * enforcement uses — undefined/empty means every configured group can
 * reach it ("empty meaning all", mirroring AdminUsageEntryView's own
 * providers/models/mcpServers/agents convention above).
 */
export interface AdminTargetView {
  name: string
  url: string
  access?: string[]
  counters: AdminTargetCountersView
}

/** GET /admin/api/targets (admin.go: adminTargetsResponse). */
export interface AdminTargetsResponse {
  mcpServers: AdminTargetView[]
  agents: AdminTargetView[]
}
