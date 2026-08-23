// Package traefikllmgateway implements a Traefik Yaegi middleware plugin: a
// multi-provider LLM gateway with groups, per-user API keys, request/token/
// cost limits, and an MCP/A2A registry and proxy.
package traefikllmgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sync"

	telemetry "github.com/lukaszraczylo/oss-telemetry"
)

// Config is the plugin's dynamic configuration, populated by Traefik from
// the middleware's YAML/testData. Every limit or list field follows the
// convention: zero/empty/omitted means unlimited/all.
// Field order below is fieldalignment-verified (golangci-lint's govet
// enable-all, run with -fix against a scratch copy to derive the exact
// zero-waste sequence, then hand-applied here so every field keeps its
// original doc comment) — see providerState's own doc comment (registry.go)
// for the general pointer-first/scalar-last convention this follows.
type Config struct {
	Redis *RedisConfig `json:"redis,omitempty"`
	// Admin gates the read-only admin dashboard (spec §4, v0.2): nil or
	// Admin.Enabled false means the /admin* routes are not registered at
	// all — ServeHTTP falls through to its existing 404/passthroughUnknown
	// handling for those paths, preserving v0.1 behavior exactly.
	Admin      *AdminConfig             `json:"admin,omitempty"`
	Pricing    map[string]*ModelPricing `json:"pricing,omitempty"`
	MCPServers map[string]*TargetConfig `json:"mcpServers,omitempty"`
	Agents     map[string]*AgentConfig  `json:"agents,omitempty"`
	Users      *UsersConfig             `json:"users,omitempty"`
	Groups     map[string]*GroupConfig  `json:"groups,omitempty"`
	// ModelAliases maps an operator-defined alias id to a target model id
	// (spec §5, v0.2) — e.g. {"aliased/coding": "anthropic/claude-sonnet-4-5"}.
	// An exact alias match wins resolution before any other rule
	// (modelRegistry.resolve, registry.go); the target then resolves
	// through the normal rules. nil/omitted preserves v0.1 behavior
	// exactly: resolve never consults an empty alias map, so no existing
	// model id's resolution changes. Validated at construction
	// (validateModelAliases, registry.go).
	ModelAliases map[string]string          `json:"modelAliases,omitempty"`
	Providers    map[string]*ProviderConfig `json:"providers,omitempty"`
	// ModelMeta declares per-model metadata overrides (feature v0.23):
	// context window size and per-token cost, keyed by an exact
	// "provider/model" id or a bare model id (applying wherever that bare
	// id resolves — modelmeta.go's lookupModelMetaConfig checks the exact
	// form first). This ALWAYS wins over discovery-captured context or
	// the built-in LiteLLM-synced table (resolveModelMeta, modelmeta.go)
	// — distinct from Pricing above, which drives request cost
	// ACCOUNTING (costMicros); ModelMeta drives metadata EXPOSURE
	// (GET /v1/models' context_window/pricing extension fields, the
	// admin dashboard) and never affects billing. nil/omitted preserves
	// prior behavior exactly: every model's metadata falls through to
	// discovery/builtin/absent, same as before this feature existed.
	// Validated at construction (validateModelMeta, modelmeta.go).
	ModelMeta map[string]*ModelMetaConfig `json:"modelMeta,omitempty"`
	// Breaker configures the discovery circuit breaker (feat/provider-
	// health): how many consecutive discovery-refresh failures open a
	// provider's breaker, and how long it then backs off before probing
	// again. A struct value, not a pointer, for the same reason as Retry/
	// Cache below — its fields are individually zero-means-default
	// (validateBreakerConfig, registry.go), not gated by one Enabled
	// flag, so "omitempty" on this tag would be a no-op that misleadingly
	// implies otherwise. The zero value (Config{} with no breaker block
	// at all) resolves to every documented default and behaves exactly
	// like a deployment with no circuit breaker at all for a provider
	// that never fails — see validateBreakerConfig's own doc comment.
	// Failover configures feat/failover — cross-provider failover for a
	// bare model id more than one configured provider serves. See
	// FailoverConfig's own doc comment (failover.go) for the full
	// contract; a struct value, not a pointer, for the same
	// zero-means-default reason as Breaker/Retry/Cache below.
	Failover FailoverConfig `json:"failover"`
	Breaker  BreakerConfig  `json:"breaker"`
	// Retry is a struct value, not a pointer, because its own Enabled
	// field is the on/off signal (unlike Redis/Users, where the block's
	// mere presence is the signal) — so its tag omits "omitempty":
	// encoding/json never treats a struct value as "empty" regardless of
	// its fields, so "omitempty" here would be a no-op that misleadingly
	// implies otherwise.
	Retry RetryConfig `json:"retry"`
	// Cache is a struct value, not a pointer, for the same reason as Retry
	// above: its own Enabled field is the on/off signal.
	Cache CacheConfig `json:"cache"`
	// MaxInFlightBodyRequests caps how many unified and media JSON/
	// multipart requests (routes_unified.go's runUnified via
	// readAndDecodeUnifiedBody; routes_media.go's decodeMediaJSONRequest/
	// readAdmittedCapped, used by handleImagesGenerations/
	// handleAudioSpeech/handleAudioTranscriptions) may concurrently be
	// INSIDE their own read+decode step at once (security review finding
	// 1b, 2026-08-22; scope narrowed to read+decode only in round 3 — see
	// acquireBodyAdmission's own doc comment, routes_unified.go, for why
	// this is deliberately NOT a cap on total in-flight requests). 0 (the
	// default) self-tunes from GOMAXPROCS (defaultBodyAdmissionCap) rather
	// than adding an operator knob for a value the runtime can already
	// infer (house rule: prefer self-tuning over operator knobs where the
	// right value can be inferred; add a flag only when it genuinely
	// cannot be). A positive value pins an exact cap instead and always
	// wins over the auto-tuned default (house rule: an explicit override
	// always wins) — e.g. a deployment with a known, tighter memory
	// budget than GOMAXPROCS alone would infer — but is still clamped to
	// maxExplicitBodyAdmissionCap (llmgateway.go): "wins" means it
	// overrides the self-tuned value, not that an operator can silently
	// disable the bound entirely with an unbounded number.
	MaxInFlightBodyRequests int  `json:"maxInFlightBodyRequests,omitempty"`
	PassthroughUnknown      bool `json:"passthroughUnknown,omitempty"`
}

