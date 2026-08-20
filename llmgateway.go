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
	Providers          map[string]*ProviderConfig `json:"providers,omitempty"`
	Groups             map[string]*GroupConfig    `json:"groups,omitempty"`
	Pricing            map[string]*ModelPricing   `json:"pricing,omitempty"`
	MCPServers         map[string]*TargetConfig   `json:"mcpServers,omitempty"`
	Agents             map[string]*AgentConfig    `json:"agents,omitempty"`
	Users              *UsersConfig               `json:"users,omitempty"`
	Redis              *RedisConfig               `json:"redis,omitempty"`
	PassthroughUnknown bool                       `json:"passthroughUnknown,omitempty"`
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
	Limits     *LimitsConfig `json:"limits,omitempty"`
	Providers  []string      `json:"providers,omitempty"`
	Models     []string      `json:"models,omitempty"`
	MCPServers []string      `json:"mcpServers,omitempty"`
	Agents     []string      `json:"agents,omitempty"`
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
	name     string
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

	lim, err := newConfiguredLimiter(config)
	if err != nil {
		return nil, err
	}
	lim.logf = g.errorf
	g.limiter = lim

	adapters, err := buildAdapters(config)
	if err != nil {
		return nil, err
	}
	g.adapters = adapters

	registry, err := newModelRegistry(adapters, config, g.errorf)
	if err != nil {
		return nil, err
	}
	warmCtx, warmCancel := context.WithTimeout(ctx, warmFillTimeout)
	defer warmCancel()
	registry.warmFill(warmCtx)
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

// newConfiguredLimiter builds the limiter for config, wiring config.Redis
// when set. A configured Redis block wires a respClient-backed redisStore
// behind AUTH/SELECT and resolveSecret'd credentials; otherwise the
// limiter runs on its in-process fallback alone. failOpen defaults to
// true (a Redis outage must not take the whole gateway down unless an
// operator opts into strict enforcement) unless RedisConfig.FailOpen is
// explicitly set.
func newConfiguredLimiter(config *Config) (*limiter, error) {
	if config.Redis == nil {
		return newLimiter(nil, true), nil
	}
	if config.Redis.Address == "" {
		return nil, errors.New("llmgateway: redis: address must not be empty")
	}
	if config.Redis.DB < 0 {
		return nil, fmt.Errorf("llmgateway: redis: db must not be negative, got %d", config.Redis.DB)
	}

	password, err := resolveSecret(config.Redis.Password)
	if err != nil {
		return nil, fmt.Errorf("llmgateway: redis: %w", err)
	}

	failOpen := true
	if config.Redis.FailOpen != nil {
		failOpen = *config.Redis.FailOpen
	}

	client := newRESPClient(config.Redis.Address, password, config.Redis.DB)
	return newLimiter(newRedisStore(client), failOpen), nil
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

	if g.cfg.PassthroughUnknown {
		g.next.ServeHTTP(sw, r)
		return
	}
	writeOAIError(sw, http.StatusNotFound, "invalid_request_error", "unknown route")
}
