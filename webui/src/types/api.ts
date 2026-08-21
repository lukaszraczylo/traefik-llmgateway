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

export interface AdminProviderView {
  name: string
  type: string
  baseUrl: string
  lastRefresh: string
  lastErr?: string
  modelCount: number
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
