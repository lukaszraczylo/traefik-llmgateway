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
 * success-rate breakdown. Day window only, no attemptsMinute/
 * failuresMinute (SHOULD-2, v0.22 review round): a dashboard with N
 * configured models was paying N wasted minute-window store reads on
 * every 5s poll for a figure that only ever fed a badge's title text,
 * never its own displayed tier. See lib/provider-rate.ts's dayRateTitle
 * for the fallback title text a per-model badge renders instead.
 */
export interface AdminModelRateView {
  attemptsDay: number
  failuresDay: number
}

/**
 * One upstream model's resolved metadata (admin.go: adminModelMetaView —
 * feature v0.23: context window, per-token cost). Every field is
 * optional/undefined, not a plain 0 — undefined means "unknown", any
 * present value (including 0, a free model's real cost) means "known".
 * Never infer "unknown" from a value being falsy; check for undefined.
 */
export interface AdminModelMetaView {
  contextTokens?: number
  inputPerMTokUsd?: number
  outputPerMTokUsd?: number
}

/**
 * One provider's one stream-state's compact latency summary (admin.go:
 * adminLatencyView — feat: instrument upstream latency), sourced from the
 * SAME in-process g.latency accumulator /metrics reads, never Redis —
 * PER-REPLICA, not fleet-wide (see AdminProviderView.latency's own doc
 * comment). Every field is optional/undefined, not a plain 0: each is
 * `omitempty` on the Go side, so undefined means "no observations for
 * this field", never a fabricated zero. avgTtfbMs in particular must
 * never be treated as interchangeable with avgDurationMs — see
 * AdminProviderView.latency's doc comment for why they diverge for
 * non-streaming traffic.
 */
export interface AdminLatencyView {
  avgTtfbMs?: number
  avgDurationMs?: number
  count?: number
}

/**
 * One provider's one non-reported provenance kind's compact usage-
 * accounting summary (admin.go: adminProvenanceView — feat: expose
 * token-accounting provenance). "estimated" means a non-streaming
 * response carried no usage from the provider, so prompt was substituted
 * with an estimate (ceil(request body length / 4)) and completion billed
 * as zero. "unbilled" means a streaming response carried no usage, so
 * the request was counted but zero tokens were billed — a provider
 * silently ignoring stream_options.include_usage serves that completion
 * entirely free against every budget. Billing itself is unaffected by
 * this data; it only reports how an already-billed number was arrived
 * at. requests/tokens are never fabricated zeros: this view only ever
 * exists (see AdminProviderView.provenance) for a kind that was actually
 * observed at least once.
 */
