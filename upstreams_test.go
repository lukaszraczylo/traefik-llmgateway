package traefikllmgateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPassthroughBase_Resolution(t *testing.T) {
	a := newOpenAIAdapter("gx10", "http://base", "")
	a.modelBases = map[string]string{"m1": "http://m1"}
	a.pathBases = map[string]string{
		"/v1/decide":       "http://decide",
		"/v1/decide/batch": "http://batch",
	}
	tests := []struct {
		name, model, path, want string
	}{
		{"model hit bare", "m1", "/v1/rerank", "http://m1"},
		{"model hit prefixed", "gx10/m1", "/v1/rerank", "http://m1"},
		{"model beats path", "m1", "/v1/decide", "http://m1"},
		{"model miss falls to base", "other", "/v1/rerank", "http://base"},
		{"other provider prefix is not stripped", "x/m1", "/v1/rerank", "http://base"},
		{"path exact", "", "/v1/decide", "http://decide"},
		{"path longest prefix wins", "", "/v1/decide/batch", "http://batch"},
		{"path sub-segment uses shorter prefix", "", "/v1/decide/other", "http://decide"},
		{"path segment boundary respected", "", "/v1/decider", "http://base"},
		{"path miss", "", "/v1/chat/completions", "http://base"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, a.passthroughBase(tc.model, tc.path))
		})
	}
}

func TestBaseForCtx(t *testing.T) {
	a := newOpenAIAdapter("gx10", "http://base", "")
	assert.Equal(t, "http://base", a.baseForCtx(withUpstreamModel(context.Background(), "m1")))
	a.modelBases = map[string]string{"m1": "http://m1"}
	assert.Equal(t, "http://m1", a.baseForCtx(withUpstreamModel(context.Background(), "m1")))
	assert.Equal(t, "http://base", a.baseForCtx(withUpstreamModel(context.Background(), "m2")))
	assert.Equal(t, "http://base", a.baseForCtx(context.Background()))
}

func TestBuildAdapters_UpstreamURLValidation(t *testing.T) {
	tests := []struct {
		name    string
		wantErr string
		pc      ProviderConfig
	}{
		{"valid", "", ProviderConfig{Type: "openai", ModelBaseURLs: map[string]string{"m": "http://h:8000/"}, PathBaseURLs: map[string]string{"/v1/decide": "https://h"}}},
		{"malformed model url", "modelBaseUrls", ProviderConfig{Type: "openai", ModelBaseURLs: map[string]string{"m": "http://[::1"}}},
		{"no scheme", "absolute http(s) URL", ProviderConfig{Type: "openai", ModelBaseURLs: map[string]string{"m": "host:8000"}}},
		{"bad scheme", "absolute http(s) URL", ProviderConfig{Type: "openai", ModelBaseURLs: map[string]string{"m": "ftp://h"}}},
		{"userinfo", "userinfo", ProviderConfig{Type: "openai", ModelBaseURLs: map[string]string{"m": "http://u:p@h"}}}, //nolint:gosec // deliberately credential-bearing input
		{"query", "userinfo, query or fragment", ProviderConfig{Type: "openai", PathBaseURLs: map[string]string{"/p": "http://h?x=1"}}},
		{"empty model key", "empty key", ProviderConfig{Type: "openai", ModelBaseURLs: map[string]string{"": "http://h"}}},
		{"path key without slash", "must start with", ProviderConfig{Type: "openai", PathBaseURLs: map[string]string{"v1/decide": "http://h"}}},
		{"bad path url", "pathBaseUrls", ProviderConfig{Type: "openai", PathBaseURLs: map[string]string{"/p": "::"}}},
		{"non-openai type", "type \"openai\" only", ProviderConfig{Type: "anthropic", APIKey: "k", ModelBaseURLs: map[string]string{"m": "http://h"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pc := tc.pc
			adapters, err := buildAdapters(&Config{Providers: map[string]*ProviderConfig{"p": &pc}})
			if tc.wantErr == "" {
				require.NoError(t, err)
				oa, ok := adapters["p"].(*openaiAdapter)
				require.True(t, ok)
				assert.Equal(t, "http://h:8000", oa.modelBases["m"])
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), "u:p@", "error must not echo the URL")
		})
	}
}