// ProviderConfig describes one upstream LLM provider.
type ProviderConfig struct {
	// Passthrough gates whether this provider's native passthrough route
	// ("/{providerName}/{rest...}", routes_passthrough.go's
	// handlePassthrough) is reachable at all. nil or true (the default)
	// preserves prior behavior exactly: every configured provider stays
	// reachable through native passthrough unless an operator explicitly
	// opts it out — FAIL-OPEN BY DESIGN, the same default direction as
	// every allow-list field in this package (GroupConfig's Providers/
	// Models/MCPServers/Agents/PassthroughPaths: empty/unset permits, an
	// explicit restriction narrows). Set false to disable a provider's
	// native passthrough entirely — e.g. an operator who wants clients
	// reaching a provider only through the unified /v1/* routes, never
	// its raw native API. A disabled provider's passthrough prefix is
	// treated exactly like an unconfigured one: ServeHTTP falls through
	// to the ordinary unknown-route 404, without even reaching auth
	// (security+performance audit, 2026-08-22). This does not affect the
	// MCP/A2A target proxy (mcp_a2a.go), which has its own routing
	// prefix and its own allow-list (GroupConfig.MCPServers/Agents) —
	// operators who route native tools like macstudio-rerank/parakeet-
	// mlx/piper-tts through THIS field's route must leave it unset or
	// true to keep that traffic flowing.
	Passthrough       *bool  `json:"passthrough,omitempty"`
	Type              string `json:"type"`
	BaseURL           string `json:"baseUrl,omitempty"`
	APIKey            string `json:"apiKey"`
	DiscoveryInterval string `json:"discoveryInterval,omitempty"`
	// MetadataPath is an optional second discovery endpoint (feature
	// v0.23), fetched alongside the provider's normal listModels call
	// when set: a path such as "/api/v0/models" (LM Studio's own,
	// non-OpenAI-compatible models endpoint) that reports per-model
	// context length. Only openai-type adapters read this field
	// (provider_openai.go's openaiAdapter.fetchModelMetadata) — an
	// anthropic/gemini provider configuring it has no effect. Empty (the
	// default) captures no metadata, preserving prior behavior exactly.
	// A failed or absent fetch is always non-fatal to discovery itself:
	// no metadata is captured, nothing else changes.
	MetadataPath string   `json:"metadataPath,omitempty"`
	Models       []string `json:"models,omitempty"`
	Discovery    bool     `json:"discovery,omitempty"`
}

// GroupConfig describes a group's access and limits. Empty/omitted
// Providers, Models, MCPServers, Agents mean all are allowed.
type GroupConfig struct {
	Limits    *LimitsConfig `json:"limits,omitempty"`
	Providers []string      `json:"providers,omitempty"`
	Models    []string      `json:"models,omitempty"`
	// Cache overrides the global cache.enabled setting for this group's
	// requests (spec §2): nil inherits the global setting, false opts the
	// group out even when caching is globally enabled, and true opts the
	// group in — but only when the global cache block is actually
	// configured (newAuthStore rejects true otherwise, a constructor
	// error, since there is nothing to inherit ttl/maxBodyBytes from).
	Cache *bool `json:"cache,omitempty"`
	// CacheTTL overrides the global cache.ttl (a Go duration string, e.g.
	// "5m") for this group's cache entries only. Empty (the default)
	// inherits the global TTL. Set means: parsed via time.ParseDuration at
	// construction (newAuthStore, auth.go); a malformed duration, a
	// duration <= 0, or CacheTTL set while the global cache block is not
	// configured (cfg.Cache.Enabled false — there is no global TTL to
	// override) is a constructor error naming the group. CacheTTL is
	// independent of Cache above: it names the TTL to use WHEN this
	// group's requests are cached, whether that caching comes from the
	// global default or from this group's own Cache:true.
	CacheTTL   string   `json:"cacheTTL,omitempty"`
	MCPServers []string `json:"mcpServers,omitempty"`
	Agents     []string `json:"agents,omitempty"`
	// PassthroughPaths restricts which REST path this group's native
	// passthrough requests may address, glob-matched (matchesGlob,
	// auth.go, via Go's path.Match) against passthroughRoute's own
	// "rest" — the ESCAPED path segment (r.URL.EscapedPath()-derived,
	// never percent-decoded) AFTER the provider name (e.g. "v1/files" for
	// a request to "/openai/v1/files"). Empty/omitted (the default) means
	// every path is allowed, preserving prior behavior exactly and
	// matching every other allow-list field's own empty-means-all
	// semantics in this struct — DO NOT invert that convention here. Set
	// a non-empty list to restrict a group to specific native endpoints
	// (e.g. ["v1/chat/completions"]), without narrowing any group that
	// leaves it unset (security review, 2026-08-22). The "home" group's
	// own providers/models/mcpServers/agents lists are already empty
	// (unrestricted); leaving this unset too keeps it that way.
	//
	// FOOTGUN (path.Match's documented behavior, verified in review): "*"
	// matches within ONE path segment only — it does NOT cross "/". A
	// pattern of exactly ["*"] therefore does NOT mean "allow every
	// path": it denies every "rest" with more than one segment, which is
	// most real passthrough traffic (e.g. "v1/chat/completions", three
	// segments, never matches "*"). Leave PassthroughPaths unset/empty to
	// actually allow everything; if you need a genuine multi-segment
	// wildcard, list every segment explicitly (e.g. "v1/*") or repeat a
	// "*/*/*"-shaped pattern per depth — there is no "**" (path.Match has
	// no such construct).
	PassthroughPaths []string `json:"passthroughPaths,omitempty"`
}

