package traefikllmgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newConcurrencyTestGateway(t *testing.T, maxConcurrency int) (http.Handler, error) {
	t.Helper()
	cfg := CreateConfig()
	cfg.MaxConcurrency = maxConcurrency
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", APIKey: "sk-test", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "tester", Group: "default", APIKey: "sk-user"}}}
	return New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
}

func TestMaxConcurrency_AdvertisedOnResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		header string
		max    int
	}{
		{"configured limit is advertised", "3", 3},
		{"zero sends no header", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, err := newConcurrencyTestGateway(t, tc.max)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			req.Header.Set("Authorization", "Bearer sk-user")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if got := rec.Header().Get("X-Concurrency-Limit"); got != tc.header {
				t.Fatalf("X-Concurrency-Limit = %q, want %q", got, tc.header)
			}
		})
	}
}

func TestMaxConcurrency_NegativeRejected(t *testing.T) {
	t.Parallel()
	if _, err := newConcurrencyTestGateway(t, -1); err == nil {
		t.Fatal("New accepted a negative maxConcurrency")
	}
}
