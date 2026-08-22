package traefikllmgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAdminOverview_ModelMeta_KnownAndUnknownFields proves GET
// /admin/api/overview's per-provider ModelMeta map (feature v0.23)
// carries a resolved entry for every known model, with unknown fields
// omitted (nil pointers) rather than zeroed.
func TestAdminOverview_ModelMeta_KnownAndUnknownFields(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.ModelMeta = map[string]*ModelMetaConfig{
		"alpha/a-model-1": {ContextTokens: 128000, InputCostPerMTokMicroUSD: 1_250_000, OutputCostPerMTokMicroUSD: 10_000_000},
	}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var alpha *adminProviderView
	for i := range got.Providers {
		if got.Providers[i].Name == "alpha" {
			alpha = &got.Providers[i]
		}
	}
	if alpha == nil {
		t.Fatalf("providers missing alpha: %+v", got.Providers)
	}

	known, ok := alpha.ModelMeta["a-model-1"]
	if !ok {
		t.Fatalf("alpha.ModelMeta missing a-model-1: %+v", alpha.ModelMeta)
	}
	if known.ContextTokens == nil || *known.ContextTokens != 128000 {
		t.Errorf("a-model-1 ContextTokens = %v, want *128000", known.ContextTokens)
	}
	if known.InputPerMTokUSD == nil || *known.InputPerMTokUSD != 1.25 {
		t.Errorf("a-model-1 InputPerMTokUSD = %v, want *1.25", known.InputPerMTokUSD)
	}
	if known.OutputPerMTokUSD == nil || *known.OutputPerMTokUSD != 10 {
		t.Errorf("a-model-1 OutputPerMTokUSD = %v, want *10", known.OutputPerMTokUSD)
	}

	unknown, ok := alpha.ModelMeta["a-model-2"]
	if !ok {
		t.Fatalf("alpha.ModelMeta missing a-model-2 entry entirely (must be present, just all-unknown): %+v", alpha.ModelMeta)
	}
	if unknown.ContextTokens != nil || unknown.InputPerMTokUSD != nil || unknown.OutputPerMTokUSD != nil {
		t.Errorf("a-model-2 (nothing known) = %+v, want every field nil", unknown)
	}
}

// TestAdminOverview_AliasModelMeta_Inherited proves GET
// /admin/api/overview's alias rows carry the alias's INHERITED metadata
// from its target (feature v0.23, hover-detail refinement).
func TestAdminOverview_AliasModelMeta_Inherited(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.ModelAliases = map[string]string{"aliased/coding": "alpha/a-model-1"}
	cfg.ModelMeta = map[string]*ModelMetaConfig{
		"alpha/a-model-1": {ContextTokens: 200000, Free: true},
	}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var got adminOverviewResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var alias *adminAliasView
	for i := range got.Aliases {
		if got.Aliases[i].Alias == "aliased/coding" {
			alias = &got.Aliases[i]
		}
	}
	if alias == nil {
		t.Fatalf("aliases missing aliased/coding: %+v", got.Aliases)
	}
	if alias.ModelMeta.ContextTokens == nil || *alias.ModelMeta.ContextTokens != 200000 {
		t.Errorf("alias ContextTokens = %v, want *200000 (inherited)", alias.ModelMeta.ContextTokens)
	}
	if alias.ModelMeta.InputPerMTokUSD == nil || *alias.ModelMeta.InputPerMTokUSD != 0 {
		t.Errorf("alias InputPerMTokUSD = %v, want *0 (inherited free)", alias.ModelMeta.InputPerMTokUSD)
	}
}