// LimitsConfig holds request/token/cost limits. Zero means unlimited.
type LimitsConfig struct {
	RequestsPerMinute int64   `json:"requestsPerMinute,omitempty"`
	RequestsPerDay    int64   `json:"requestsPerDay,omitempty"`
	TokensPerDay      int64   `json:"tokensPerDay,omitempty"`
	TokensPerMonth    int64   `json:"tokensPerMonth,omitempty"`
	CostPerDayUSD     float64 `json:"costPerDayUSD,omitempty"`
	CostPerMonthUSD   float64 `json:"costPerMonthUSD,omitempty"`
}

// UsersConfig combines inline users with an optional file-backed source.
type UsersConfig struct {
	File   string        `json:"file,omitempty"`
	Inline []*UserConfig `json:"inline,omitempty"`
}

// UserConfig describes one API-key holder.
type UserConfig struct {
	Limits *LimitsConfig `json:"limits,omitempty"`
	Name   string        `json:"name"`
	Group  string        `json:"group"`
	APIKey string        `json:"apiKey"`
	// Admin grants this user access to the read-only admin dashboard
	// (spec §4, v0.2), whether the user is inline or file-sourced —
	// file-users granting admin is operator-controlled via the Secret
	// backing Users.File, which is an accepted trust boundary. An admin
	// user is otherwise an ordinary user: their own keys, group, and
	// limits still apply, including to the admin routes themselves.
	Admin bool `json:"admin,omitempty"`
}

// RedisConfig configures the distributed limit-state backend.
type RedisConfig struct {
	FailOpen *bool  `json:"failOpen,omitempty"`
	Address  string `json:"address"`
	Password string `json:"password,omitempty"`
	DB       int    `json:"db,omitempty"`
	// PoolSize overrides respClient's self-tuned connection pool size
	// (defaultRespPoolSize, resp.go: GOMAXPROCS clamped to [4, 8]) — perf
	// finding 1, 2026-08-2x audit. Left at 0 (the default), the pool
	// self-tunes; an operator who sets this explicitly always wins over
	// that auto-tuning (house engineering rule), e.g. to open more
	// connections than a low-GOMAXPROCS container would otherwise pick
	// for a Redis known to have plenty of headroom, or fewer against a
	// connection-constrained managed Redis tier. A negative value, or one
	// above respPoolConfigMax (resp.go), is a construction error
	// (buildRedisClient, below) — negative can never mean "no pool", and
	// an unbounded value would eagerly allocate that many *respConn
	// structs at construction before a single request arrives.
	PoolSize int `json:"poolSize,omitempty"`
}

// RetryConfig configures same-provider retry on transient upstream
// failures (connection errors, HTTP 429, HTTP 5xx) — spec §1. The zero
// value (Enabled: false) disables retry, preserving v0.1 behavior
// exactly: every request still makes exactly one upstream attempt.
// Attempts and Backoff are validated (and defaulted, when left zero) by
// newRetryPolicy (retry.go) only when Enabled is true.
type RetryConfig struct {
	// Backoff is the base wait before the first retry (a Go duration
	// string, e.g. "250ms"); it doubles on each further retry, capped at
	// 2s per wait. Defaults to "250ms" when Enabled and left empty.
	Backoff string `json:"backoff,omitempty"`
	// Attempts is the number of retries performed AFTER the first try —
	// not the total try count. 1 (the default, applied when Enabled and
	// left at 0) allows one retry: two tries total. The maximum, 3,
	// allows three retries: four tries total. A value outside 1..3 is a
	// construction error.
	Attempts int  `json:"attempts,omitempty"`
	Enabled  bool `json:"enabled,omitempty"`
}

// CacheConfig configures the opt-in, Redis-backed response cache for
// unified non-streaming chat/embeddings responses (spec §2). The zero
// value (Enabled: false) disables caching, preserving v0.1 behavior
// exactly. TTL and MaxBodyBytes are validated (and defaulted, when left
// zero) by validateCacheConfig (cache.go) only when Enabled is true.
// Caching additionally requires config.Redis (buildResponseCache disables
// it with a one-time warning, not a constructor error, when Enabled is
// true but Redis is not configured) — a per-replica cache without a
// shared backend would serve divergent responses across replicas.
type CacheConfig struct {
	// TTL is how long a cached response stays valid (a Go duration
	// string, e.g. "5m"). Defaults to defaultCacheTTL when Enabled and
	// left empty.
	TTL string `json:"ttl,omitempty"`
	// MaxBodyBytes caps how large a response body may be to still be
	// stored; a larger one is skipped (spec §2's "body ≤ maxBodyBytes").
	// Defaults to defaultCacheMaxBodyBytes when Enabled and left at 0; a
	// value above maxCacheMaxBodyBytes is a construction error.
	MaxBodyBytes int  `json:"maxBodyBytes,omitempty"`
	Enabled      bool `json:"enabled,omitempty"`
}

// BreakerConfig configures the per-provider discovery circuit breaker
// (feat/provider-health): closed -> open -> half-open, driven entirely by
// discovery-refresh outcomes (registry.go's providerState.finishRefresh),
// never by request-path traffic. Every field is individually zero-means-
// default (validateBreakerConfig, registry.go) — there is no Enabled
// flag, unlike Retry/Cache above, because the breaker is always live:
// its defaults are chosen so a provider that never fails never notices
// it exists, so there is nothing meaningful to "disable". Field order
// (both strings before the int) is fieldalignment-sensitive, the same
// convention providerState's own doc comment explains.
type BreakerConfig struct {
	// OpenDuration is the base backoff a newly opened breaker waits
	// before its first half-open probe (a Go duration string, e.g.
	// "1m"). Doubles on every further failed probe, capped at
	// MaxOpenDuration. Empty (the default) uses
	// defaultBreakerOpenDuration (registry.go).
	OpenDuration string `json:"openDuration,omitempty"`
	// MaxOpenDuration caps the exponential backoff OpenDuration above
	// doubles into. Empty (the default) uses
	// defaultBreakerMaxOpenDuration (registry.go). Must be >=
	// OpenDuration when both are set.
	MaxOpenDuration string `json:"maxOpenDuration,omitempty"`
	// FailureThreshold is how many CONSECUTIVE failed discovery refreshes
	// open the breaker. 0 (the default) uses defaultBreakerFailureThreshold
	// (registry.go). A value outside 1..maxBreakerFailureThreshold is a
	// construction error.
	FailureThreshold int `json:"failureThreshold,omitempty"`
}

