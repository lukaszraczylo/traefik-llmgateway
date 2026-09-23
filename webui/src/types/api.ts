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

/** The three discovery circuit-breaker states AdminProviderView.healthState and AdminCatalogProvider.healthState both carry (see AdminProviderView.healthState's own doc comment). Named so both interfaces reference the same union instead of two copies drifting apart. */
export type ProviderHealthState = 'closed' | 'open' | 'half-open'

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
  healthState: ProviderHealthState
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

/**
 * Which admin-stats features this deployment has opt-in enabled
 * (admin.go: adminFeaturesView, AdminConfig.Stats — llmgateway.go). Every
 * new-endpoint store/page gates its own fetch and empty-state copy on
 * these rather than guessing from data shape: a false userModelStats, for
 * instance, means GET /admin/api/usage/totals?kind=usermodel 404s, not
 * that it happens to return no rows yet.
 */
export interface AdminFeaturesView {
  userModelStats: boolean
  latencyStats: boolean
  cacheStats: boolean
  lastSeen: boolean
  failover: boolean
}

/** Model-count breakdown by billing price source across the catalog (admin.go: adminPricingSummaryView) — the Home page's "N unpriced models" warning reads `unpriced` directly. */
export interface AdminPricingSummary {
  override: number
  builtin: number
  litellm: number
  free: number
  unpriced: number
}

/** GET /admin/api/overview (admin.go: adminOverviewResponse). */
export interface AdminOverviewResponse {
  version: string
  /**
   * replica identifies WHICH of this deployment's Traefik replicas
   * answered this poll (admin.go: os.Hostname(), falling back to
   * $HOSTNAME then "unknown"). Every per-replica, in-process figure this
   * panel already renders (provider latency, provenance, discovery
   * health — see AdminProviderView.latency's own doc comment) has always
   * been silently replica-scoped; this field is the first place that
   * scoping becomes visible to the reader, rather than a caveat they
   * have to take on faith.
   */
  replica: string
  /** instance is the gateway's own configured name (g.name), distinct from replica — the deployment's logical identity vs. which pod happened to answer. */
  instance: string
  /**
   * warnings holds the configuration warnings this replica logged while
   * the middleware was being constructed (llmgateway.go's
   * collectingWarnings window) — runtime warnings are not kept. Never nil;
   * an empty array when there is nothing to report.
   */
  warnings: string[]
  /** warningsDropped counts warnings beyond the server's own cap (configWarningsCap) that were logged but not kept — omitted entirely when nothing was dropped. */
  warningsDropped?: number
  providers: AdminProviderView[]
  groups: AdminGroupView[]
  aliases: AdminAliasView[]
  redis: AdminRedisView
  cache: AdminCacheView
  retry: AdminRetryView
  features: AdminFeaturesView
  pricing: AdminPricingSummary
}

