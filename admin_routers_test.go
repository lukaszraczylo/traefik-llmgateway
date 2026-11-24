package traefikllmgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func adminRouterTestConfig() *Config {
	cfg := newAdminTestConfig()
	cfg.SmartRouters = map[string]*SmartRouterConfig{
		"zauto": {Tiers: []SmartRouterTier{{Name: "only", Models: []string{"zeta/z-model"}}}},
		"auto": {
			DefaultTier: "big",
			Selection:   "hash",
			MaxAttempts: 3,
			Tiers: []SmartRouterTier{
				{Name: "small", MinScore: 0, Models: []string{"alpha/a-model-1"}},
				{Name: "big", MinScore: 400, Models: []string{"zeta/z-model", "a-model-2"}},
			},
			KeywordRules: []SmartRouterKeywordRule{{Tier: "big", Keywords: []string{"Prove"}}},
		},
	}
	return cfg
}

func TestAdminRouters_OverviewAndCatalog(t *testing.T) {
	t.Parallel()
	h, _ := newAdminGatewayHandle(t, adminRouterTestConfig())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	require.Equal(t, http.StatusOK, rec.Code)
	var ov adminOverviewResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ov))
	require.Len(t, ov.Routers, 2)
	assert.Equal(t, "auto", ov.Routers[0].Name, "sorted by name")
	auto := ov.Routers[0]
	assert.Equal(t, "big", auto.DefaultTier)
	assert.Equal(t, "hash", auto.Selection)
	assert.Equal(t, 3, auto.MaxAttempts)
	assert.True(t, auto.ContextEscalation)
	require.Len(t, auto.Tiers, 2)
	assert.Equal(t, adminRouterTierView{Name: "big", MinScore: 400, Models: []string{"zeta/z-model", "a-model-2"}}, auto.Tiers[1])
	require.Len(t, auto.KeywordRules, 1)
	assert.Equal(t, "big", auto.KeywordRules[0].Tier)
	assert.Equal(t, []string{"prove"}, auto.KeywordRules[0].Keywords)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminCatalogPath, "sk-admin1"))
	require.Equal(t, http.StatusOK, rec.Code)
	var cat adminCatalogResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cat))
	assert.Len(t, cat.Routers, 2)
	got := map[string][]string{}
	for _, p := range cat.Providers {
		for _, m := range p.Models {
			got[m.ID] = m.Routers
		}
	}
	assert.Equal(t, []string{"auto", "zauto"}, got["zeta/z-model"])
	assert.Equal(t, []string{"auto"}, got["alpha/a-model-1"])
	assert.Equal(t, []string{"auto"}, got["alpha/a-model-2"], "bare target name marks the model")
}

func TestAdminRouters_NoRoutersIsEmptyArray(t *testing.T) {
	t.Parallel()
	h, _ := newAdminGatewayHandle(t, newAdminTestConfig())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	assert.Contains(t, rec.Body.String(), `"routers":[]`)
}

func TestAdminRouters_RequiresAdmin(t *testing.T) {
	t.Parallel()
	h, _ := newAdminGatewayHandle(t, adminRouterTestConfig())
	for _, p := range []string{adminOverviewPath, adminCatalogPath} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(http.MethodGet, p, "sk-alice"))
		assert.NotEqual(t, http.StatusOK, rec.Code, p)
		assert.NotContains(t, rec.Body.String(), "zauto", p)
	}
}