// AdminConfig configures the read-only admin dashboard (spec §4, v0.2).
// The zero value (Enabled: false) disables it, preserving v0.1 behavior
// exactly: no /admin* route is registered, so those paths fall through
// to ServeHTTP's existing 404/passthroughUnknown handling like any other
// unrecognized path.
type AdminConfig struct {
	Enabled bool `json:"enabled,omitempty"`
}

// ModelPricing overrides the built-in per-model price table.
type ModelPricing struct {
	InputPerM  float64 `json:"inputPerM"`
	OutputPerM float64 `json:"outputPerM"`
}

// ModelMetaConfig overrides one model's context window and per-token
// cost (feature v0.23, Config.ModelMeta) — layered ahead of discovery-
// captured context and the built-in LiteLLM-synced table
// (resolveModelMeta, modelmeta.go). ContextTokens left at 0 falls
// through to the next layer rather than overriding with an explicit
// unknown; the same holds for InputCostPerMTokMicroUSD/
// OutputCostPerMTokMicroUSD when Free is false. Free, when true,
// explicitly zeroes both cost fields regardless of whatever they are
// set to — a KNOWN zero cost, distinct from the "not overridden here"
// fallback a zero cost field means when Free is false. Setting Free
// true together with a non-zero cost field is rejected at construction
// (validateModelMeta) as a contradictory config.
type ModelMetaConfig struct {
	ContextTokens             int   `json:"contextTokens,omitempty"`
	InputCostPerMTokMicroUSD  int64 `json:"inputCostPerMTokMicroUsd,omitempty"`
	OutputCostPerMTokMicroUSD int64 `json:"outputCostPerMTokMicroUsd,omitempty"`
	Free                      bool  `json:"free,omitempty"`
}

// TargetConfig is a proxied upstream target (MCP server).
type TargetConfig struct {
	URL string `json:"url"`
}

// AgentConfig is a proxied A2A agent.
type AgentConfig struct {
	URL  string `json:"url"`
	Card string `json:"card,omitempty"`
}

// CreateConfig returns a Config populated with the plugin's defaults.
func CreateConfig() *Config {
	return &Config{}
}

// minBodyAdmissionCap/maxBodyAdmissionCap clamp defaultBodyAdmissionCap's
// self-tuned result (security review finding 1b; numbers revised round
// 3, 2026-08-22 — see this const block's own history below for why).
// The floor keeps a single-vCPU sidecar from self-tuning down to a cap so
// small it throttles ordinary traffic, and the ceiling keeps a very
// large host from self-tuning up to a cap so high the semaphore stops
// bounding anything.
//
// REVISED (round 3, coordinator ruling — fixes an availability
// regression the round-2 numbers shipped alongside): round 2 sized these
// against a "whole request" hold duration (acquireBodyAdmission was held
// for the entire handler, including the upstream round trip) and a
// ~170MB-per-slot estimate that bundled the decode, the re-marshal, AND
// the response buffer together. Measured, zero-config, at round 2's
// numbers: 400 concurrent chat requests against a 2-vCPU pod (cap 16)
// saw 220 succeed and 180 get 503 — an LLM completion is I/O-bound for
// seconds to minutes, so a cap sized for a brief decode became a hard
// ceiling on TOTAL in-flight requests instead.
//
// acquireBodyAdmission's slot is now held ONLY for the read+decode step
// itself (see its own doc comment, routes_unified.go) — typically
// single-digit milliseconds for a body under maxUnifiedRequestBytes
// (4MiB), a reduction of roughly three to four orders of magnitude in
// HOLD DURATION versus round 2. The relevant per-slot memory figure is
// also now decode-only, not decode+response: measured at up to ~51.5MB
// LIVE heap (12.3x) for an adversarial 4MiB body shaped to maximize
// per-value allocation overhead, with ~189MB TotalAlloc (~45x) of mostly
// short-lived garbage collected within the same decode
// (maxUnifiedRequestBytes' own doc comment, routes_unified.go, has the
// full measurement). At the new ceiling (256), a SUSTAINED worst case —
// every slot simultaneously decoding the adversarial shape, which the
// now-brief hold duration makes far less likely to persist than round
// 2's multi-second window — peaks around 256 × 51.5MB ≈ 13.2GB LIVE
// heap; the cumulative TotalAlloc figure (256 × 189MB ≈ 48.4GB) is
// garbage-collection churn, not simultaneously resident memory, and is
// not the number to size a pod's memory limit against. Both the floor
// and ceiling were doubled from round 2 (16→32, 128→256) and the
// per-CPU multiplier tripled (8→24): a raise smaller than the hold-
// duration reduction warrants, deliberately conservative, since the
// worst-case LIVE-heap arithmetic above still needs to stay defensible
// on a modest node.
const (
	minBodyAdmissionCap = 8
	maxBodyAdmissionCap = 64
)

