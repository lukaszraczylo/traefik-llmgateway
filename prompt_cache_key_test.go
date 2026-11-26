package traefikllmgateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var pckPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func pckBody(system, first string, extra ...any) map[string]any {
	msgs := []any{
		map[string]any{"role": "system", "content": system},
		map[string]any{"role": "user", "content": first},
	}
	msgs = append(msgs, extra...)
	return map[string]any{"model": "shared", "messages": msgs}
}

func pckKey(t *testing.T, body map[string]any, callerScope string) string {
	t.Helper()
	return hashPromptCacheKey(smartCallerID([]limitScope{{kind: "user", id: callerScope}}, nil), promptCacheSessionID(body, nil))
}

func TestPromptCacheKeyStableAcrossTurns(t *testing.T) {
	t.Parallel()
	short := pckBody("be terse", "hello")
	long := pckBody("be terse", "hello",
		map[string]any{"role": "assistant", "content": "hi"},
		map[string]any{"role": "user", "content": "more"})
	assert.Equal(t, pckKey(t, short, "alice"), pckKey(t, long, "alice"))
	assert.NotEqual(t, pckKey(t, short, "alice"), pckKey(t, pckBody("be terse", "other"), "alice"))
}

func TestPromptCacheKeyDiffersByCaller(t *testing.T) {
	t.Parallel()
	b := pckBody("s", "u")
	assert.NotEqual(t, pckKey(t, b, "alice"), pckKey(t, b, "bob"))
}

func TestPromptCacheKeyNoRawID(t *testing.T) {
	t.Parallel()
	b := pckBody("s", "u")
	b["metadata"] = map[string]any{"user_id": "alice@example.com"}
	k := pckKey(t, b, "alice")
	assert.Regexp(t, pckPattern, k)
	assert.NotContains(t, k, "alice")
}

type pckServers struct {
	bodies map[string][]string
	mu     sync.Mutex
}

func (p *pckServers) handler(name string, status int, resp string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.bodies[name] = append(p.bodies[name], string(b))
		p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(resp))
	}
}

func (p *pckServers) last(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.bodies[name]
	if len(b) == 0 {
		return ""
	}
	return b[len(b)-1]
}

func newPckGateway(t *testing.T, key bool, alphaStatus int) (*Gateway, *pckServers) {
	t.Helper()
	ps := &pckServers{bodies: map[string][]string{}}
	alpha := httptest.NewServer(ps.handler("alpha", alphaStatus, successRespBody))
	t.Cleanup(alpha.Close)
	beta := httptest.NewServer(ps.handler("beta", http.StatusOK, successRespBody))
	t.Cleanup(beta.Close)
	cfg := twoProviderFailoverConfig(alpha, beta)
	if key {
		cfg.Providers["alpha"].PromptCache = &PromptCacheConfig{Key: true}
		cfg.Providers["beta"].PromptCache = &PromptCacheConfig{Key: true}
	}
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	require.NoError(t, err)
	gw, ok := h.(*Gateway)
	require.True(t, ok)
	return gw, ps
}

func servePck(t *testing.T, gw *Gateway, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec
}

func TestPromptCacheKeyRespectsClient(t *testing.T) {
	gw, ps := newPckGateway(t, true, http.StatusOK)
	body := pckBody("s", "u")
	body["prompt_cache_key"] = ""
	servePck(t, gw, body)
	got := ps.last("alpha") + ps.last("beta")
	assert.Contains(t, got, `"prompt_cache_key":""`)
}

func TestPromptCacheKeyNotInPristine(t *testing.T) {
	gw, ps := newPckGateway(t, true, http.StatusInternalServerError)
	body := pckBody("s", "u")
	servePck(t, gw, body)
	a, b := ps.last("alpha"), ps.last("beta")
	require.NotEmpty(t, a)
	require.NotEmpty(t, b)
	re := regexp.MustCompile(`"prompt_cache_key":"([0-9a-f]{32})"`)
	ma, mb := re.FindStringSubmatch(a), re.FindStringSubmatch(b)
	require.Len(t, ma, 2)
	require.Len(t, mb, 2)
	assert.Equal(t, ma[1], mb[1], "same key on both attempts")
	assert.Equal(t, 1, strings.Count(b, "prompt_cache_key"), "key set once on the failover attempt")

}

func TestPromptCacheKeyLeavesResponseCacheKey(t *testing.T) {
	gw, _ := newPckGateway(t, true, http.StatusInternalServerError)
	redisLn := newBehavioralRedisServer(t)
	gw.cfg.Redis = &RedisConfig{Address: redisLn.Addr().String()}
	gw.cfg.Cache = CacheConfig{Enabled: true, TTL: "1m"}
	cfg := *gw.cfg
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &cfg, "llmgw")
	require.NoError(t, err)
	g2, ok := h.(*Gateway)
	require.True(t, ok)
	require.NotNil(t, g2.cache)

	body := pckBody("s", "u")
	servePck(t, g2, body)
	k := cacheKey("beta", "shared", "shared", cacheEndpointChat, map[string]any{"model": "shared", "messages": body["messages"]})
	_, hit := g2.cache.lookup(k)
	assert.True(t, hit, "response-cache key must not include prompt_cache_key")
}

func TestPromptCacheKeyOffByDefault(t *testing.T) {
	gw, ps := newPckGateway(t, false, http.StatusOK)
	servePck(t, gw, pckBody("s", "u"))
	assert.NotContains(t, ps.last("alpha")+ps.last("beta"), "prompt_cache_key")
}

func TestTranslatedMessagesGetsKey(t *testing.T) {
	ps := &pckServers{bodies: map[string][]string{}}
	srv := httptest.NewServer(ps.handler("openai", http.StatusOK, `{"id":"c","object":"chat.completion","model":"gpt-real","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":4}}`))
	defer srv.Close()
	gw := newMessagesTestGateway(t, "", srv.URL)
	gw.cfg.Providers["openai"].PromptCache = &PromptCacheConfig{Key: true}

	body := map[string]any{
		"model": "gpt-test", "max_tokens": 10, "system": "be terse",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, newMessagesRequest(t, body, "x-api-key", "sk-alice"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Regexp(t, `"prompt_cache_key":"[0-9a-f]{32}"`, ps.last("openai"))
}
