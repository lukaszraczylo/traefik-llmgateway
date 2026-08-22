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
)

// Config is the plugin's dynamic configuration, populated by Traefik from
// the middleware's YAML/testData. Every limit or list field follows the
// convention: zero/empty/omitted means unlimited/all.
type Config struct {
	Providers  map[string]*ProviderConfig `json:"providers,omitempty"`
	Groups     map[string]*GroupConfig    `json:"groups,omitempty"`
	Pricing    map[string]*ModelPricing   `json:"pricing,omitempty"`
	MCPServers map[string]*TargetConfig   `json:"mcpServers,omitempty"`
	Agents     map[string]*AgentConfig    `json:"agents,omitempty"`
	Users      *UsersConfig               `json:"users,omitempty"`
	Redis      *RedisConfig               `json:"redis,omitempty"`
	// Admin gates the read-only admin dashboard (spec §4, v0.2): nil or
	// Admin.Enabled false means the /admin* routes are not registered at
	// all — ServeHTTP falls through to its existing 404/passthroughUnknown
	// handling for those paths, preserving v0.1 behavior exactly.
	Admin *AdminConfig `json:"admin,omitempty"`
	// ModelAliases maps an operator-defined alias id to a target model id
	// (spec §5, v0.2) — e.g. {"aliased/coding": "anthropic/claude-sonnet-4-5"}.
	// An exact alias match wins resolution before any other rule
	// (modelRegistry.resolve, registry.go); the target then resolves
	// through the normal rules. nil/omitted preserves v0.1 behavior
	// exactly: resolve never consults an empty alias map, so no existing
	// model id's resolution changes. Validated at construction
	// (validateModelAliases, registry.go).
	ModelAliases map[string]string `json:"modelAliases,omitempty"`
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
	// Retry is a struct value, not a pointer, because its own Enabled
	// field is the on/off signal (unlike Redis/Users, where the block's
	// mere presence is the signal) — so its tag omits "omitempty":
	// encoding/json never treats a struct value as "empty" regardless of
	// its fields, so "omitempty" here would be a no-op that misleadingly
	// implies otherwise.
	Retry RetryConfig `json:"retry"`
	// Cache is a struct value, not a pointer, for the same reason as Retry
	// above: its own Enabled field is the on/off signal.
	Cache              CacheConfig `json:"cache"`
	PassthroughUnknown bool        `json:"passthroughUnknown,omitempty"`
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

// Gateway is the Traefik middleware handler.
type Gateway struct {
	next     http.Handler
	cfg      *Config
	auth     *authStore
	limiter  *limiter
	registry *modelRegistry
	adapters map[string]providerAdapter
	// targetClient is the shared, connection-pooled *http.Client the
	// MCP/A2A target proxy (mcp_a2a.go) issues every upstream request
	// through — built once via newAdapterHTTPClient, the same constructor
	// each provider adapter uses for its own client.
	targetClient *http.Client
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
	name        string
}

// New creates the middleware. NOTE: no tail call — Yaegi zeroes
// multi-value tail-call returns across the reflect boundary.
func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	g, err := newGateway(ctx, next, config, name)
	if err != nil {
		return nil, err
	}
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