// NOTE on these bounds (round-4 correction, measured): review round 3
// shipped [32, 256], reasoning from the isolated json.Unmarshal peak
// (~51.5MB for an adversarial 4MiB body). The verifier then measured the
// END-TO-END per-slot cost through the real handler — ~80-108MB, since
// the raw body []byte stays live alongside the decoded map and GC lags —
// and swept the cap against peak HeapInuse under 400 concurrent
// adversarial requests:
//
//	cap  16 ->  1.5GB    cap  64 ->  6.1GB
//	cap  32 ->  3.4GB    cap 256 -> 21.1GB
//
// A 256 ceiling therefore does not prevent the OOM this semaphore exists
// to prevent: it is above any realistic pod budget. It is still strictly
// better than the unbounded base (25GB on the same burst), but "better
// than unbounded" was never the goal. The reference deployment runs
// Traefik with resources.limits.memory: 512Mi, where even the 8-slot
// floor is the binding constraint, not the ceiling.
//
// So the bounds are cut to [8, 64]: 64 keeps a large-memory host from
// being throttled below what it can actually serve, 8 keeps a small pod
// from admitting more concurrent decodes than its heap can hold. This
// costs legitimate throughput almost nothing because the slot is now
// held ONLY across read+decode (round-3 fix) — microseconds to low
// milliseconds — not across the upstream call; an operator whose host
// genuinely wants more sets Config.MaxInFlightBodyRequests explicitly,
// which always wins. Deriving the ceiling from the process's real memory
// budget (runtime/debug.SetMemoryLimit(-1), which the chart already sets
// via goMemLimitPercentage) is the better long-term shape and is left as
// a follow-up: it needs its own Yaegi-interpreted verification, since
// runtime/debug's availability under the interpreter is unproven here.

// bodyAdmissionCapPerCPU is defaultBodyAdmissionCap's GOMAXPROCS
// multiplier (revised round 3 — see minBodyAdmissionCap's own doc
// comment for the full before/after reasoning): a decode-scoped slot is
// held for milliseconds, not the seconds-to-minutes an I/O-bound LLM
// completion takes, so — unlike round 2's comment, which reasoned about
// upstream-latency headroom for a hold spanning the whole request — this
// multiplier is sized against CPU throughput for a brief, CPU-bound
// json.Unmarshal burst, not I/O parking.
const bodyAdmissionCapPerCPU = 24

// defaultBodyAdmissionCap returns the self-tuned in-flight cap
// acquireBodyAdmission's semaphore is sized to when Config.
// MaxInFlightBodyRequests is left at 0 (security review finding 1b,
// 2026-08-22): GOMAXPROCS-derived rather than a single constant that would
// be wrong for both a 1-vCPU sidecar and a 32-vCPU ingress node alike
// (house rule: prefer self-tuning over operator knobs where the right
// value can be inferred from the environment), clamped to
// [minBodyAdmissionCap, maxBodyAdmissionCap].
func defaultBodyAdmissionCap() int {
	n := runtime.GOMAXPROCS(0) * bodyAdmissionCapPerCPU
	if n < minBodyAdmissionCap {
		return minBodyAdmissionCap
	}
	if n > maxBodyAdmissionCap {
		return maxBodyAdmissionCap
	}
	return n
}

// maxExplicitBodyAdmissionCap clamps an explicit Config.
// MaxInFlightBodyRequests override (round 3, 2026-08-22 review — the
// round-2 version accepted any positive value verbatim, including
// something like math.MaxInt from a fat-fingered config, which would
// silently disable the bound entirely by making the semaphore
// effectively unbounded). The house rule ("explicit override always
// wins") governs the choice between self-tuning and pinning a value, not
// whether a pinned value gets any sanity check at all — an operator who
// genuinely needs more concurrency than this still gets a very high
// ceiling, just not an unbounded one reachable by typo.
const maxExplicitBodyAdmissionCap = 10_000

// Gateway is the Traefik middleware handler.
//
// Field order below is fieldalignment-verified (golangci-lint's govet
// enable-all, run with -fix against a scratch copy to derive the exact
// zero-waste sequence, then hand-applied here so every field keeps its
// original doc comment) — see providerState's own doc comment
// (registry.go) for the general convention.
type Gateway struct {
	next http.Handler
	// targetClient is the shared, connection-pooled *http.Client the
	// MCP/A2A target proxy (mcp_a2a.go) issues every upstream request
	// through — built once via newAdapterHTTPClient, the same constructor
	// each provider adapter uses for its own client.
	targetClient *http.Client
	auth         *authStore
	limiter      *limiter
	registry     *modelRegistry
	adapters     map[string]providerAdapter
	cfg          *Config
	// cache is nil whenever response caching is not configured or not
	// usable (cfg.Cache.Enabled is false, or true with no config.Redis —
	// see buildResponseCache, cache.go). Every call site checks for nil
	// before using it, rather than responseCache having its own
	// always-disabled zero value.
	cache *responseCache
	// redisClient is the same instance newGateway hands to both the
	// limiter's redisStore and the response cache (its own doc comment,
	// below, explains why it's built once and shared) — kept here too,
	// on Gateway itself, purely so Close (below) has something to release
	// pooled connections through. nil when config.Redis is absent, same
	// as buildRedisClient's own nil-for-unconfigured contract.
	redisClient *respClient
	// bodyAdmission is the buffered-channel semaphore acquireBodyAdmission
	// (routes_unified.go) claims from and releases: security review
	// finding 1b, 2026-08-22. Sized once, here, by newGateway (see
	// defaultBodyAdmissionCap/Config.MaxInFlightBodyRequests) — never
	// resized afterward, matching a Go channel's own fixed-capacity
	// contract.
	bodyAdmission chan struct{}
	// failoverHealth is feat/failover's own per-pod, in-memory
	// request-path health signal (failover.go), built once, here, by
	// newGateway. Every method on it is nil-receiver-safe, so a Gateway
	// assembled directly (bypassing newGateway, as a few older tests do)
	// degrades to "failover never skips a candidate for request health"
	// rather than a nil-pointer panic.
	failoverHealth *requestHealthTracker
	name           string
	// failoverLogGate rate-limits runMeteredCall's generic "failing over"
	// log line (routes_unified.go) to once per storeErrorLogEvery
	// (adversarial-review fix, F10) — reuses auth.go's own logGate type,
	// the identical technique shouldLogAuthFailure/logStoreError already
	// apply elsewhere in this package. Zero-value-usable: the first call
	// on a fresh Gateway always logs. Deliberately does NOT gate the 404
	// loud-log line (operator ruling: that one must always log).
	failoverLogGate logGate
	// failover is Config.Failover, validated and resolved once by
	// newGateway (validateFailoverConfig, failover.go).
	failover failoverConfig
}