func TestBuildAdapters_NoUpstreamFields_Unchanged(t *testing.T) {
	adapters, err := buildAdapters(&Config{Providers: map[string]*ProviderConfig{
		"p": {Type: "openai", BaseURL: "http://base/"},
	}})
	require.NoError(t, err)
	oa, ok := adapters["p"].(*openaiAdapter)
	require.True(t, ok)
	assert.Nil(t, oa.modelBases)
	assert.Nil(t, oa.pathBases)
	assert.False(t, oa.hasUpstreamOverrides())
	assert.Equal(t, "http://base", oa.baseForCtx(withUpstreamModel(context.Background(), "any")))
	assert.Equal(t, "http://base", oa.passthroughBase("any", "/v1/decide"))
}

type upstreamHit struct{ server, path, auth, model string }

type upstreamRecorder struct {
	hits []upstreamHit
	mu   sync.Mutex
}

func (u *upstreamRecorder) handler(server, respContentType, respBody string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		model := ""
		if i := strings.Index(string(b), `"model":"`); i >= 0 {
			rest := string(b)[i+len(`"model":"`):]
			model = rest[:strings.Index(rest, `"`)]
		}
		u.mu.Lock()
		u.hits = append(u.hits, upstreamHit{server, r.URL.Path, r.Header.Get("Authorization"), model})
		u.mu.Unlock()
		w.Header().Set("Content-Type", respContentType)
		_, _ = w.Write([]byte(respBody))
	}
}

func (u *upstreamRecorder) snapshot() []upstreamHit {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]upstreamHit(nil), u.hits...)
}

func newUpstreamsGateway(t *testing.T, pc *ProviderConfig) http.Handler {
	t.Helper()
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{"gx10": pc}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	require.NoError(t, err)
	return h
}

const upstreamsSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"

func TestUpstreams_StreamingChat_TwoModelsTwoServers(t *testing.T) {
	rec := &upstreamRecorder{}
	base := httptest.NewServer(rec.handler("base", "text/event-stream", upstreamsSSE))
	defer base.Close()
	qwen := httptest.NewServer(rec.handler("qwen", "text/event-stream", upstreamsSSE))
	defer qwen.Close()
	embed := httptest.NewServer(rec.handler("embed", "application/json", `{"data":[],"usage":{"prompt_tokens":1}}`))
	defer embed.Close()

	h := newUpstreamsGateway(t, &ProviderConfig{
		Type: "openai", BaseURL: base.URL, APIKey: "sk-up", Models: []string{"current"},
		ModelBaseURLs: map[string]string{"Qwen3.8-27B": qwen.URL, "e5": embed.URL},
	})

	chat := func(model string) string {
		req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice",
			map[string]any{"model": model, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		return w.Body.String()
	}
	assert.Contains(t, chat("gx10/Qwen3.8-27B"), "[DONE]")
	assert.Contains(t, chat("gx10/current"), "[DONE]")

	req := newUnifiedRequest(t, http.MethodPost, "/v1/embeddings", "sk-alice", map[string]any{"model": "gx10/e5", "input": "x"})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, []upstreamHit{
		{"qwen", "/v1/chat/completions", "Bearer sk-up", "Qwen3.8-27B"},
		{"base", "/v1/chat/completions", "Bearer sk-up", "current"},
		{"embed", "/v1/embeddings", "Bearer sk-up", "e5"},
	}, rec.snapshot())

	// ModelBaseURLs ids are declared models: they appear in /v1/models.
	mreq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	mreq.Header.Set("Authorization", "Bearer sk-alice")
	mw := httptest.NewRecorder()
	h.ServeHTTP(mw, mreq)
	require.Equal(t, http.StatusOK, mw.Code)
	assert.Contains(t, mw.Body.String(), `"id":"Qwen3.8-27B"`)
	assert.Contains(t, mw.Body.String(), `"id":"e5"`)
}

