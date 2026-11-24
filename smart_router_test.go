package traefikllmgateway

import (
	"strings"
	"testing"
)

func smartTestTiers() []SmartRouterTier {
	return []SmartRouterTier{
		{Name: "cheap", MinScore: 0, Models: []string{"openai/gpt-mini"}},
		{Name: "mid", MinScore: 300, Models: []string{"openai/gpt-mid"}},
		{Name: "top", MinScore: 600, Models: []string{"openai/gpt-top", "openai/gpt-top2"}},
	}
}

func smartTestConfig(mut func(*Config)) *Config {
	cfg := &Config{
		Providers: map[string]*ProviderConfig{
			"openai": {Models: []string{"gpt-mini", "gpt-mid", "gpt-top", "gpt-top2"}},
		},
		SmartRouters: map[string]*SmartRouterConfig{
			"smart/auto": {Tiers: smartTestTiers()},
		},
	}
	if mut != nil {
		mut(cfg)
	}
	return cfg
}

func newSmartTestRegistry(cfg *Config) (*modelRegistry, error) {
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	return newModelRegistry(adapters, cfg, func(string, ...any) {})
}

func TestNewModelRegistry_SmartRouters_Validation(t *testing.T) {
	t.Parallel()
	one := 1
	tests := []struct {
		name    string
		mut     func(*Config)
		wantErr string
	}{
		{name: "valid router", mut: nil},
		{name: "no routers", mut: func(c *Config) { c.SmartRouters = nil }},
		{
			name:    "nil router entry",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"] = nil },
			wantErr: "must not be nil",
		},
		{
			name: "name collides with provider prefix",
			mut: func(c *Config) {
				c.SmartRouters["openai/auto"] = &SmartRouterConfig{Tiers: []SmartRouterTier{{Name: "a", Models: []string{"x"}}}}
			},
			wantErr: "names a configured provider",
		},
		{
			name: "name collides with explicit model",
			mut: func(c *Config) {
				c.SmartRouters["gpt-mini"] = &SmartRouterConfig{Tiers: []SmartRouterTier{{Name: "a", Models: []string{"x"}}}}
			},
			wantErr: "explicitly configured model",
		},
		{
			name: "name collides with alias key",
			mut: func(c *Config) {
				c.ModelAliases = map[string]string{"smart/auto": "openai/gpt-mini"}
			},
			wantErr: "modelAliases key",
		},
		{
			name: "alias targeting a router",
			mut: func(c *Config) {
				c.ModelAliases = map[string]string{"my-alias": "smart/auto"}
			},
			wantErr: "smart router",
		},
		{
			name: "router targeting a router",
			mut: func(c *Config) {
				c.SmartRouters["other"] = &SmartRouterConfig{Tiers: []SmartRouterTier{{Name: "a", Models: []string{"smart/auto"}}}}
			},
			wantErr: "itself a router",
		},
		{
			name: "router targeting an alias is allowed",
			mut: func(c *Config) {
				c.ModelAliases = map[string]string{"my-alias": "openai/gpt-mini"}
				c.SmartRouters["smart/auto"].Tiers[0].Models = []string{"my-alias"}
			},
		},
		{
			name:    "invalid router name",
			mut:     func(c *Config) { c.SmartRouters["bad\x01name"] = &SmartRouterConfig{} },
			wantErr: "invalid",
		},
		{
			name:    "no tiers",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].Tiers = nil },
			wantErr: "at least one tier",
		},
		{
			name:    "empty tier models",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].Tiers[1].Models = nil },
			wantErr: "has no models",
		},
		{
			name:    "empty tier name",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].Tiers[1].Name = "" },
			wantErr: "invalid name",
		},
		{
			name:    "duplicate tier name",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].Tiers[1].Name = "cheap" },
			wantErr: "duplicate tier name",
		},
		{
			name:    "first tier minScore not zero",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].Tiers[0].MinScore = 10 },
			wantErr: "must have minScore 0",
		},
		{
			name:    "equal minScore",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].Tiers[1].MinScore = 0 },
			wantErr: "greater than the previous",
		},
		{
			name:    "descending minScore",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].Tiers[2].MinScore = 200 },
			wantErr: "greater than the previous",
		},
		{
			name:    "minScore above 1000",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].Tiers[2].MinScore = 1001 },
			wantErr: "between 0 and 1000",
		},
		{
			name:    "negative minScore",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].Tiers[2].MinScore = -1 },
			wantErr: "between 0 and 1000",
		},
		{
			name:    "minScore exactly 1000",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].Tiers[2].MinScore = 1000 },
			wantErr: "",
		},
		{
			name:    "duplicate target across tiers",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].Tiers[2].Models = []string{"openai/gpt-mini"} },
			wantErr: "tiers must be disjoint",
		},
		{
			name: "duplicate target within a tier",
			mut: func(c *Config) {
				c.SmartRouters["smart/auto"].Tiers[2].Models = []string{"openai/gpt-top", "openai/gpt-top"}
			},
			wantErr: "tiers must be disjoint",
		},
		{
			name:    "unknown defaultTier",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].DefaultTier = "nope" },
			wantErr: "defaultTier",
		},
		{
			name:    "known defaultTier",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].DefaultTier = "mid" },
			wantErr: "",
		},
		{
			name: "unknown rule tier",
			mut: func(c *Config) {
				c.SmartRouters["smart/auto"].KeywordRules = []SmartRouterKeywordRule{{Tier: "nope", Keywords: []string{"x"}}}
			},
			wantErr: "keywordRules[0] tier",
		},
		{
			name: "rule without keywords",
			mut: func(c *Config) {
				c.SmartRouters["smart/auto"].KeywordRules = []SmartRouterKeywordRule{{Tier: "top"}}
			},
			wantErr: "no keywords",
		},
		{
			name: "blank keyword",
			mut: func(c *Config) {
				c.SmartRouters["smart/auto"].KeywordRules = []SmartRouterKeywordRule{{Tier: "top", Keywords: []string{"  "}}}
			},
			wantErr: "empty keyword",
		},
		{
			name: "too many keywords",
			mut: func(c *Config) {
				kws := make([]string, maxSmartKeywords+1)
				for i := range kws {
					kws[i] = "kw" + strings.Repeat("x", i)
				}
				c.SmartRouters["smart/auto"].KeywordRules = []SmartRouterKeywordRule{{Tier: "top", Keywords: kws}}
			},
			wantErr: "more than 64 keywords",
		},
		{
			name:    "unknown selection",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].Selection = "random" },
			wantErr: "selection",
		},
		{
			name:    "negative maxAttempts",
			mut:     func(c *Config) { c.SmartRouters["smart/auto"].MaxAttempts = -1 },
			wantErr: "maxAttempts",
		},
		{
			name: "weight out of range",
			mut: func(c *Config) {
				bad := 1001
				c.SmartRouters["smart/auto"].Weights = &SmartRouterWeights{Code: &bad}
			},
			wantErr: "weight \"code\"",
		},
		{
			name: "valid weight",
			mut: func(c *Config) {
				c.SmartRouters["smart/auto"].Weights = &SmartRouterWeights{Code: &one}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := newSmartTestRegistry(smartTestConfig(tt.mut))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestValidateSmartRouters_AppliesDefaultsAndLowercasesKeywords(t *testing.T) {
	t.Parallel()
	cfg := smartTestConfig(func(c *Config) {
		c.SmartRouters["smart/auto"].KeywordRules = []SmartRouterKeywordRule{{Tier: "top", Keywords: []string{" Kubernetes ", "SQL"}}}
	})
	reg, err := newSmartTestRegistry(cfg)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	rt := reg.routers["smart/auto"]
	if rt == nil {
		t.Fatal("router smart/auto not compiled")
	}
	if rt.selection != smartSelectionOrdered || rt.maxAttempts != defaultSmartMaxAttempts || !rt.contextEscalation || rt.defaultRank != 0 {
		t.Errorf("defaults not applied: %+v", rt)
	}
	if rt.weights.code != defaultSmartWeightCode || rt.weights.simple != defaultSmartWeightSimple {
		t.Errorf("default weights not applied: %+v", rt.weights)
	}
	if len(rt.rules) != 1 || rt.rules[0].rank != 2 || rt.rules[0].keywords[0] != "kubernetes" || rt.rules[0].keywords[1] != "sql" {
		t.Errorf("rules = %+v, want one rule at rank 2 with lowercased trimmed keywords", rt.rules)
	}
	if len(rt.tiers) != 3 || rt.tiers[2].minScore != 600 || len(rt.tiers[2].models) != 2 {
		t.Errorf("tiers = %+v", rt.tiers)
	}
}

func TestValidateSmartRouters_ExplicitOverridesWin(t *testing.T) {
	t.Parallel()
	off := false
	zero := 0
	cfg := smartTestConfig(func(c *Config) {
		rc := c.SmartRouters["smart/auto"]
		rc.ContextEscalation = &off
		rc.MaxAttempts = 2
		rc.Selection = smartSelectionHash
		rc.DefaultTier = "mid"
		rc.Weights = &SmartRouterWeights{Code: &zero}
	})
	reg, err := newSmartTestRegistry(cfg)
	if err != nil {
		t.Fatalf("newModelRegistry: %v", err)
	}
	rt := reg.routers["smart/auto"]
	if rt.contextEscalation || rt.maxAttempts != 2 || rt.selection != smartSelectionHash || rt.defaultRank != 1 || rt.weights.code != 0 {
		t.Errorf("overrides not honoured: %+v", rt)
	}
}
