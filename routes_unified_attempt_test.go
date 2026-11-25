package traefikllmgateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bodyRecorder is an upstream that records every request body it receives.
type bodyRecorder struct {
	bodies []string
	mu     sync.Mutex
}

func (b *bodyRecorder) add(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bodies = append(b.bodies, s)
}

func (b *bodyRecorder) all() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.bodies...)
}

func recordingServer(status int, body string, rec *bodyRecorder) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		rec.add(string(raw))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func newCachedFailoverGateway(t *testing.T, alpha, beta *httptest.Server) (http.Handler, *Gateway) {
	t.Helper()
	redisLn := newBehavioralRedisServer(t)
	cfg := twoProviderFailoverConfig(alpha, beta)
	cfg.ModelAliases = map[string]string{"fast": "shared"}
	cfg.Redis = &RedisConfig{Address: redisLn.Addr().String()}
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m", MaxBodyBytes: 1 << 20}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	require.NoError(t, err)
	gw, ok := h.(*Gateway)
	require.True(t, ok)
	return h, gw
}

// Golden values were captured from the pre-refactor implementation, where
// the model was rewritten once on pristineReq. The per-attempt rewrite must
// produce the same bytes.
const (
	goldenKeyNormal   = "llmgw:cache:77de5fc75c72e1181bf99a80040173eb30519d80e71b5ba6008ad4a3821128a3"
	goldenKeyFailover = "llmgw:cache:57d6f218ff1c7343e0b6af6d061c0256e9130c9b86eb79de224d0b736a2bbac9"
	goldenKeyAlias    = "llmgw:cache:a0e6793644ed6a16c2d46ce15f03b82b4d87d1cca3ef4a8756ed2857c86a9c70"
)

func TestCacheKeyPinned_NormalFailoverAndAlias(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		wantProvider string
		golden       string
		alphaStatus  int
	}{
		{name: "normal", model: "shared", alphaStatus: http.StatusOK, wantProvider: "alpha", golden: goldenKeyNormal},
		{name: "failover_beta_wins", model: "shared", alphaStatus: http.StatusInternalServerError, wantProvider: "beta", golden: goldenKeyFailover},
		{name: "alias_failover", model: "fast", alphaStatus: http.StatusInternalServerError, wantProvider: "beta", golden: goldenKeyAlias},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var aRec, bRec bodyRecorder
			alpha := recordingServer(tt.alphaStatus, successRespBody, &aRec)
			defer alpha.Close()
			beta := recordingServer(http.StatusOK, successRespBody, &bRec)
			defer beta.Close()
			h, gw := newCachedFailoverGateway(t, alpha, beta)

			msgs := []any{map[string]any{"role": "user", "content": "hi"}}
			req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", map[string]any{"model": tt.model, "messages": msgs})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, "miss", rec.Header().Get("X-Llmgw-Cache"))

			want := cacheKey(tt.wantProvider, "shared", tt.model, cacheEndpointChat, map[string]any{"model": "shared", "messages": msgs})
			assert.Equal(t, tt.golden, want, "key must stay byte-identical to the pre-refactor value")
			_, hit := gw.cache.lookup(want)
			assert.True(t, hit, "response must be stored under the pinned key")
		})
	}
}

func TestFailover_NoStreamOptionsOrAliasKeyLeakToUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non_stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var aRec, bRec bodyRecorder
			alpha := recordingServer(http.StatusInternalServerError, `{"error":{"message":"down","type":"server_error"}}`, &aRec)
			defer alpha.Close()
			beta := recordingServer(http.StatusOK, successRespBody, &bRec)
			defer beta.Close()

			cfg := twoProviderFailoverConfig(alpha, beta)
			h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
			require.NoError(t, err)

			body := map[string]any{"model": "shared", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			if stream {
				body["stream"] = true
			}
			req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			aBodies, bBodies := aRec.all(), bRec.all()
			require.Len(t, aBodies, 1)
			require.Len(t, bBodies, 1)
			for _, b := range append(aBodies, bBodies...) {
				assert.NotContains(t, b, gatewayAliasKey)
				assert.Contains(t, b, `"model":"shared"`)
			}
			assert.JSONEq(t, aBodies[0], bBodies[0], "failover attempt must send the same upstream body as the first attempt")
			if !stream {
				assert.NotContains(t, bBodies[0], "stream_options", "a non-streaming client never gets stream_options injected")
			}
		})
	}
}

func TestFailover_SameModelServedByBothProviders(t *testing.T) {
	var aRec, bRec bodyRecorder
	alpha := recordingServer(http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`, &aRec)
	defer alpha.Close()
	beta := recordingServer(http.StatusOK, successRespBody, &bRec)
	defer beta.Close()

	cfg := twoProviderFailoverConfig(alpha, beta)
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	require.NoError(t, err)

	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", map[string]any{"model": "shared", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, successRespBody, rec.Body.String())
	assert.Len(t, aRec.all(), 1)
	assert.Len(t, bRec.all(), 1)
}
