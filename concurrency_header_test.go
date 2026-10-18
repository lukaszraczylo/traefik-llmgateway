package traefikllmgateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
		{"configured limit is advertised", `"concurrency";q=3;qu="concurrent-requests"`, 3},
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
			if got := rec.Header().Get("RateLimit-Policy"); got != tc.header {
				t.Fatalf("RateLimit-Policy = %q, want %q", got, tc.header)
			}
			if got := rec.Header().Get("X-Concurrency-Limit"); got != "" {
				t.Fatalf("legacy X-Concurrency-Limit = %q, want none", got)
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

func TestConcurrencyLearner_Learning(t *testing.T) {
	t.Parallel()
	resp := func(code int) *http.Response { return &http.Response{StatusCode: code} }
	tests := []struct {
		err      error
		name     string
		events   []*http.Response
		explicit int
		inFlight int
		want     int
	}{
		{nil, "503 halves in-flight", []*http.Response{resp(503)}, 0, 8, 4},
		{nil, "429 halves in-flight", []*http.Response{resp(429)}, 0, 7, 3},
		{nil, "504 halves in-flight", []*http.Response{resp(504)}, 0, 6, 3},
		{context.DeadlineExceeded, "timeout halves in-flight", []*http.Response{nil}, 0, 10, 5},
		{nil, "floor of one", []*http.Response{resp(503)}, 0, 1, 1},
		{nil, "never raised on overload", []*http.Response{resp(503), resp(503)}, 0, 8, 4},
		{nil, "success without learning stays unknown", []*http.Response{resp(200), resp(200), resp(200), resp(200)}, 0, 8, 0},
		{nil, "four successes add one", []*http.Response{resp(503), resp(200), resp(200), resp(200), resp(200)}, 0, 8, 5},
		{nil, "three successes add nothing", []*http.Response{resp(503), resp(200), resp(200), resp(200)}, 0, 8, 4},
		{nil, "overload resets the streak", []*http.Response{resp(503), resp(200), resp(200), resp(200), resp(503), resp(200)}, 0, 8, 4},
		{nil, "client error is neutral", []*http.Response{resp(503), resp(404), resp(404), resp(404), resp(404)}, 0, 8, 4},
		{nil, "explicit caps learned", []*http.Response{resp(503)}, 2, 8, 2},
		{nil, "learned below explicit wins", []*http.Response{resp(503)}, 6, 4, 2},
		{nil, "explicit alone", []*http.Response{resp(200)}, 3, 4, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newConcurrencyLearner(tc.explicit)
			for i := 0; i < tc.inFlight; i++ {
				c.begin("p")
			}
			for _, r := range tc.events {
				c.observe("p", r, tc.err)
			}
			if got := c.limit("p"); got != tc.want {
				t.Fatalf("limit = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestConcurrencyLearner_ClearsAtCeiling(t *testing.T) {
	t.Parallel()
	c := newConcurrencyLearner(0)
	for i := 0; i < 2*(learnedCeiling-1); i++ {
		c.begin("p")
	}
	c.observe("p", &http.Response{StatusCode: 503}, nil)
	if got := c.limit("p"); got != learnedCeiling-1 {
		t.Fatalf("limit = %d, want %d", got, learnedCeiling-1)
	}
	for i := 0; i < successesPerRegrow; i++ {
		c.observe("p", &http.Response{StatusCode: 200}, nil)
	}
	if got := c.limit("p"); got != 0 {
		t.Fatalf("limit = %d after reaching ceiling, want 0 (unknown)", got)
	}
}

func TestConcurrencyLearner_EndReleasesInFlight(t *testing.T) {
	t.Parallel()
	c := newConcurrencyLearner(0)
	end := c.begin("p")
	c.begin("p")
	end()
	end()
	c.observe("p", &http.Response{StatusCode: 503}, nil)
	if got := c.limit("p"); got != 1 {
		t.Fatalf("limit = %d, want 1 (one request still in flight, end is idempotent)", got)
	}
}

func newAutodetectGateway(t *testing.T, upstream string, autodetect bool, max int) http.Handler {
	t.Helper()
	cfg := CreateConfig()
	cfg.ConcurrencyAutodetect = autodetect
	cfg.MaxConcurrency = max
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: upstream, APIKey: "sk-up", Models: []string{"gpt-test"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func chatPolicyHeader(t *testing.T, h http.Handler) (int, string) {
	t.Helper()
	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	return rec.Code, rec.Header().Get("RateLimit-Policy")
}

func TestConcurrencyAutodetect_EndToEnd(t *testing.T) {
	t.Parallel()
	var status atomic.Int64
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	t.Run("overload then regrow", func(t *testing.T) {
		h := newAutodetectGateway(t, srv.URL, true, 0)
		status.Store(http.StatusOK)
		if _, got := chatPolicyHeader(t, h); got != "" {
			t.Fatalf("header before any overload = %q, want none", got)
		}
		for _, code := range []int64{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusGatewayTimeout} {
			status.Store(code)
			gotCode, got := chatPolicyHeader(t, h)
			if gotCode != int(code) {
				t.Fatalf("status = %d, want %d passed through", gotCode, code)
			}
			if want := `"concurrency";q=1;qu="concurrent-requests"`; got != want {
				t.Fatalf("header after %d = %q, want %q", code, got, want)
			}
		}
		status.Store(http.StatusOK)
		var got string
		for i := 0; i < successesPerRegrow; i++ {
			_, got = chatPolicyHeader(t, h)
		}
		if want := `"concurrency";q=2;qu="concurrent-requests"`; got != want {
			t.Fatalf("header after regrow = %q, want %q", got, want)
		}
	})

	t.Run("explicit caps learned", func(t *testing.T) {
		h := newAutodetectGateway(t, srv.URL, true, 3)
		status.Store(http.StatusOK)
		if _, got := chatPolicyHeader(t, h); got != `"concurrency";q=3;qu="concurrent-requests"` {
			t.Fatalf("header = %q, want explicit 3", got)
		}
		status.Store(http.StatusServiceUnavailable)
		if _, got := chatPolicyHeader(t, h); got != `"concurrency";q=1;qu="concurrent-requests"` {
			t.Fatalf("header after overload = %q, want learned 1", got)
		}
	})

	t.Run("autodetect off never learns", func(t *testing.T) {
		h := newAutodetectGateway(t, srv.URL, false, 0)
		status.Store(http.StatusServiceUnavailable)
		if _, got := chatPolicyHeader(t, h); got != "" {
			t.Fatalf("header = %q, want none with autodetect off", got)
		}
	})
}
