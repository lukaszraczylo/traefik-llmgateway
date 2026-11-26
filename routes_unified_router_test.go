package traefikllmgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	routerName    = "auto"
	routerKeyword = "escalate-me"
	routerFail    = `{"error":{"message":"down","type":"server_error"}}`
)

type routerBackend struct {
	srv   *httptest.Server
	calls int64
}

func newRouterBackend(status int, body string) *routerBackend {
	b := &routerBackend{}
	b.srv = countingServer(status, body, &b.calls)
	return b
}

func (b *routerBackend) count() int64 { return atomic.LoadInt64(&b.calls) }

// routerTestConfig wires alpha(a1,a2), beta(b1), gamma(c1) behind the "auto" router.
func routerTestConfig(alpha, beta, gamma *routerBackend, tiers []SmartRouterTier) *Config {
	cfg := CreateConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"alpha": {Type: "openai", BaseURL: alpha.srv.URL, APIKey: "k", Models: []string{"a1", "a2"}},
		"beta":  {Type: "openai", BaseURL: beta.srv.URL, APIKey: "k", Models: []string{"b1"}},
		"gamma": {Type: "openai", BaseURL: gamma.srv.URL, APIKey: "k", Models: []string{"c1"}},
	}
	cfg.Groups = map[string]*GroupConfig{"default": {}}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{{Name: "alice", Group: "default", APIKey: "sk-alice", Limits: &LimitsConfig{}}}}
	cfg.SmartRouters = map[string]*SmartRouterConfig{
		routerName: {
			Tiers:        tiers,
			KeywordRules: []SmartRouterKeywordRule{{Tier: tiers[len(tiers)-1].Name, Keywords: []string{routerKeyword}}},
		},
	}
	return cfg
}

// twoTiers rewrites the router to tier t0 (the given models) over t1 (c1).
func twoTiers(c *Config, t0 ...string) {
	rc := c.SmartRouters[routerName]
	rc.KeywordRules = nil
	rc.Tiers = []SmartRouterTier{
		{Name: "t0", MinScore: 0, Models: t0},
		{Name: "t1", MinScore: 300, Models: []string{"c1"}},
	}
}

func threeTiers() []SmartRouterTier {
	return []SmartRouterTier{
		{Name: "t0", MinScore: 0, Models: []string{"a1"}},
		{Name: "t1", MinScore: 300, Models: []string{"b1"}},
		{Name: "t2", MinScore: 600, Models: []string{"c1"}},
	}
}

func buildRouterGateway(t *testing.T, cfg *Config) (http.Handler, *Gateway) {
	t.Helper()
	h, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), cfg, "llmgw")
	require.NoError(t, err)
	gw, ok := h.(*Gateway)
	require.True(t, ok)
	return h, gw
}