export interface AdminProvenanceView {
  requests: number
  tokens: number
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
   * Resolved metadata (context window, per-token cost — feature v0.23)
   * for every id in `models`, keyed by upstream model id. Always
   * present (possibly with every sub-field undefined), never omitted —
   * mirroring modelRates' own "never nil" convention below.
   */
  modelMeta: Record<string, AdminModelMetaView>
  /**
   * discoveryEnabled mirrors the provider's own configured Discovery flag
   * (admin.go: adminProviderView.DiscoveryEnabled — Feature B, v0.22): the
   * Providers tab uses it to tell "discovery is off" apart from "discovery
   * is on but has not refreshed yet", both of which otherwise show the same
   * zero lastRefresh.
   */
  discoveryEnabled: boolean
  /**
   * healthState is the provider's DISCOVERY circuit breaker state
   * (admin.go: adminProviderView.HealthState — feat/provider-health):
   * 'closed' (normal), 'open' (backing off after repeated discovery
   * failures — openUntil below is when it next probes), or 'half-open'
   * (a probe is in flight, deciding whether to close again). Reflects
   * the discovery endpoint ONLY, not request-path health — a provider
   * whose model listing 401s while its actual completion endpoint works
   * fine still reads 'open' here. A provider with discovery disabled, or
   * one that has never failed a refresh, always reads 'closed'.
   */
  healthState: 'closed' | 'open' | 'half-open'
  /**
   * openUntil is when this provider's breaker will next attempt a
   * half-open probe (admin.go: adminProviderView.OpenUntil). The unset
   * zero-time sentinel (formatAgo/formatTimestamp's ZERO_TIME) when
   * healthState is not 'open'.
   */
  openUntil: string
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
  /**
   * latency is this provider's compact per-stream average TTFB/duration
   * summary (admin.go: adminProviderView.Latency — feat: instrument
   * upstream latency), keyed by "streaming"/"non-streaming" — the same
   * two string keys buildAdminLatencyViews (admin.go) emits, mirrored
   * here as a literal union rather than a bare Record<string, ...> so a
   * caller cannot accidentally index a third, nonexistent stream state.
   * Undefined entirely for a provider with no observations yet, or a
   * deployment with metrics collection gated off — undefined, never an
   * empty object, so "no data" is never confused with "data says zero".
   *
   * PER-REPLICA, IN-PROCESS: sourced from the same in-process
   * accumulator the rate-limit rejection counters read, never Redis.
   * This deployment runs multiple Traefik replicas, so any single poll
   * reflects only whichever replica answered it, and a freshly restarted
   * pod shows a short observation window — never render this as if it
   * were a fleet-wide average.
   *
   * STREAMING VS NON-STREAMING ARE NOT COMPARABLE: avgTtfbMs is the
   * load-sensitive figure, and only for stream === 'streaming' — a
   * non-streaming provider buffers its whole completion before sending
   * anything, so its own avgTtfbMs is approximately equal to its own
   * avgDurationMs and carries the identical output-length contamination
   * (a long completion legitimately takes longer than a short one on an
   * equally healthy provider). Never average the two stream states
   * together or present either one as a bare, unlabeled "latency".
   */
  latency?: Partial<Record<'streaming' | 'non-streaming', AdminLatencyView>>
  /**
   * provenance summarizes non-reported usage accounting for this
   * provider (admin.go: adminProviderView.Provenance — feat: expose
   * token-accounting provenance), keyed by provenance kind: 'estimated'
   * or 'unbilled' only — 'reported' (the default, healthy case) is never
   * a key here, see AdminProvenanceView's own doc comment for why.
   * Undefined entirely for a provider with no estimated/unbilled outcome
   * yet, or a deployment with metrics collection gated off — mirroring
   * latency's identical "undefined, never an empty object" convention
   * above.
   *
   * PER-REPLICA, IN-PROCESS: sourced from the same in-process accumulator
   * as latency above — see that field's own caveat, which applies here
   * unchanged.
   */
  provenance?: Partial<Record<'estimated' | 'unbilled', AdminProvenanceView>>
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
  /**
   * The alias's own resolved metadata (admin.go: adminAliasView.
   * ModelMeta — feature v0.23): INHERITED from its target unless the
   * alias itself carries its own modelMeta override. Always present
   * (possibly with every sub-field undefined).
   */
  modelMeta: AdminModelMetaView
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

/**
 * One ranked model in GET /admin/api/usage/models (admin.go:
 * adminUsageModelEntryView). `id` is the canonical "provider/model" — the
 * provider that actually SERVED the traffic, which is why a failover
 * target and its primary appear as two separate rows.
 */
export interface AdminUsageModelEntry {
  id: string
  value: number
}

/**
 * GET /admin/api/usage/models (admin.go: adminUsageModelsResponse) — the
 * model ranking behind the Charts view's "Models" tab, and the source of
 * the scope picker's model list.
 *
 * ONLY MODELS WITH NON-ZERO USAGE ARE RETURNED (server-side, by operator
 * requirement): the catalog runs to hundreds of models, so neither the
 * ranking nor the picker may pad itself with idle ones. An empty `models`
 * array therefore means "nothing used in this window", never "no models
 * configured" — and a failed store read is a 503, never an empty array.
 */
export interface AdminUsageModelsResponse {
  metric: string
  window: string
  models: AdminUsageModelEntry[]
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
