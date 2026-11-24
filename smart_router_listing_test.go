package traefikllmgateway

import (
	"fmt"
	"strings"
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

func TestModelRegistry_ListFor_RouterAdvertisesTargetContext(t *testing.T) {
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
		escOff  bool
	}{
		{
			name:  "escalation on advertises the maximum, unknown and unresolved targets ignored",
			meta:  map[string]*ModelMetaConfig{"big": {ContextTokens: 200000}, "small": {ContextTokens: 32000}},
			tiers: tiers,
			want:  200000, known: true,
		},
		{
			name:  "escalation off advertises the minimum",
			meta:  map[string]*ModelMetaConfig{"big": {ContextTokens: 200000}, "small": {ContextTokens: 32000}},
			tiers: tiers, escOff: true,
			want: 32000, known: true,
		},
		{
			name:  "router-level modelMeta override wins with escalation on",
			meta:  map[string]*ModelMetaConfig{"big": {ContextTokens: 200000}, "small": {ContextTokens: 32000}, "smart": {ContextTokens: 16000}},
			tiers: tiers,
			want:  16000, known: true,
		},
		{
			name:  "router-level modelMeta override wins with escalation off",
			meta:  map[string]*ModelMetaConfig{"big": {ContextTokens: 200000}, "small": {ContextTokens: 32000}, "smart": {ContextTokens: 16000}},
			tiers: tiers, escOff: true,
			want: 16000, known: true,
		},
		{
			name:    "alias target uses its own alias-level metadata",
			meta:    map[string]*ModelMetaConfig{"big": {ContextTokens: 200000}, "tiny": {ContextTokens: 8000}},
			aliases: map[string]string{"tiny": "openai/big"},
			tiers:   []SmartRouterTier{{Name: "low", Models: []string{"openai/big", "tiny"}}},
			escOff:  true,
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
			if tc.escOff {
				off := false
				cfg.SmartRouters["smart"].ContextEscalation = &off
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

func TestModelRegistry_RouterOverrideAboveLargestTargetWarnsOnce(t *testing.T) {
	cfg := &Config{
		Providers:    map[string]*ProviderConfig{"openai": {Models: []string{"big", "small"}}},
		ModelMeta:    map[string]*ModelMetaConfig{"big": {ContextTokens: 200000}, "small": {ContextTokens: 32000}, "smart": {ContextTokens: 500000}},
		SmartRouters: map[string]*SmartRouterConfig{"smart": {Tiers: []SmartRouterTier{{Name: "low", Models: []string{"openai/big", "openai/small"}}}}},
	}
	var logs []string
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	reg, err := newModelRegistry(adapters, cfg, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		reg.listFor(allowAllGroup())
	}
	var warns int
	for _, l := range logs {
		if strings.Contains(l, "exceeds the largest known target window") {
			warns++
		}
	}
	assert.Equal(t, 1, warns, "%v", logs)
}

func TestModelRegistry_RouterOverrideWithinLargestTargetDoesNotWarn(t *testing.T) {
	cfg := &Config{
		Providers:    map[string]*ProviderConfig{"openai": {Models: []string{"big"}}},
		ModelMeta:    map[string]*ModelMetaConfig{"big": {ContextTokens: 200000}, "smart": {ContextTokens: 200000}},
		SmartRouters: map[string]*SmartRouterConfig{"smart": {Tiers: []SmartRouterTier{{Name: "low", Models: []string{"openai/big"}}}}},
	}
	var logs []string
	adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
	reg, err := newModelRegistry(adapters, cfg, func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })
	require.NoError(t, err)
	reg.listFor(allowAllGroup())
	for _, l := range logs {
		assert.NotContains(t, l, "exceeds the largest known target window")
	}
}

func TestNewModelRegistry_RouterModelMetaOverrideMustBePositive(t *testing.T) {
	t.Parallel()
	for _, v := range []int{0, -1} {
		cfg := &Config{
			Providers:    map[string]*ProviderConfig{"openai": {Models: []string{"big"}}},
			ModelMeta:    map[string]*ModelMetaConfig{"smart": {ContextTokens: v}},
			SmartRouters: map[string]*SmartRouterConfig{"smart": {Tiers: []SmartRouterTier{{Name: "low", Models: []string{"openai/big"}}}}},
		}
		adapters := map[string]providerAdapter{"openai": newFakeAdapter("openai")}
		_, err := newModelRegistry(adapters, cfg, func(string, ...any) {})
		require.Error(t, err, "contextTokens %d", v)
	}
}
