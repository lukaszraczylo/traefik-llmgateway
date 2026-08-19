// Package traefikllmgateway implements a Traefik Yaegi middleware plugin: a
// multi-provider LLM gateway with groups, per-user API keys, request/token/
// cost limits, and an MCP/A2A registry and proxy.
package traefikllmgateway

import (
	"context"
	"errors"
	"net/http"
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
	next http.Handler
	cfg  *Config
	name string
	// wired in later tasks: auth *authStore, limiter *limiter,
	// registry *modelRegistry, adapters map[string]providerAdapter
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
	return &Gateway{next: next, name: name, cfg: config}, nil
}

// ServeHTTP is the internal router. It grows in later tasks.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusTrackingWriter{ResponseWriter: w}
	defer recoverPanic(sw, g)
	if g.cfg.PassthroughUnknown {
		g.next.ServeHTTP(sw, r)
		return
	}
	writeOAIError(sw, http.StatusNotFound, "invalid_request_error", "unknown route")
}
