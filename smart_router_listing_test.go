package traefikllmgateway

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func routerListEntry(t *testing.T, cfg *Config, name string) map[string]any {
	t.Helper()
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	reg, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
	require.NoError(t, err)
	for _, entry := range reg.listFor(allowAllGroup()) {
		if entry["id"] == name {
			return entry
		}
	}
	t.Fatalf("listFor has no %q entry", name)
	return nil
}

func TestModelRegistry_ListFor_RouterAdvertisesSmallestTargetContext(t *testing.T) {
	t.Parallel()
	tiers := []SmartRouterTier{
		{Name: "low", Models: []string{"openai/big"}},
		{Name: "high", MinScore: 500, Models: []string{"openai/small", "openai/nometa", "openai/missing"}},
	}
	cases := []struct {
		name    string
		meta    map[string]*ModelMetaConfig
		aliases map[string]string
		tiers   []SmartRouterTier
		want    int
		known   bool
	}{
		{
			name:  "minimum across every tier, unknown and unresolved targets ignored",
			meta:  map[string]*ModelMetaConfig{"big": {ContextTokens: 200000}, "small": {ContextTokens: 32000}},
			tiers: tiers,
			want:  32000, known: true,
		},
		{
			name:  "router-level modelMeta override wins",
			meta:  map[string]*ModelMetaConfig{"big": {ContextTokens: 200000}, "small": {ContextTokens: 32000}, "smart": {ContextTokens: 16000}},
			tiers: tiers,
			want:  16000, known: true,
		},
		{
			name:    "alias target uses its own alias-level metadata",
			meta:    map[string]*ModelMetaConfig{"big": {ContextTokens: 200000}, "tiny": {ContextTokens: 8000}},
			aliases: map[string]string{"tiny": "openai/big"},
			tiers:   []SmartRouterTier{{Name: "low", Models: []string{"openai/big", "tiny"}}},
			want:    8000, known: true,
		},
		{
			name:  "no target with a known context omits the field",
			tiers: []SmartRouterTier{{Name: "low", Models: []string{"openai/nometa"}}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{
				Providers:    map[string]*ProviderConfig{"openai": {Models: []string{"big", "small", "nometa"}}},
				ModelMeta:    tc.meta,
				ModelAliases: tc.aliases,
				SmartRouters: map[string]*SmartRouterConfig{"smart": {Tiers: tc.tiers}},
			}
			entry := routerListEntry(t, cfg, "smart")
			if !tc.known {
				for _, k := range []string{"context_window", "context_length", "max_model_len"} {
					_, present := entry[k]
					assert.False(t, present, "%s must be omitted", k)
				}
				return
			}
			for _, k := range []string{"context_window", "context_length", "max_model_len"} {
				assert.Equal(t, tc.want, entry[k], k)
			}
		})
	}
}