// telemetryStartupOnce keeps the anonymous "plugin loaded" ping to one per
// process. Traefik calls New once per ROUTE that uses this plugin, and a
// busy ingress can have many; oss-telemetry does not deduplicate on the
// client side (the server does), so the gate belongs here.
var telemetryStartupOnce sync.Once

// sendStartupTelemetry fires at most one anonymous "plugin loaded" ping
// per process, matching traefikoidc's behaviour.
//
// What leaves the pod: project name, version and a timestamp. No
// identifiers, no config, no provider names, no keys, no request data.
// The call never blocks (it runs in oss-telemetry's own goroutine), never
// panics, never retries and never returns an error, so it cannot affect
// request handling or delay Traefik's startup.
//
// Silent unless the build was stamped: an unstamped tree still carries
// devPluginVersion, so developers and operators building from a checkout
// never phone home. Operators of a RELEASE build opt out with any of
// DO_NOT_TRACK=1, OSS_TELEMETRY_DISABLED=1, or
// TRAEFIK_LLMGATEWAY_DISABLE_TELEMETRY=1 (oss-telemetry reads all three;
// the last is this project's name uppercased with dashes replaced). This
// is documented in README.md — a plugin that phones home without saying
// so in its own README would be indefensible.
func sendStartupTelemetry() {
	telemetryStartupOnce.Do(func() {
		if !shouldSendTelemetry(pluginVersion) {
			return
		}
		telemetry.Send(telemetryProjectName, pluginVersion)
	})
}

// telemetryProjectName is the project identifier reported in the ping. It
// matches the repository name, and doubles as the prefix oss-telemetry
// derives its per-project opt-out variable from
// (TRAEFIK_LLMGATEWAY_DISABLE_TELEMETRY).
const telemetryProjectName = "traefik-llmgateway"

// shouldSendTelemetry reports whether a build stamped with version may
// emit a startup ping. Only release-stamped builds may: an empty or
// dev-sentinel version means nothing stamped this tree, so it is a
// checkout, a test run or a local build, none of which should phone home.
//
// Split out from sendStartupTelemetry so the decision is testable without
// a network: the send itself goes through oss-telemetry, whose endpoint
// is deliberately not overridable from consuming code.
func shouldSendTelemetry(version string) bool {
	return version != "" && version != devPluginVersion
}

// New creates the middleware. NOTE: no tail call — Yaegi zeroes
// multi-value tail-call returns across the reflect boundary.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	g, err := newGateway(ctx, next, config, name)
	if err != nil {
		return nil, err
	}
	// Deliberately AFTER construction succeeds: a config the gateway
	// rejects never loaded a working plugin, so counting it as an install
	// would inflate the numbers with failures.
	sendStartupTelemetry()
	return g, nil
}

// Close releases resources this Gateway instance holds open: currently,
// the pooled Redis connections behind redisClient (perf finding 1),
// shared between the limiter's redisStore and the response cache — a
// nil-safe no-op when Redis is not configured.
//
// Review fix (Should-Fix 4, 2026-08-2x): nothing in this codebase calls
// Close automatically. Traefik's plugin contract for a locally-loaded
// middleware is exactly `func New(...) (http.Handler, error)`, above —
// the returned value satisfies only http.Handler, with no Shutdown/Close
// interface Traefik itself checks for or invokes on a discarded
// instance. README.md's "Config hot-reload" section already documents
// this repository has no evidence of Traefik's teardown mechanism for an
// old instance at config-reload swap time; a plugin-side Close, however
// correct, has nowhere in Traefik's own contract to be wired to. Close
// exists for direct Go callers — tests, or a future framework/harness
// that DOES hold such a hook — to release pooled connections
// deterministically instead of depending solely on GC finalizing the
// underlying net.Conns. A discarded, never-Closed Gateway still leaks at
// most respPoolMax (8) idle connections rather than the pre-pool single
// connection — a real but bounded regression the pool accepts (perf
// finding 1), because nothing in Traefik's own contract gives this
// plugin a way to do better.
func (g *Gateway) Close() error {
	if g.redisClient == nil {
		return nil
	}
	return g.redisClient.Close()
}

