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
	Type              string   `json:"type"`
	BaseURL           string   `json:"baseUrl,omitempty"`
	APIKey            string   `json:"apiKey"`
	DiscoveryInterval string   `json:"discoveryInterval,omitempty"`
	Models            []string `json:"models,omitempty"`
	Discovery         bool     `json:"discovery,omitempty"`
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
	Cache      *bool    `json:"cache,omitempty"`
	MCPServers []string `json:"mcpServers,omitempty"`
	Agents     []string `json:"agents,omitempty"`
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
}

// RedisConfig configures the distributed limit-state backend.
type RedisConfig struct {
	FailOpen *bool  `json:"failOpen,omitempty"`
	Address  string `json:"address"`
	Password string `json:"password,omitempty"`
	DB       int    `json:"db,omitempty"`
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

// ModelPricing overrides the built-in per-model price table.
type ModelPricing struct {
	InputPerM  float64 `json:"inputPerM"`
	OutputPerM float64 `json:"outputPerM"`
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
	name  string
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

	password, err := resolveSecret(rc.Password)
	if err != nil {
		return nil, fmt.Errorf("llmgateway: redis: %w", err)
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
		if _, known := g.adapters[providerName]; known {
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
