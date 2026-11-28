package traefikllmgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newModelsCatalogGateway(t *testing.T, cfg *Config) http.Handler {
	t.Helper()
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "tester", Group: "default", APIKey: "sk-user"}}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	require.NoError(t, err)
	return h
}

func TestHandleModels_MaxContextLength(t *testing.T) {
	t.Parallel()
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", APIKey: "sk-test", Models: []string{"known", "mystery-model", "big", "small", "shared"}},
		"other":  {Type: "openai", APIKey: "sk-test", Models: []string{"shared"}},
	}
	cfg.ModelMeta = map[string]*ModelMetaConfig{
		"known":         {ContextTokens: 128000},
		"openai/shared": {ContextTokens: 64000},
		"big":           {ContextTokens: 200000},
		"small":         {ContextTokens: 32000},
		"override":      {ContextTokens: 8000},
	}
	cfg.ModelAliases = map[string]string{
		"inherit":  "openai/known",
		"override": "openai/known",
	}
	cfg.SmartRouters = map[string]*SmartRouterConfig{
		"smart": {Tiers: []SmartRouterTier{{Name: "low", Models: []string{"openai/big", "openai/small"}}}},
	}
	h := newModelsCatalogGateway(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-user")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	byID := make(map[string]map[string]any, len(body.Data))
	for _, e := range body.Data {
		byID[e["id"].(string)] = e
	}

	cases := []struct {
		name string
		id   string
		want int
	}{
		{"modelMeta contextTokens set", "known", 128000},
		{"provider/model form", "openai/shared", 64000},
		{"provider/model form without metadata", "other/shared", 0},
		{"unknown omits the field", "mystery-model", 0},
		{"alias inherits target", "inherit", 128000},
		{"alias with own override", "override", 8000},
		{"smart router largest target window", "smart", 200000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, ok := byID[tc.id]
			require.True(t, ok, "missing %q in %v", tc.id, byID)
			got, present := entry["max_context_length"]
			if tc.want == 0 {
				assert.False(t, present, "max_context_length must be omitted, got %v", got)
				return
			}
			assert.Equal(t, float64(tc.want), got)
			assert.Equal(t, entry["context_window"], got)
		})
	}
}

// A downstream gateway reading this gateway's /v1/models through
// metadataPath must learn the same context values the producer advertises.
func TestHandleModels_MaxContextLength_RoundTripsThroughMetadataFetcher(t *testing.T) {
	t.Parallel()
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", APIKey: "sk-test", Models: []string{"known", "mystery-model"}},
	}
	cfg.ModelMeta = map[string]*ModelMetaConfig{"known": {ContextTokens: 128000}}
	cfg.ModelAliases = map[string]string{"inherit": "openai/known"}
	h := newModelsCatalogGateway(t, cfg)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer sk-user")
		h.ServeHTTP(w, r)
	}))
	defer upstream.Close()

	a := newOpenAIAdapter("down", upstream.URL, "sk-user")
	a.metadataPath = "/v1/models"
	got, err := a.fetchModelMetadata(context.Background())
	require.NoError(t, err)

	assert.Equal(t, map[string]int{
		"known":   128000,
		"inherit": 128000,
	}, got)
}