func newGateway(ctx context.Context, next http.Handler, config *Config, name string) (*Gateway, error) {
	if config == nil || len(config.Providers) == 0 {
		return nil, errors.New("llmgateway: at least one provider must be configured")
	}
	if err := validateTargetURLs(config); err != nil {
		return nil, err
	}
	auth, err := newAuthStore(config)
	if err != nil {
		return nil, err
	}
	g := &Gateway{next: next, name: name, cfg: config, auth: auth}

	// feat/failover: validated once, here — see FailoverConfig's own doc
	// comment (failover.go) for the default (disabled, coordinator
	// ruling) and what "single-provider deployment behaves exactly as
	// before" means in practice.
	failoverCfg, err := validateFailoverConfig(config.Failover)
	if err != nil {
		return nil, err
	}
	g.failover = failoverCfg
	g.failoverHealth = newRequestHealthTracker()

	// bodyAdmissionCap: an explicit MaxInFlightBodyRequests always wins
	// over the self-tuned default (house rule: explicit override always
	// wins) — see Config.MaxInFlightBodyRequests' own doc comment. 0 (the
	// field's zero value, and every config that predates this field) means
	// "self-tune", never "cap at zero" — a channel with a zero buffer
	// would reject every request outright, which is never the intent of
	// an operator who simply never set this field. An explicit override
	// is still clamped to maxExplicitBodyAdmissionCap (round 3, 2026-08-22
	// review) — "wins" means it overrides the self-tuned VALUE, not that
	// it can disable the bound entirely via an unbounded number.
	bodyAdmissionCap := defaultBodyAdmissionCap()
	if config.MaxInFlightBodyRequests > 0 {
		bodyAdmissionCap = config.MaxInFlightBodyRequests
		if bodyAdmissionCap > maxExplicitBodyAdmissionCap {
			bodyAdmissionCap = maxExplicitBodyAdmissionCap
		}
	}
	g.bodyAdmission = make(chan struct{}, bodyAdmissionCap)

	if config.Users != nil && config.Users.File != "" {
		if err = attachUsersFile(auth, config.Users.File, g); err != nil {
			return nil, err
		}
	}

	// The Redis client (nil when config.Redis is absent) is built once,
	// here, and shared between the limiter's redisStore and the response
	// cache below — spec §2's "reuse the SAME respClient instance as the
	// limiter's redisStore (one connection, one config)". Building it
	// separately per consumer would dial (and AUTH/SELECT) a second TCP
	// connection to the exact same server for no benefit.
	redisClient, err := buildRedisClient(config.Redis)
	if err != nil {
		return nil, err
	}
	g.redisClient = redisClient
	lim := newConfiguredLimiter(config, redisClient)
	lim.logf = g.errorf
	g.limiter = lim

	cache, err := buildResponseCache(config.Cache, redisClient, g.logf, g.errorf)
	if err != nil {
		return nil, err
	}
	g.cache = cache

	adapters, err := buildAdapters(config)
	if err != nil {
		return nil, err
	}
	g.adapters = adapters
	g.targetClient = newAdapterHTTPClient()

	registry, err := newModelRegistry(adapters, config, g.errorf)
	if err != nil {
		return nil, err
	}
	// Routes the self-resolving conditions (a model id two providers both
	// serve) to WARN instead of ERROR — see the registry's warn field.
	registry.warn = g.warnf
	// ctx is passed through unwrapped, not re-bounded to warmFillTimeout here:
	// warmFill already gives each discovery-enabled provider its own fresh
	// warmFillTimeout budget per provider (registry.go). Wrapping ctx to a
	// single shared warmFillTimeout here would make that per-provider budget
	// a lie — provider 2..N would inherit whatever is left of the first
	// provider's deadline instead of a full one. Construction's worst case is
	// therefore N * warmFillTimeout (N = discovery-enabled providers), which
	// is accepted.
	registry.warmFill(ctx)
	g.registry = registry

	setPricingWarnFn(func(msg string) {
		if msg == warnCapMessage {
			g.logf("%s", msg)
			return
		}
		g.logf("pricing: no price configured for model %q; cost will be recorded as 0", msg)
	})

	return g, nil
}

// buildRedisClient validates rc and returns a respClient for it, or nil
// when rc is nil (Redis not configured). Split out of what was previously
// newConfiguredLimiter's own inline construction so newGateway can share
// one respClient — one dialled connection, one AUTH/SELECT handshake —
// between the limiter's redisStore and the response cache (cache.go),
// instead of each dialing its own connection to the identical server.
func buildRedisClient(rc *RedisConfig) (*respClient, error) {
	if rc == nil {
		return nil, nil
	}
	if rc.Address == "" {
		return nil, errors.New("llmgateway: redis: address must not be empty")
	}
	if rc.DB < 0 {
		return nil, fmt.Errorf("llmgateway: redis: db must not be negative, got %d", rc.DB)
	}
	if rc.PoolSize < 0 {
		return nil, fmt.Errorf("llmgateway: redis: poolSize must not be negative, got %d", rc.PoolSize)
	}
	if rc.PoolSize > respPoolConfigMax {
		return nil, fmt.Errorf("llmgateway: redis: poolSize must not exceed %d, got %d", respPoolConfigMax, rc.PoolSize)
	}

	password, err := resolveSecret(rc.Password)
	if err != nil {
		return nil, fmt.Errorf("llmgateway: redis: %w", err)
	}

	// PoolSize left at 0 keeps newRESPClient's own self-tuned default
	// (defaultRespPoolSize, resp.go); an explicit positive value always
	// overrides it (RedisConfig.PoolSize's own doc comment).
	if rc.PoolSize > 0 {
		return newRESPClientPool(rc.Address, password, rc.DB, rc.PoolSize), nil
	}
	return newRESPClient(rc.Address, password, rc.DB), nil
}

// newConfiguredLimiter builds the limiter for config, using client (nil
// means config.Redis was absent) as its redisStore backend. failOpen
// defaults to true (a Redis outage must not take the whole gateway down
// unless an operator opts into strict enforcement) unless
// RedisConfig.FailOpen is explicitly set.
func newConfiguredLimiter(config *Config, client *respClient) *limiter {
	if client == nil {
		return newLimiter(nil, true)
	}
	failOpen := true
	if config.Redis.FailOpen != nil {
		failOpen = *config.Redis.FailOpen
	}
	return newLimiter(newRedisStore(client), failOpen)
}

// attachUsersFile performs the synchronous initial load of a Users.File path
// and wires auth up for throttled hot reload. A failing initial load is a
// constructor error — fail fast on bad config rather than start with an
// empty file-sourced user set.
func attachUsersFile(auth *authStore, path string, log gatewayLogger) error {
	// Stat before load, matching maybeReload's ordering: capturing the mtime
	// before reading content means a write landing between the stat and the
	// read still shows up as a further mtime change on the first post-
	// startup reload check, instead of being silently lost forever — a
	// stat-after-load can observe a newer mtime for content it never read.
	//
	// statErr is intentionally ignored here: if the stat fails, lastModTime
	// stays at its zero value, so the first maybeReload call sees any real
	// mtime as "changed" and reloads once more — a harmless extra reload of
	// content already loaded, not a correctness problem.
	info, statErr := os.Stat(path)

	uf := newUsersFile(path)
	initial, err := uf.load()
	if err != nil {
		return fmt.Errorf("llmgateway: cannot load initial users file %q: %w", path, err)
	}
	if err := auth.replaceFileUsers(initial); err != nil {
		return fmt.Errorf("llmgateway: initial users file %q: %w", path, err)
	}

	auth.usersFile = uf
	auth.log = log
	if statErr == nil {
		auth.lastModTime = info.ModTime()
	}
	auth.lastCheck = auth.nowFn()
	return nil
}

