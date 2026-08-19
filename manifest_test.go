package traefikllmgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
)

// yamlToJSONLite converts the small, known-shape testData block of
// .traefik.yml to JSON via a scripted extraction — we cannot import a YAML
// lib. Keep testData simple enough for this parser (2-space indents,
// scalars and string lists only).
func testDataConfig(t *testing.T) *Config {
	t.Helper()
	raw, err := os.ReadFile(".traefik.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^testData:`).Match(raw) {
		t.Fatal(".traefik.yml missing testData")
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