export interface AdminUsageEntryView {
  limits?: LimitsConfig
  kind: string
  id: string
  groupName?: string
  /**
   * The user's full member group list (multi-group support, admin.go's
   * adminUsageEntryView.Groups) — set only on a user entry, same "user-only"
   * convention groupName above already follows. groupName keeps the FIRST
   * group for back-compat; groups is the complete membership. Every
   * group-membership check (search matching, the members table, the Group
   * column) must read `entry.groups ?? [entry.groupName]`, never groupName
   * alone — a multi-group user otherwise silently drops out of every group
   * but their first.
   */
  groups?: string[]
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
  /**
   * rejectionsPerDay is this scope's fleet-wide (Redis-backed, like every
   * other counter on this view) count of requests DENIED by a limit
   * check today — never the in-process Prometheus rejection counter,
   * which is per-replica and not exposed here (admin.go: settleRejection
   * folds this into the SAME storeIncrMulti round trip the rejection
   * path already paid, so this field costs nothing extra to read).
   */
  rejectionsPerDay: number
  storeDown?: boolean
  /**
   * Unix seconds this scope last made a request, fleet-wide (admin.go:
   * adminUsageEntryView.LastSeen — the last-seen counter family,
   * throttled per-replica to one write per 60s). 0 or omitted means
   * never seen, or admin.stats.lastSeen is off (AdminFeaturesView.
   * lastSeen) — never render 0 as an epoch date.
   */
  lastSeen?: number
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

// UsageHistoryPoint/UsageHistoryResponse (GET /admin/api/usage/history)
// were removed (P3 item 25, dead code): the redesigned shell reads
// GET /admin/api/usage/series instead (AdminSeriesResponse below) —
// grep confirms no remaining importer since stores/history.ts (+its own
// spec) was deleted (redesign-plan.md section 3.5).

/**
 * One ranked model in GET /admin/api/usage/models (admin.go:
 * adminUsageModelEntryView). `id` is the canonical "provider/model" — the
 * provider that actually SERVED the traffic, which is why a failover
 * target and its primary appear as two separate rows. `value` is the total
 * for whichever metric the request asked to rank by — unchanged meaning
 * whether or not `detail` was requested.
 *
 * free-models-plan.md (per-model stats for free models): requests/tokensIn/
 * tokensOut/costMicroUsd/free are `detail=1`-only — present (each field
 * independently, possibly 0) only when the request set `detail=1`
 * (AdminUsageModelsResponse.detail), undefined otherwise. An id is included
 * by the server whenever ANY of the four totals is > 0, so a free model
 * with real request traffic but zero cost is still listed even when
 * ranking by cost, with `value` (and `costMicroUsd`) genuinely 0 — `free`
 * is how a reader tells that apart from "this model simply has no cost
 * data yet" (mirrors AdminModelMetaView's own "undefined means unknown,
 * present 0 means known-free" convention, never infer either from a value
 * being falsy).
 */
/**
 * Which billing rule actually priced a model (admin.go/pricing.go:
 * billingPriceSource) — an explicit per-model override, the built-in
 * LiteLLM-derived table, a live LiteLLM lookup, a configured free model
 * (modelMeta.free), or unpriced (billed as 0, no rule matched). This is
 * the BILLING truth, distinct from a ':free' suffix's display-only
 * relabeling (routes_unified.go) — a ':free' model still reports its
 * real priceSource here even though it displays as free.
 */
export type PriceSource = 'override' | 'builtin' | 'litellm' | 'free' | 'unpriced'

export interface AdminUsageModelEntry {
  id: string
  value: number
  requests?: number
  tokensIn?: number
  tokensOut?: number
  costMicroUsd?: number
  free?: boolean
  /** Cache hits/savings for this model today — present only when `detail=1` AND AdminFeaturesView.cacheStats is on (admin.go). cacheSavedMicroUsd is micro-USD, same unit as costMicroUsd. */
  cacheHits?: number
  cacheSavedMicroUsd?: number
  /** Count of 402 (unpriced-refusal) responses for this model today — `detail=1` only (admin.go). */
  r402?: number
  /** This model's billing price source — `detail=1` only (admin.go). */
  priceSource?: PriceSource
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
  /**
   * span is how many of `window`'s buckets, ending now, this ranking
   * summed (admin.go: parseUsageModelsSpan — empty request query means 1,
   * today's original single-current-bucket behaviour). Always echoed back,
   * never omitted, so a caller reading a cached/stale response can tell
   * which span it was fetched under without re-deriving it from the query
   * string.
   */
  span: number
  /**
   * detail echoes whether the request set `detail=1` (free-models-plan.md)
   * — true means every entry in `models` additionally carries its own
   * requests/tokensIn/tokensOut/costMicroUsd/free. Omitted (falsy) for the
   * ordinary, byte-identical-with-before response.
   */
  detail?: boolean
  models: AdminUsageModelEntry[]
  /**
   * How many buckets before the most recent one this ranking's span was
   * shifted back by (admin.go: parseUsageModelsOffset) — 0 for "ending
   * now", the ordinary case every pre-offset caller still gets. Always
   * echoed back, mirroring `span`'s own convention above.
   */
  offset: number
}

/** One MCP-server or agent target's current-window request counters (admin.go: adminTargetCountersView) — requests only, no tokens or cost. */
export interface AdminTargetCountersView {
  requestsPerMinute: number
  requestsPerDay: number
  requestsPerMonth: number
}

/**
 * One configured MCP server's or A2A agent's current health, matching the
 * GET /admin/api/targets `health` object the Go side builds against
 * (feat/target-health). Two independent sources feed `state`, never
 * conflated: 'traffic' is PASSIVE — recorded from the target's own real
 * proxied/federated request traffic as it happens, no extra requests
 * sent; 'probe' is the gateway's own lazy health check, triggered
 * opportunistically by an operator hitting /metrics, /admin, or a listing
 * endpoint (never a background poller). `state` is 'unhealthy' once at
 * least the configured `failureThreshold` consecutive failures have been
 * observed (whether or not this target has ever succeeded), 'healthy'
 * once it has succeeded at least once and stays below that threshold, and
 * 'unknown' otherwise. `consecutiveFailures` is always present.
 *
 * 'unknown' reads two different ways server-side, and only ONE of them
 * omits the rest of this object (F4, feat/target-health review):
 *  - never observed by either source at all: every other field is
 *    omitted — there is truly nothing to report.
 *  - observed, but never once succeeded, still below `failureThreshold`:
 *    every other field is PRESENT — `lastCheck`/`lastError`/`latencyMs`/
 *    `source` all carry a real observation, this target simply has not
 *    answered successfully yet. Do not treat 'unknown' as "no data" for
 *    this case; check whether `lastCheck` is set instead.
 *
 * `lastError` is additionally omitted whenever it is empty (a healthy
 * target has nothing to show), and `latencyMs`/`source` are omitted only
 * when genuinely not known (the never-observed case above).
 */
export interface AdminTargetHealthView {
  state: 'healthy' | 'unhealthy' | 'unknown'
  lastCheck?: string
  lastError?: string
  consecutiveFailures: number
  latencyMs?: number
  source?: 'probe' | 'traffic'
}

/**
 * One configured MCP server's or A2A agent's row in GET /admin/api/targets
 * (admin.go: adminTargetView). Access is the group names actually allowed
 * to reach this target, computed server-side via the same matching authz
 * enforcement uses — null/undefined means every configured group can
 * reach it, [] means no configured group can, and a non-empty list names
 * the groups that can (admin.go adminTargetView.Access).
 */
export interface AdminTargetView {
  name: string
  url: string
  access?: string[] | null
  counters: AdminTargetCountersView
  /** See AdminTargetHealthView's own doc comment (feat/target-health) — always present, unlike access above. */
  health: AdminTargetHealthView
}

/** GET /admin/api/targets (admin.go: adminTargetsResponse). */
export interface AdminTargetsResponse {
  mcpServers: AdminTargetView[]
  agents: AdminTargetView[]
}

/**
 * AdminEventKind is one gatewayEvent's classification (admin.go/events.go:
 * eventKindRateLimit/eventKindBudget/eventKindStoreDown/eventKindUpstream/
 * eventKindTimeout/eventKindUnpriced/eventKindCapacity). Typed as a union
 * with `| string` on AdminEventView.kind below, not this type alone: a
 * server build newer than this client could add a kind this webui does
 * not yet recognize, and the Events view must still render its raw text
 * rather than fail to compile/render it.
 */
export type AdminEventKind =
  | 'rate_limit'
  | 'budget'
  | 'store_down'
  | 'upstream'
  | 'timeout'
  | 'unpriced'
  | 'capacity'

/**
 * One row in GET /admin/api/events (admin.go/events.go: gatewayEvent — the
 * SAME shape stored as JSON in the eventRing/Redis list, never
 * re-projected). user/group/model/provider are the scope the event
 * happened against; every one of them, like message, is either a fixed
 * string or sanitizeProviderErr/sanitizeTargetErr output (admin.go) —
 * never a raw body, header, or credential (see events.go's own risk note,
 * dashboard-plan.md section 6). route is one of the fixed route-name
 * strings admin.go documents (chat/completions, embeddings, messages,
 * images, audio/speech, audio/transcriptions, passthrough, mcp, a2a,
 * mcp-federated, pricing, capacity) — kept as a bare `string` rather than a union
 * here since this view only ever displays it, never branches on it.
 */
export interface AdminEventView {
  time: string
  /** Which replica recorded this event — admin.go's g.replica, same value AdminOverviewResponse.replica carries for this replica's own poll. */
  replica: string
  instance?: string
  user?: string
  group?: string
  model?: string
  provider?: string
  route: string
  kind: AdminEventKind | string
  message: string
  status?: number
}

/**
 * GET /admin/api/events?limit=N (admin.go/events.go: adminEventsResponse —
 * new, F3). `source` tells the Events view whether `events` is the
 * fleet-wide Redis-backed list ('redis') or this replica's own in-memory
 * ring, read because Redis was unreachable or unconfigured ('replica') —
 * the view must caption this, never present a replica-only fallback as if
 * it were fleet-wide. `degraded` is set only in the 'replica' case where
 * Redis IS configured but the read failed (as opposed to Redis simply not
 * being configured at all) — omitted otherwise.
 */
export interface AdminEventsResponse {
  /** Newest first, never nil — an empty array means no events recorded (or capacity 0), never "the read failed" (a failed read still falls back to the ring, see `source`). */
  events: AdminEventView[]
  source: 'redis' | 'replica'
  replica: string
  /** The ring's fixed capacity (events.go: eventRingCap) — lets the view show "showing N of capacity" rather than a bare list length. */
  capacity: number
  degraded?: boolean
}
/**
 * GET /admin/api/usage/series (admin.go: adminSeriesResponse — phase 2
 * redesign). One or more scopes' bucketed time series for a single
 * metric, aligned on the SAME `buckets` array — every entry in `series`
 * has exactly `buckets.length` points, index-for-index. `unknown` names
 * every requested scope id the server could not resolve (never throws
 * for one bad id among several valid ones); `series` never nil.
 */
export interface AdminSeriesResponse {
  metric: string
  window: HistoryWindow
  span: number
  offset: number
  /** Oldest first — the same chronological order formatBucketLabel (lib/format.ts) already expects. */
  buckets: string[]
  series: { scope: string; points: number[] }[]
  unknown: string[]
}

/** The five aggregation kinds GET /admin/api/usage/totals accepts (admin.go: adminTotalsResponse.Kind). */
export type TotalsKind = 'user' | 'group' | 'usermodel' | 'modeluser' | 'targetcaller'

/**
 * GET /admin/api/usage/totals (admin.go: adminTotalsResponse — phase 2
 * redesign). A ranked table of ids (users, groups, user×model pairs,
 * model×user pairs, or target×caller pairs) with one or more summed
 * metrics each. Rows with every requested metric at 0 are dropped
 * server-side; `truncated` is set only when `limit` cut off further,
 * non-zero rows.
 */
export interface AdminTotalsResponse {
  kind: TotalsKind
  window: HistoryWindow
  span: number
  offset: number
  metrics: string[]
  /** id is a bare user/group/model id for kind 'user'/'group', or a composite id ("targetcaller": "mcp/fetch/alice") for the compound kinds — see admin.go's own id-building for the exact join rule per kind. */
  rows: { id: string; values: Record<string, number> }[]
  truncated?: boolean
}

/** The two `kind` values GET /admin/api/performance accepts, and the two windows it supports — a strict subset of HistoryWindow (no 'month': performance is a live-operations view, not a monthly rollup). */
export type PerfKind = 'provider' | 'model'
export type PerfWindow = Extract<HistoryWindow, 'hour' | 'day'>

/**
 * One provider's or model's performance row (admin.go: adminPerfRow).
 * Percentile fields (p50/p95/p99Ms, ttfbP50/95Ms) are present only when
 * AdminFeaturesView.latencyStats is on AND this id has at least one
 * observation in the requested span — undefined never means "0ms".
 * Attempts/failures are always present (admin.stats.latency is not
 * required for them). `overflow` marks a row whose slowest bucket
 * exceeded the latency histogram's own ceiling (metrics.go) — its
 * percentile fields, if present, are a floor, not an exact value.
 */
export interface AdminPerfRow {
  id: string
  p50Ms?: number
  p95Ms?: number
  p99Ms?: number
  ttfbP50Ms?: number
  ttfbP95Ms?: number
  count: number
  attempts: number
  failures: number
  timeouts?: number
  failovers?: number
  overflow?: boolean
}

/**
 * GET /admin/api/performance (admin.go: adminPerfResponse — phase 2
 * redesign). `rows` is the ranked-by-id table (the default view);
 * `buckets`/`points` are populated only when the request asked for
 * `series=1` against a single id, one AdminPerfRow per bucket (its own
 * `id` field is then the bucket string, not the provider/model id).
 * `rows` is optional: the server omits it (`omitempty`) on an empty
 * result (a quiet span or a fresh gateway) rather than sending `[]` —
 * every read site must default to `[]`.
 */
export interface AdminPerfResponse {
  kind: PerfKind
  window: PerfWindow
  span: number
  offset: number
  rows?: AdminPerfRow[]
  buckets?: string[]
  points?: AdminPerfRow[]
  latencyEnabled: boolean
}

/**
 * One provider's one model's resolved catalog entry (admin.go:
 * adminCatalogModel — phase 2 redesign). `priceSource`/`displayFree`
 * mirror AdminUsageModelEntry's own detail fields, but computed for
 * EVERY catalog model, not just ones with observed traffic — the Models
 * page's pricing-health table joins this against the usage ranking.
 *
 * `inputPerMTokUsd`/`outputPerMTokUsd` are the ACTUAL BILLED rate for
 * `priceSource` (Go's own billingPriceSource, pricing.go) — the exact
 * price a real request against this model gets charged, never a
 * separately-resolved display price. Both are undefined for
 * `priceSource: 'free'` or `'unpriced'` (nothing to report), and set for
 * every other source. `contextTokens` is a SEPARATE, display-only
 * resolution (admin.go's buildAdminModelMetaView — the same one GET
 * /admin/api/overview's own AdminModelMetaView carries), since a
 * context window is not a billing concept. N1 (verify-redesign-
 * final.md): an earlier version filled the two price fields from that
 * same display resolution instead, so a `pricing:` override with no
 * separate modelMeta/discovery price reported `priceSource: 'override'`
 * alongside undefined prices — CostAvoidedCard's pickReferenceModel
 * (lib/cost-avoided.ts) and PricingHealthTable both read these fields
 * assuming they agree with `priceSource`, and broke for exactly that
 * model.
 */
export interface AdminCatalogModel {
  id: string
  model: string
  contextTokens?: number
  inputPerMTokUsd?: number
  outputPerMTokUsd?: number
  priceSource: PriceSource
  /** True for a ':free' suffix model (routes_unified.go) — display-only relabeling; billing still follows `priceSource` unchanged. */
  displayFree?: boolean
  aliases?: string[]
}

/** One provider's full catalog listing (admin.go: adminCatalogProvider — phase 2 redesign), on-demand only (never part of the 5s overview poll). */
export interface AdminCatalogProvider {
  lastRefresh: string
  openUntil: string
  name: string
  type: string
  healthState: ProviderHealthState
  models: AdminCatalogModel[]
  discoveryEnabled: boolean
}

/** GET /admin/api/catalog (admin.go: adminCatalogResponse — phase 2 redesign). On-demand/stale-cached, never polled every 5s (Q12). */
export interface AdminCatalogResponse {
  providers: AdminCatalogProvider[]
  aliases: AdminAliasView[]
}

/**
 * One configured user's consumer-directory row (admin.go:
 * adminConsumerUser — phase 2 redesign). `source` is 'inline' (defined
 * directly in the plugin config) or 'file' (loaded from users.file);
 * `providers`/`models` are this user's OWN personal grant, distinct from
 * anything their group(s) already allow.
 */
export interface AdminConsumerUser {
  limits?: LimitsConfig
  name: string
  source: 'inline' | 'file'
  groups: string[]
  providers?: string[]
  models?: string[]
  admin?: boolean
}

/** How many of a provider's models a group can reach vs. how many that provider has (admin.go: adminModelAccessCount) — the Access Matrix's per-cell "n/m models" reading. */
export interface AdminModelAccessCount {
  allowed: number
  total: number
}

/**
 * One configured group's consumer-directory + access row (admin.go:
 * adminConsumerGroup — phase 2 redesign). `allowedProviders` is never
 * nil (empty means every configured provider); `modelAccess` is keyed by
 * provider name.
 */
export interface AdminConsumerGroup {
  limits?: LimitsConfig
  name: string
  providers?: string[]
  models?: string[]
  mcpServers?: string[]
  agents?: string[]
  passthroughPaths?: string[]
  allowedProviders: string[]
  modelAccess: Record<string, AdminModelAccessCount>
  memberCount: number
}

/** GET /admin/api/consumers (admin.go: adminConsumersResponse — phase 2 redesign). `usersFile` is the configured path only, never its contents (Risks, redesign-plan.md section 6). */
export interface AdminConsumersResponse {
  users: AdminConsumerUser[]
  groups: AdminConsumerGroup[]
  usersFile?: string
}

/**
 * GET /admin/api/config (admin.go: adminConfigResponse — phase 2
 * redesign). `config` is the FULL plugin config after redactConfig's
 * recursive walk: every apiKey/password literal replaced with
 * "[redacted]", an "env:NAME"/"file:/path" indirection kept verbatim,
 * and every baseUrl/url stripped of embedded userinfo credentials — safe
 * to render as-is (Config page's ConfigTree.vue), never re-sanitized
 * client-side.
 */
export interface AdminConfigResponse {
  config: Record<string, unknown>
  warnings: string[]
  features: AdminFeaturesView
  version: string
}

