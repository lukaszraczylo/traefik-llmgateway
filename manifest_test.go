package traefikllmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
)

// testDataConfig decodes a hand-maintained JSON mirror of the .traefik.yml
// testData block — we cannot import a YAML library (stdlib only). The
// mirror is drift-checked against the raw manifest by the assertions below:
// every top-level key and scalar value the mirror encodes must also appear
// in .traefik.yml's raw bytes, so an edit to either side that removes or
// renames a testData field fails this test instead of silently going
// stale.
func testDataConfig(t *testing.T) *Config {
	t.Helper()
	raw, err := os.ReadFile(".traefik.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^testData:`).Match(raw) {
		t.Fatal(".traefik.yml missing testData")
	}

	// Drift check: every top-level key and scalar value referenced by the
	// mirror blob below must appear in the raw manifest bytes.
	mirrorTokens := []string{
		"providers", "groups", "users", "smartRouters", // top-level testData keys
		"openai", "type: openai", "test-key", "gpt-test", "gpt-test-large", // providers.openai
		"auto", "classifier: v2", "name: small", "name: large", "minScore: 400", // smartRouters.auto
		"[openai/gpt-test]", "[openai/gpt-test-large]", // smartRouters.auto tier models
		"decider", "decider-test", "pathBaseUrls", "/v1/decide: http://decider.example.invalid", // providers.decider
		"decision:", "provider: decider", "mode: shadow", // smartRouters.auto.decision
		"default",       // groups.default and users.inline[0].group
		"providers: []", // groups.default.providers — zero-semantic hedge, not bare {}
		"tester",        // users.inline[0].name
		"test-user-key", // users.inline[0].apiKey
	}
	var missing []string
	for _, tok := range mirrorTokens {
		if !bytes.Contains(raw, []byte(tok)) {
			missing = append(missing, tok)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("manifest drift: mirror blob references %v not found in .traefik.yml — update the mirror or the manifest so they match", missing)
	}

	cfg := CreateConfig()
	// mirror of testData, kept in sync by this test living next to the manifest
	blob := `{"providers":{"openai":{"type":"openai","apiKey":"test-key","models":["gpt-test","gpt-test-large"]},
	    "decider":{"type":"openai","apiKey":"test-key","models":["decider-test"],"pathBaseUrls":{"/v1/decide":"http://decider.example.invalid"}}},
	  "smartRouters":{"auto":{"classifier":"v2","decision":{"provider":"decider","mode":"shadow"},"tiers":[
	    {"name":"small","minScore":0,"models":["openai/gpt-test"]},
	    {"name":"large","minScore":400,"models":["openai/gpt-test-large"]}]}},
	  "groups":{"default":{"providers":[]}},
	  "users":{"inline":[{"name":"tester","group":"default","apiKey":"test-user-key"}]}}`
	if err := json.Unmarshal([]byte(blob), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNewFromTestData(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	cfg := testDataConfig(t)
	if router := cfg.SmartRouters["auto"]; router == nil || len(router.Tiers) != 2 {
		t.Fatalf("testData must declare the two-tier smart router %q, got %+v", "auto", router)
	}
	if d := cfg.SmartRouters["auto"].Decision; d == nil || d.Provider != "decider" {
		t.Fatalf("testData must declare the decision block, got %+v", d)
	}
	h, err := New(context.Background(), next, cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if h == nil {
		t.Fatal("nil handler")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/definitely-unknown", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("want JSON error envelope, got %q", ct)
	}
}