// ServeHTTP is the internal router. It grows in later tasks.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusTrackingWriter{ResponseWriter: w}
	defer recoverPanic(sw, g)
	g.auth.maybeReload()
	g.registry.maybeRefresh(r.Context())

	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		g.handleModels(sw, r)
		return
	}

	if r.Method == http.MethodPost && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/embeddings") {
		u, grp, ok := g.auth.identify(r)
		g.logAuthEvent(ok, authEventUserName(u), r)
		if !ok {
			writeOAIError(sw, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			g.handleChat(sw, r, u, grp)
		} else {
			g.handleEmbeddings(sw, r, u, grp)
		}
		return
	}

	if r.Method == http.MethodPost && r.URL.Path == messagesPath {
		// Anthropic SDKs authenticate with x-api-key, not Authorization:
		// Bearer — g.auth.identify (auth.go) already reads both headers,
		// Bearer taking priority when a caller somehow sends both, so no
		// route-specific auth handling is needed here beyond using the
		// same identify/logAuthEvent call every other route already
		// makes. A failed auth gets the Anthropic error shape
		// (writeAnthropicError, routes_messages.go), not writeOAIError's
		// — the one thing that differs from every route above.
		u, grp, ok := g.auth.identify(r)
		g.logAuthEvent(ok, authEventUserName(u), r)
		if !ok {
			writeAnthropicError(sw, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
			return
		}
		g.handleMessages(sw, r, u, grp)
		return
	}

	if r.Method == http.MethodPost && (r.URL.Path == imagesGenerationsPath || r.URL.Path == audioSpeechPath || r.URL.Path == audioTranscriptionsPath) {
		u, grp, ok := g.auth.identify(r)
		g.logAuthEvent(ok, authEventUserName(u), r)
		if !ok {
			writeOAIError(sw, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
			return
		}
		switch r.URL.Path {
		case imagesGenerationsPath:
			g.handleImagesGenerations(sw, r, u, grp)
		case audioSpeechPath:
			g.handleAudioSpeech(sw, r, u, grp)
		case audioTranscriptionsPath:
			g.handleAudioTranscriptions(sw, r, u, grp)
		}
		return
	}

	// Admin routes (spec §4, v0.2) are matched only when adminEnabled: a
	// nil or disabled Config.Admin means none of these paths are
	// registered at all, so a request to /admin* falls through to the
	// same 404/passthroughUnknown handling as any other unrecognized
	// path — no special-casing needed for the disabled case.
	if adminEnabled(g.cfg) && r.Method == http.MethodGet && isAdminPath(r.URL.Path) {
		g.handleAdmin(sw, r)
		return
	}

	if r.Method == http.MethodGet && (r.URL.Path == "/v1/mcp/servers" || r.URL.Path == "/v1/agents") {
		u, grp, ok := g.auth.identify(r)
		g.logAuthEvent(ok, authEventUserName(u), r)
		if !ok {
			writeOAIError(sw, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
			return
		}
		if r.URL.Path == "/v1/mcp/servers" {
			g.handleMCPServers(sw, grp)
		} else {
			g.handleAgents(sw, grp)
		}
		return
	}

	// Federated /mcp (Feature C, v0.21) is matched ahead of the per-server
	// "/mcp/{name}/..." target proxy below: targetRoute's own splitting
	// already rejects a bare "/mcp" (no name segment to route to), so
	// there is no ambiguity between the two, but this ordering keeps every
	// exact-path route grouped together (mirroring "/v1/mcp/servers" and
	// "/v1/agents" immediately above) ahead of the prefix-matched one.
	//
	// Checked on r.URL.Path alone, BEFORE the method check: a GET or
	// DELETE to exactly "/mcp" is a real, well-known route addressed with
	// the wrong verb (SF8, review round 2, 2026-08-21) — 405 with an
	// Allow header naming the one method this route accepts, matching
	// RFC 9110 §15.5.6, rather than falling through to passthroughUnknown/
	// 404 the way a path this handler has never heard of would. No
	// auth check runs first: which HTTP method a route accepts is
	// unauthenticated request-shape information, not something worth
	// gating behind a valid API key.
	if r.URL.Path == federatedMCPPath {
		if r.Method != http.MethodPost {
			sw.Header().Set("Allow", http.MethodPost)
			writeOAIError(sw, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		u, grp, ok := g.auth.identify(r)
		g.logAuthEvent(ok, authEventUserName(u), r)
		if !ok {
			writeOAIError(sw, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
			return
		}
		g.handleMCPFederated(sw, r, u, grp)
		return
	}

	if name, rest, ok := targetRoute(r.URL.EscapedPath(), targetKindMCP); ok {
		u, grp, authOK := g.auth.identify(r)
		g.logAuthEvent(authOK, authEventUserName(u), r)
		if !authOK {
			writeOAIError(sw, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
			return
		}
		g.handleTargetProxy(sw, r, u, grp, targetKindMCP, name, rest)
		return
	}
	if name, rest, ok := targetRoute(r.URL.EscapedPath(), targetKindAgent); ok {
		u, grp, authOK := g.auth.identify(r)
		g.logAuthEvent(authOK, authEventUserName(u), r)
		if !authOK {
			writeOAIError(sw, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
			return
		}
		g.handleTargetProxy(sw, r, u, grp, targetKindAgent, name, rest)
		return
	}

	if providerName, rest, ok := passthroughRoute(r.URL.EscapedPath()); ok {
		// providerPassthroughEnabled (providers.go, security+performance
		// audit 2026-08-22): a provider with Passthrough explicitly set
		// false is treated exactly like an unconfigured one here — the
		// request falls through to the same unknown-route 404 below,
		// without even reaching auth.identify.
		if _, known := g.adapters[providerName]; known && providerPassthroughEnabled(g.cfg, providerName) {
			u, grp, ok := g.auth.identify(r)
			g.logAuthEvent(ok, authEventUserName(u), r)
			if !ok {
				writeOAIError(sw, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
				return
			}
			g.handlePassthrough(sw, r, u, grp, providerName, rest)
			return
		}
	}

	if g.cfg.PassthroughUnknown {
		g.next.ServeHTTP(sw, r)
		return
	}
	writeOAIError(sw, http.StatusNotFound, "invalid_request_error", "unknown route")
}
