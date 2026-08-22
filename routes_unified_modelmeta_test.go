package traefikllmgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandleModels_ContextWindowAndPricing_FieldPresenceOmission is the
// HTTP-level counterpart to registry_modelmeta_test.go's listFor
// coverage: a real GET /v1/models request through the full Gateway,
// proving the OpenAI-compat-safe extension fields (feature v0.23:
// "context_window", "pricing") are present with the expected values for
// a model with known metadata, present-with-zeros for an explicitly free
// one, and entirely absent (not present as null or 0) for a model
// nothing knows anything about.
func TestHandleModels_ContextWindowAndPricing_FieldPresenceOmission(t *testing.T) {
	t.Parallel()
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", APIKey: "sk-test", Models: []string{"gpt-test", "free-model", "unknown-model"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "tester", Group: "default", APIKey: "sk-user"}}}
	cfg.ModelMeta = map[string]*ModelMetaConfig{
		"gpt-test":   {ContextTokens: 128000, InputCostPerMTokMicroUSD: 1_250_000, OutputCostPerMTokMicroUSD: 10_000_000},
		"free-model": {ContextTokens: 32768, Free: true},
	}

	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-user")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byID := make(map[string]map[string]any, len(body.Data))
	for _, entry := range body.Data {
		id, _ := entry["id"].(string)
		byID[id] = entry
	}

	priced, ok := byID["gpt-test"]
	if !ok {
		t.Fatalf("response missing gpt-test: %v", body.Data)
	}
	if priced["context_window"] != 128000.0 {
		t.Errorf(`gpt-test["context_window"] = %v, want 128000`, priced["context_window"])
	}
	pricing, ok := priced["pricing"].(map[string]any)
	if !ok || pricing["input_per_mtok_usd"] != 1.25 || pricing["output_per_mtok_usd"] != 10.0 {
		t.Errorf(`gpt-test["pricing"] = %v, want {input_per_mtok_usd:1.25 output_per_mtok_usd:10}`, priced["pricing"])
	}

	free, ok := byID["free-model"]
	if !ok {
		t.Fatalf("response missing free-model: %v", body.Data)
	}
	freePricing, ok := free["pricing"].(map[string]any)
	if !ok {
		t.Fatalf(`free-model["pricing"] missing, want present with zeros (explicit free, not absent)`)
	}
	if freePricing["input_per_mtok_usd"] != 0.0 || freePricing["output_per_mtok_usd"] != 0.0 {
		t.Errorf(`free-model["pricing"] = %v, want zeros`, freePricing)
	}

	unknown, ok := byID["unknown-model"]
	if !ok {
		t.Fatalf("response missing unknown-model: %v", body.Data)
	}
	if _, present := unknown["context_window"]; present {
		t.Errorf(`unknown-model["context_window"] present (%v), want entirely omitted`, unknown["context_window"])
	}
	if _, present := unknown["pricing"]; present {
		t.Errorf(`unknown-model["pricing"] present (%v), want entirely omitted`, unknown["pricing"])
	}
}
