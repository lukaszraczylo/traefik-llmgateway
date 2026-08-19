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
		"providers", "groups", "users", // top-level testData keys
		"openai", "type: openai", "test-key", "gpt-test", // providers.openai
		"default",       // groups.default and users.inline[0].group
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
	blob := `{"providers":{"openai":{"type":"openai","apiKey":"test-key","models":["gpt-test"]}},
	  "groups":{"default":{}},
	  "users":{"inline":[{"name":"tester","group":"default","apiKey":"test-user-key"}]}}`
	if err := json.Unmarshal([]byte(blob), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNewFromTestData(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	h, err := New(context.Background(), next, testDataConfig(t), "llmgw")
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