func routerChat(t *testing.T, h http.Handler, model string, content any, extra map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": content}}}
	for k, v := range extra {
		body[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
	return rec
}

func closeBackends(bs ...*routerBackend) {
	for _, b := range bs {
		b.srv.Close()
	}
}

func TestRouter_EscalationOrder(t *testing.T) {
	tests := []struct {
		name       string
		prompt     string
		wantTier   string
		wantModel  string
		statuses   [3]int
		wantCalls  [3]int64
		wantStatus int
	}{
		{name: "healthy tier-0 serves", prompt: "hi", statuses: [3]int{200, 200, 200}, wantStatus: 200, wantTier: "t0", wantModel: "alpha/a1", wantCalls: [3]int64{1, 0, 0}},
		{name: "tier-0 fails, escalates to tier-1", prompt: "hi", statuses: [3]int{503, 200, 200}, wantStatus: 200, wantTier: "t1", wantModel: "beta/b1", wantCalls: [3]int64{1, 1, 0}},
		{name: "tier-0 and 1 fail, escalates to tier-2", prompt: "hi", statuses: [3]int{503, 503, 200}, wantStatus: 200, wantTier: "t2", wantModel: "gamma/c1", wantCalls: [3]int64{1, 1, 1}},
		{name: "complex request never lands on a lower tier", prompt: routerKeyword, statuses: [3]int{200, 200, 503}, wantStatus: 503, wantCalls: [3]int64{0, 0, 1}},
		{name: "upstream 400 does not escalate", prompt: "hi", statuses: [3]int{400, 200, 200}, wantStatus: 400, wantCalls: [3]int64{1, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := func(s int) string {
				if s == 200 {
					return successRespBody
				}
				return routerFail
			}
			a := newRouterBackend(tt.statuses[0], body(tt.statuses[0]))
			b := newRouterBackend(tt.statuses[1], body(tt.statuses[1]))
			c := newRouterBackend(tt.statuses[2], body(tt.statuses[2]))
			defer closeBackends(a, b, c)
			h, gw := buildRouterGateway(t, routerTestConfig(a, b, c, threeTiers()))
			require.False(t, gw.failover.enabled, "escalation must work with failover disabled")

			rec := routerChat(t, h, routerName, tt.prompt, nil)
			assert.Equal(t, tt.wantStatus, rec.Code, rec.Body.String())
			assert.Equal(t, tt.wantCalls, [3]int64{a.count(), b.count(), c.count()})
			if tt.wantStatus == 200 {
				assert.Equal(t, tt.wantTier, rec.Header().Get("X-Llmgw-Router-Tier"))
				assert.Equal(t, tt.wantModel, rec.Header().Get("X-Llmgw-Routed-Model"))
			}
		})
	}
}

func TestRouter_SameTierModelsBeforeNextTier(t *testing.T) {
	a := newRouterBackend(503, routerFail)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	tiers := []SmartRouterTier{
		{Name: "t0", MinScore: 0, Models: []string{"a1", "b1"}},
		{Name: "t1", MinScore: 300, Models: []string{"c1"}},
	}
	h, _ := buildRouterGateway(t, routerTestConfig(a, b, c, tiers))
	rec := routerChat(t, h, routerName, "hi", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Equal(t, "beta/b1", rec.Header().Get("X-Llmgw-Routed-Model"))
	assert.Equal(t, "t0", rec.Header().Get("X-Llmgw-Router-Tier"))
	assert.Equal(t, int64(0), c.count())
}

// Regression for LiteLLM #38832: fallbacks were looked up under the router name.
// Here the failover candidate comes from the tier's own model, never the router name.
func TestRouter_Regression_LiteLLM38832_FallbackPerTierCandidate(t *testing.T) {
	a := newRouterBackend(503, routerFail)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	cfg := routerTestConfig(a, b, c, []SmartRouterTier{
		{Name: "t0", MinScore: 0, Models: []string{"shared"}},
		{Name: "t1", MinScore: 300, Models: []string{"c1"}},
	})
	cfg.Providers["alpha"].Models = []string{"shared"}
	cfg.Providers["beta"].Models = []string{"shared"}
	yes := true
	cfg.Failover = FailoverConfig{Enabled: &yes}
	h, _ := buildRouterGateway(t, cfg)

	rec := routerChat(t, h, routerName, "hi", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Equal(t, "beta/shared", rec.Header().Get("X-Llmgw-Routed-Model"), "same-model provider fallback must serve")
	assert.Equal(t, "t0", rec.Header().Get("X-Llmgw-Router-Tier"), "fallback stays inside the tier")
	assert.Equal(t, int64(0), c.count(), "no cross-tier escalation while the tier still has a candidate")
}

func TestRouter_TargetSkipping(t *testing.T) {
	no := false
	longPrompt := strings.Repeat("word ", 400)
	tests := []struct {
		mut        func(*Config)
		content    any
		name       string
		wantModel  string
		wantStatus int
		wantA1Hit  int64
	}{
		{
			name: "denied target is skipped",
			mut: func(c *Config) {
				c.Groups["default"].Models = []string{routerName, "b1"}
			},
			content: "hi", wantStatus: 200, wantModel: "beta/b1",
		},
		{
			name: "router name denied is 403",
			mut: func(c *Config) {
				c.Groups["default"].Models = []string{"a1", "b1"}
			},
			content: "hi", wantStatus: 403,
		},
		{
			name: "all targets denied is 403",
			mut: func(c *Config) {
				c.Groups["default"].Models = []string{routerName}
			},
			content: "hi", wantStatus: 403,
		},
		{
			name: "unresolvable target is skipped",
			mut: func(c *Config) {
				c.SmartRouters[routerName].Tiers[0].Models = []string{"ghost", "a1"}
			},
			content: "hi", wantStatus: 200, wantModel: "alpha/a1", wantA1Hit: 1,
		},
		{
			name: "no resolvable target is 404",
			mut: func(c *Config) {
				c.SmartRouters[routerName].Tiers = []SmartRouterTier{{Name: "t0", Models: []string{"ghost"}}}
				c.SmartRouters[routerName].KeywordRules = nil
			},
			content: "hi", wantStatus: 404,
		},
		{
			name: "vision false is skipped for image requests",
			mut: func(c *Config) {
				c.ModelMeta = map[string]*ModelMetaConfig{"a1": {Vision: &no}}
				twoTiers(c, "a1", "b1")
			},
			content: []any{
				map[string]any{"type": "text", "text": "hi"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
			},
			wantStatus: 200, wantModel: "beta/b1",
		},
		{
			name: "vision false still serves text requests",
			mut: func(c *Config) {
				c.ModelMeta = map[string]*ModelMetaConfig{"a1": {Vision: &no}}
				twoTiers(c, "a1", "b1")
			},
			content: "hi", wantStatus: 200, wantModel: "alpha/a1", wantA1Hit: 1,
		},
		{
			name: "small context window skipped when escalation on",
			mut: func(c *Config) {
				c.ModelMeta = map[string]*ModelMetaConfig{"a1": {ContextTokens: 50}}
				twoTiers(c, "a1", "b1")
			},
			content: longPrompt, wantStatus: 200, wantModel: "beta/b1",
		},
		{
			name: "small context window kept when escalation off",
			mut: func(c *Config) {
				c.ModelMeta = map[string]*ModelMetaConfig{"a1": {ContextTokens: 50}}
				twoTiers(c, "a1", "b1")
				c.SmartRouters[routerName].ContextEscalation = &no
			},
			content: longPrompt, wantStatus: 200, wantModel: "alpha/a1", wantA1Hit: 1,
		},
		{
			name: "unpriced target skipped under a cost budget instead of 402",
			mut: func(c *Config) {
				c.Users.Inline[0].Limits = &LimitsConfig{CostPerDayUSD: 10}
				c.Pricing = map[string]*ModelPricing{"b1": {InputPerM: 1, OutputPerM: 1}}
				twoTiers(c, "a1", "b1")
			},
			content: "hi", wantStatus: 200, wantModel: "beta/b1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newRouterBackend(200, successRespBody)
			b := newRouterBackend(200, successRespBody)
			c := newRouterBackend(200, successRespBody)
			defer closeBackends(a, b, c)
			cfg := routerTestConfig(a, b, c, threeTiers())
			tt.mut(cfg)
			h, _ := buildRouterGateway(t, cfg)
			rec := routerChat(t, h, routerName, tt.content, nil)
			require.Equal(t, tt.wantStatus, rec.Code, rec.Body.String())
			assert.Equal(t, tt.wantA1Hit, a.count())
			if tt.wantModel != "" {
				assert.Equal(t, tt.wantModel, rec.Header().Get("X-Llmgw-Routed-Model"))
			}
		})
	}
}

func TestRouter_EmbeddingsRejected(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, _ := buildRouterGateway(t, routerTestConfig(a, b, c, threeTiers()))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/embeddings", "sk-alice", map[string]any{"model": routerName, "input": "hi"}))
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "embeddings")
	assert.Equal(t, int64(0), a.count()+b.count()+c.count())
}

func TestRouter_MediaRoutesDoNotKnowRouters(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, _ := buildRouterGateway(t, routerTestConfig(a, b, c, threeTiers()))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/images/generations", "sk-alice", map[string]any{"model": routerName, "prompt": "cat"}))
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	assert.Equal(t, int64(0), a.count()+b.count()+c.count())
}

func TestRouter_HeadersOnCacheMissAndHit(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	cfg := routerTestConfig(a, b, c, threeTiers())
	cfg.Redis = &RedisConfig{Address: newBehavioralRedisServer(t).Addr().String()}
	cfg.Cache = CacheConfig{Enabled: true, TTL: "1m", MaxBodyBytes: 1 << 20}
	h, _ := buildRouterGateway(t, cfg)

	miss := routerChat(t, h, routerName, "hi", nil)
	require.Equal(t, 200, miss.Code, miss.Body.String())
	assert.Equal(t, "miss", miss.Header().Get("X-Llmgw-Cache"))
	assert.Equal(t, "alpha/a1", miss.Header().Get("X-Llmgw-Routed-Model"))
	assert.Equal(t, "t0", miss.Header().Get("X-Llmgw-Router-Tier"))

	hit := routerChat(t, h, routerName, "hi", nil)
	require.Equal(t, 200, hit.Code)
	assert.Equal(t, "hit", hit.Header().Get("X-Llmgw-Cache"))
	assert.Equal(t, "alpha/a1", hit.Header().Get("X-Llmgw-Routed-Model"))
	assert.Equal(t, "t0", hit.Header().Get("X-Llmgw-Router-Tier"))
	assert.Equal(t, int64(1), a.count(), "second request must come from cache")
}

func TestRouter_StreamingNoEscalationAfterFirstByte(t *testing.T) {
	const firstChunk = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"
	var aCalls int64
	alphaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&aCalls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(firstChunk))
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	defer alphaSrv.Close()
	a := &routerBackend{srv: alphaSrv}
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(b, c)
	h, _ := buildRouterGateway(t, routerTestConfig(a, b, c, threeTiers()))

	rec := routerChat(t, h, routerName, "hi", map[string]any{"stream": true})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, strings.HasPrefix(rec.Body.String(), firstChunk))
	assert.Equal(t, int64(1), atomic.LoadInt64(&aCalls))
	assert.Equal(t, int64(0), b.count()+c.count(), "no escalation once bytes reached the client")
}

func TestRouter_MessagesRouteIsRouted(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		gotModel, _ = m["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(successRespBody))
	}))
	defer srv.Close()
	a := &routerBackend{srv: srv}
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(b, c)
	h, _ := buildRouterGateway(t, routerTestConfig(a, b, c, threeTiers()))

	body := map[string]any{"model": routerName, "max_tokens": 50, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newMessagesRequest(t, body, "x-api-key", "sk-alice"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "a1", gotModel, "upstream must receive the real model, not the router name")
	assert.Equal(t, "alpha/a1", rec.Header().Get("X-Llmgw-Routed-Model"))
	assert.Equal(t, "t0", rec.Header().Get("X-Llmgw-Router-Tier"))
	assert.Contains(t, rec.Body.String(), `"model":"`+routerName+`"`, "the messages echo keeps the router name")
}