func TestUpstreams_Passthrough_ModelAndPath(t *testing.T) {
	rec := &upstreamRecorder{}
	base := httptest.NewServer(rec.handler("base", "application/json", `{}`))
	defer base.Close()
	rr := httptest.NewServer(rec.handler("rerank", "application/json", `{}`))
	defer rr.Close()
	thx := httptest.NewServer(rec.handler("thx", "application/json", `{}`))
	defer thx.Close()

	h := newUpstreamsGateway(t, &ProviderConfig{
		Type: "openai", BaseURL: base.URL, Models: []string{"current"},
		ModelBaseURLs: map[string]string{"bge": rr.URL},
		PathBaseURLs:  map[string]string{"/v1/decide": thx.URL},
	})
	post := func(path, body string) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-alice")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	post("/gx10/v1/rerank", `{"model":"gx10/bge","query":"q"}`)
	post("/gx10/v1/rerank", `{"model":"bge","query":"q"}`)
	post("/gx10/v1/decide", `{"x":1}`)
	post("/gx10/v1/decide/batch", `{"x":1}`)
	post("/gx10/v1/other", `{"model":"current"}`)

	var servers, paths []string
	for _, hit := range rec.snapshot() {
		servers = append(servers, hit.server)
		paths = append(paths, hit.path)
	}
	assert.Equal(t, []string{"rerank", "rerank", "thx", "thx", "base"}, servers)
	assert.Equal(t, []string{"/v1/rerank", "/v1/rerank", "/v1/decide", "/v1/decide/batch", "/v1/other"}, paths)
}

func TestUpstreams_Passthrough_PathOnly(t *testing.T) {
	rec := &upstreamRecorder{}
	base := httptest.NewServer(rec.handler("base", "application/json", `{}`))
	defer base.Close()
	thx := httptest.NewServer(rec.handler("thx", "application/json", `{}`))
	defer thx.Close()

	h := newUpstreamsGateway(t, &ProviderConfig{
		Type: "openai", BaseURL: base.URL, Models: []string{"current"},
		PathBaseURLs: map[string]string{"/v1/decide": thx.URL},
	})
	post := func(path, body string) {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-alice")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	post("/gx10/v1/decide", `{"model":"current"}`)
	post("/gx10/v1/other", `{"model":"current"}`)

	var servers []string
	for _, hit := range rec.snapshot() {
		servers = append(servers, hit.server)
	}
	assert.Equal(t, []string{"thx", "base"}, servers)
}

type failingBody struct{ read bool }

func (f *failingBody) Read(p []byte) (int, error) {
	if !f.read {
		f.read = true
		return copy(p, `{"mod`), nil
	}
	return 0, io.ErrClosedPipe
}

func TestUpstreams_Passthrough_BodyReadError_400(t *testing.T) {
	rec := &upstreamRecorder{}
	base := httptest.NewServer(rec.handler("base", "application/json", `{}`))
	defer base.Close()
	h := newUpstreamsGateway(t, &ProviderConfig{
		Type: "openai", BaseURL: base.URL, Models: []string{"current"},
		ModelBaseURLs: map[string]string{"bge": base.URL},
	})
	req := httptest.NewRequest(http.MethodPost, "/gx10/v1/rerank", &failingBody{})
	req.Header.Set("Authorization", "Bearer sk-alice")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Empty(t, rec.snapshot())
}

func TestUpstreams_NoNewFields_AllTrafficToBaseURL(t *testing.T) {
	rec := &upstreamRecorder{}
	base := httptest.NewServer(rec.handler("base", "application/json", `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	defer base.Close()
	h := newUpstreamsGateway(t, &ProviderConfig{Type: "openai", BaseURL: base.URL, Models: []string{"current"}})

	req := newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice",
		map[string]any{"model": "gx10/current", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	preq := httptest.NewRequest(http.MethodPost, "/gx10/v1/decide", strings.NewReader(`{"model":"current"}`))
	preq.Header.Set("Authorization", "Bearer sk-alice")
	preq.Header.Set("Content-Type", "application/json")
	pw := httptest.NewRecorder()
	h.ServeHTTP(pw, preq)
	require.Equal(t, http.StatusOK, pw.Code)

	hits := rec.snapshot()
	require.Len(t, hits, 2)
	assert.Equal(t, "base", hits[0].server)
	assert.Equal(t, "base", hits[1].server)
	assert.Equal(t, "/v1/decide", hits[1].path)
}