func TestRouter_ModelsListing(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	cfg := routerTestConfig(a, b, c, threeTiers())
	cfg.Groups = map[string]*GroupConfig{
		"open":       {},
		"noRouter":   {Models: []string{"a1", "b1", "c1"}},
		"noTargets":  {Models: []string{routerName}},
		"someTarget": {Models: []string{routerName, "c1"}},
	}
	cfg.Users = &UsersConfig{Inline: []*UserConfig{
		{Name: "u-open", Group: "open", APIKey: "sk-open", Limits: &LimitsConfig{}},
		{Name: "u-norouter", Group: "noRouter", APIKey: "sk-norouter", Limits: &LimitsConfig{}},
		{Name: "u-notargets", Group: "noTargets", APIKey: "sk-notargets", Limits: &LimitsConfig{}},
		{Name: "u-some", Group: "someTarget", APIKey: "sk-some", Limits: &LimitsConfig{}},
	}}
	h, _ := buildRouterGateway(t, cfg)

	tests := []struct {
		key    string
		listed bool
	}{
		{"sk-open", true},
		{"sk-norouter", false},
		{"sk-notargets", false},
		{"sk-some", true},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+tt.key)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, 200, rec.Code, tt.key)
		var out struct {
			Data []map[string]any `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		found := false
		for _, m := range out.Data {
			if m["id"] == routerName {
				found = true
				assert.Equal(t, routerOwnedBy, m["owned_by"])
			}
		}
		assert.Equal(t, tt.listed, found, tt.key)
	}
}

func TestPlanSmartRouter_HashSelectionDedupeAndCap(t *testing.T) {
	a := newRouterBackend(200, successRespBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	cfg := routerTestConfig(a, b, c, []SmartRouterTier{
		{Name: "t0", MinScore: 0, Models: []string{"a1", "a2", "b1"}},
		{Name: "t1", MinScore: 300, Models: []string{"fast", "c1"}},
	})
	cfg.ModelAliases = map[string]string{"fast": "c1"}
	cfg.SmartRouters[routerName].Selection = smartSelectionHash
	cfg.SmartRouters[routerName].MaxAttempts = 5
	_, gw := buildRouterGateway(t, cfg)
	rt := gw.registry.routers[routerName]
	grp := &group{}

	first := map[string]bool{}
	for _, prompt := range []string{"alpha one", "beta two", "gamma three", "delta four", "epsilon five", "zeta six", "eta seven", "theta eight"} {
		req := map[string]any{"messages": []any{map[string]any{"role": "user", "content": prompt}}}
		p1, err := gw.planSmartRouter(context.Background(), rt, req, cacheEndpointChat, nil, grp, nil)
		require.NoError(t, err)
		p2, err := gw.planSmartRouter(context.Background(), rt, req, cacheEndpointChat, nil, grp, nil)
		require.NoError(t, err)
		require.Equal(t, p1.candidates(), p2.candidates(), "hash selection must be deterministic")
		first[p1.entries[0].cand.canonical] = true

		assert.Len(t, p1.entries, 4, "3 tier-0 models plus one tier-1 model; the alias duplicate is dropped")
		for _, e := range p1.entries[:3] {
			assert.Equal(t, "t0", e.tier)
		}
		assert.Equal(t, "t1", p1.entries[3].tier)
	}
	assert.Greater(t, len(first), 1, "hash selection must spread across the tier")

	rt.maxAttempts = 2
	req := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	p, err := gw.planSmartRouter(context.Background(), rt, req, cacheEndpointChat, nil, grp, nil)
	require.NoError(t, err)
	assert.Len(t, p.entries, 2, "plan must be capped at maxAttempts")
}

const routerOverflowBody = `{"error":{"message":"This model's maximum context length is 8192 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`

func routerMetricsConfig(a, b, c *routerBackend) *Config {
	cfg := routerTestConfig(a, b, c, threeTiers())
	cfg.Metrics = &MetricsConfig{Enabled: true}
	return cfg
}

func TestRouter_ContextOverflow400EscalatesToNextTier(t *testing.T) {
	a := newRouterBackend(400, routerOverflowBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, gw := buildRouterGateway(t, routerMetricsConfig(a, b, c))
	require.False(t, gw.failover.enabled)

	rec := routerChat(t, h, routerName, "hi", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "t1", rec.Header().Get("X-Llmgw-Router-Tier"))
	assert.Equal(t, [3]int64{1, 1, 0}, [3]int64{a.count(), b.count(), c.count()})
	_, escs := gw.routerStats.snapshot()
	assert.Equal(t, int64(1), escs[routerEscalationKey{router: routerName, from: "t0", to: "t1"}])
}

func TestRouter_ContextOverflow400EscalatesOnStreamBeforeFirstByte(t *testing.T) {
	a := newRouterBackend(400, routerOverflowBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, _ := buildRouterGateway(t, routerMetricsConfig(a, b, c))

	rec := routerChat(t, h, routerName, "hi", map[string]any{"stream": true})
	assert.Equal(t, [3]int64{1, 1, 0}, [3]int64{a.count(), b.count(), c.count()}, rec.Body.String())
}

func TestRouter_Other400DoesNotEscalate(t *testing.T) {
	a := newRouterBackend(400, `{"error":{"message":"invalid temperature","type":"invalid_request_error"}}`)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, gw := buildRouterGateway(t, routerMetricsConfig(a, b, c))

	rec := routerChat(t, h, routerName, "hi", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, [3]int64{1, 0, 0}, [3]int64{a.count(), b.count(), c.count()})
	_, escs := gw.routerStats.snapshot()
	assert.Empty(t, escs)
}

func TestRouter_ContextOverflow400OnDirectRequestIsUnchanged(t *testing.T) {
	a := newRouterBackend(400, routerOverflowBody)
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(a, b, c)
	h, _ := buildRouterGateway(t, routerMetricsConfig(a, b, c))

	rec := routerChat(t, h, "a1", "hi", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, [3]int64{1, 0, 0}, [3]int64{a.count(), b.count(), c.count()})
}

func TestRouter_ContextOverflowAfterBytesWrittenDoesNotEscalate(t *testing.T) {
	const firstChunk = "data: {\"error\":{\"code\":\"context_length_exceeded\"}}\n\n"
	alphaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(firstChunk))
		w.(http.Flusher).Flush()
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}))
	defer alphaSrv.Close()
	a := &routerBackend{srv: alphaSrv}
	b := newRouterBackend(200, successRespBody)
	c := newRouterBackend(200, successRespBody)
	defer closeBackends(b, c)
	h, _ := buildRouterGateway(t, routerMetricsConfig(a, b, c))

	rec := routerChat(t, h, routerName, "hi", map[string]any{"stream": true})
	assert.True(t, strings.HasPrefix(rec.Body.String(), firstChunk))
	assert.Equal(t, int64(0), b.count()+c.count())
}

func TestIsContextOverflowBody(t *testing.T) {
	yes := []string{
		`{"error":{"code":"context_length_exceeded"}}`,
		"This model's Maximum Context Length is 8192 tokens",
		"prompt is too long: 250000 tokens > 200000 maximum",
		"exceeds the CONTEXT WINDOW of this model",
		"This prompt (12000 tokens) is longer than the model's max length",
		"Request has too many tokens",
	}
	for _, s := range yes {
		assert.True(t, isContextOverflowBody([]byte(s)), s)
	}
	for _, s := range []string{"", `{"error":{"message":"invalid temperature"}}`, "bad request"} {
		assert.False(t, isContextOverflowBody([]byte(s)), s)
	}
	late := strings.Repeat("x", contextOverflowScanBytes) + "context_length_exceeded"
	assert.False(t, isContextOverflowBody([]byte(late)), "only a bounded prefix is scanned")
}

func TestRouterEscalatable(t *testing.T) {
	overflow := &providerHTTPError{status: http.StatusBadRequest, body: []byte(routerOverflowBody)}
	plain := &providerHTTPError{status: http.StatusBadRequest, body: []byte("nope")}
	assert.True(t, routerEscalatable(overflow, true))
	assert.False(t, routerEscalatable(overflow, false), "non-router requests keep the 400 rule")
	assert.False(t, routerEscalatable(plain, true))
	assert.True(t, routerEscalatable(&providerHTTPError{status: 503}, true))
	assert.True(t, routerEscalatable(&providerHTTPError{status: 503}, false))
	assert.False(t, routerEscalatable(nil, true))
}
