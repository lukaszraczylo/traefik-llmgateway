package traefikllmgateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pcEntryStore struct {
	*memoryStore
	batches [][]counterIncr
}

func (s *pcEntryStore) incrMulti(entries []counterIncr) ([]int64, error) {
	s.batches = append(s.batches, append([]counterIncr(nil), entries...))
	return s.memoryStore.incrMulti(entries)
}

func (s *pcEntryStore) incrAndGetMulti(entries []counterIncr, reads []string) ([]int64, []int64, error) {
	s.batches = append(s.batches, append([]counterIncr(nil), entries...))
	return s.memoryStore.incrAndGetMulti(entries, reads)
}

func (s *pcEntryStore) entryCount() int {
	n := 0
	for _, b := range s.batches {
		n += len(b)
	}
	return n
}

func TestAccountWithPromptCacheCounters(t *testing.T) {
	t.Parallel()
	l := newLimiter(newMemoryStore(), false)
	now := time.Now()
	scopes := []limitScope{{kind: "user", id: "alice"}, {kind: kindModel, id: "p/m"}}
	l.accountWith(scopes, usage{prompt: 1000, cached: 900, cacheWrite: 50}, 0, accountExtras{promptCacheSaved: 2430})

	for _, window := range []string{windowHour, windowDay, windowMonth} {
		for metric, want := range map[string]int64{metricPcRead: 900, metricPcWrite: 50, metricPcSave: 2430} {
			got, _ := l.getCounter(kindModel, "p/m", metric, window, now)
			assert.Equal(t, want, got, "%s %s", metric, window)
		}
	}
	tokIn, _ := l.getCounter(kindModel, "p/m", metricTokIn, windowDay, now)
	assert.EqualValues(t, 1000, tokIn, "day token limits keep counting full prompt tokens")
	for _, metric := range []string{metricPcRead, metricPcWrite, metricPcSave} {
		got, _ := l.getCounter("user", "alice", metric, windowDay, now)
		assert.Zero(t, got, "%s must not be written for non-model scopes", metric)
	}
}

func TestAccountWithNoCacheNoExtraWrites(t *testing.T) {
	t.Parallel()
	scopes := []limitScope{{kind: "user", id: "alice"}, {kind: kindModel, id: "p/m"}}
	count := func(u usage) int {
		st := &pcEntryStore{memoryStore: newMemoryStore()}
		newLimiter(st, false).accountWith(scopes, u, 5, accountExtras{})
		return st.entryCount()
	}
	base := count(usage{prompt: 1000, completion: 10})
	assert.Equal(t, base, count(usage{prompt: 1000, completion: 10, cached: 0, cacheWrite: 0}))
	assert.Greater(t, count(usage{prompt: 1000, completion: 10, cached: 900}), base)
}

func TestAccountWithResponseCacheHitSkipsPromptCache(t *testing.T) {
	t.Parallel()
	l := newLimiter(newMemoryStore(), false)
	l.accountWith([]limitScope{{kind: kindModel, id: "p/m"}}, usage{prompt: 100, cached: 90}, 0, accountExtras{responseCacheHit: true, promptCacheSaved: 5})
	got, _ := l.getCounter(kindModel, "p/m", metricPcRead, windowDay, time.Now())
	assert.Zero(t, got)
}

func TestPromptCacheMetricsExposition(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	cfg.Metrics = &MetricsConfig{Enabled: true}
	_, gw := newAdminGatewayHandle(t, cfg)

	assert.NotContains(t, string(gw.renderMetrics()), "llmgateway_prompt_cache_tokens_total")
	gw.recordPromptCache("anthropic", "claude-x", usage{prompt: 1000, cached: 900, cacheWrite: 50})
	out := string(gw.renderMetrics())
	assert.Contains(t, out, `llmgateway_prompt_cache_tokens_total{provider="anthropic",model="claude-x",kind="read"} 900`)
	assert.Contains(t, out, `llmgateway_prompt_cache_tokens_total{provider="anthropic",model="claude-x",kind="write"} 50`)
	assert.Contains(t, out, `llmgateway_prompt_cache_tokens_total{provider="anthropic",model="claude-x",kind="uncached"} 50`)
}

func TestUnifiedPromptCacheSavedFreeModelSavesZero(t *testing.T) {
	t.Parallel()
	in, cachedIn := 3.0, 0.3
	over := map[string]*ModelPricing{"p/m": {InputPerM: in, CachedInputPerM: &cachedIn}}
	u := usage{prompt: 1000, cached: 1000}
	assert.EqualValues(t, 2700, unifiedPromptCacheSaved("p/m", "m", u, over, nil))
	meta := map[string]*ModelMetaConfig{"p/m": {Free: true}}
	assert.Zero(t, unifiedPromptCacheSaved("p/m", "m", u, over, meta))
}

func TestAdminUsageModelsPromptCache(t *testing.T) {
	t.Parallel()
	cfg := newAdminTestConfig()
	h, gw := newAdminGatewayHandle(t, cfg)
	now := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return now }
	seedModelCounter(gw, "alpha/a-model-1", metricReq, windowDay, now, 1)
	seedModelCounter(gw, "alpha/a-model-1", metricPcRead, windowDay, now, 900)
	seedModelCounter(gw, "alpha/a-model-1", metricPcWrite, windowDay, now, 50)
	seedModelCounter(gw, "alpha/a-model-1", metricPcSave, windowDay, now, 2430)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageModelsPath+"?metric=req&window=day&detail=1", "sk-admin1"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"promptCacheReadTokens":900`)
	var got adminUsageModelsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Models, 1)
	require.NotNil(t, got.Models[0].PromptCacheReadTokens)
	require.NotNil(t, got.Models[0].PromptCacheWriteTokens)
	require.NotNil(t, got.Models[0].PromptCacheSavedMicroUSD)
	assert.EqualValues(t, 900, *got.Models[0].PromptCacheReadTokens)
	assert.EqualValues(t, 50, *got.Models[0].PromptCacheWriteTokens)
	assert.EqualValues(t, 2430, *got.Models[0].PromptCacheSavedMicroUSD)
}

func TestAdminUsageModelsLegacyShapeOmitsPromptCache(t *testing.T) {
	t.Parallel()
	h, gw := newAdminGatewayHandle(t, newAdminTestConfig())
	now := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return now }
	seedModelCounter(gw, "alpha/a-model-1", metricReq, windowDay, now, 1)
	seedModelCounter(gw, "alpha/a-model-1", metricPcRead, windowDay, now, 900)

	for _, q := range []string{"?metric=req&window=day", "?metric=req&window=day&detail=0"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageModelsPath+q, "sk-admin1"))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "alpha/a-model-1")
		for _, f := range []string{"promptCacheReadTokens", "promptCacheWriteTokens", "promptCacheSavedMicroUsd"} {
			assert.NotContains(t, rec.Body.String(), f, q)
		}
	}
}

func TestCacheHitDoesNotRecordPromptCache(t *testing.T) {
	const respBody = `{"id":"c1","object":"chat.completion","model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":8}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	}))
	defer srv.Close()
	gw := newCacheTestGateway(t, srv, nil, defaultCacheMaxBodyBytes)
	gw.cfg.Cache.AccountHits = true
	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for _, want := range []string{"miss", "hit"} {
		rec := httptest.NewRecorder()
		gw.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", "sk-alice", body))
		require.Equal(t, want, rec.Header().Get("X-Llmgw-Cache"))
	}
	pcr, _ := gw.limiter.getCounter(kindModel, "openai/gpt-test", metricPcRead, windowDay, time.Now())
	assert.EqualValues(t, 8, pcr, "only the upstream response counts, not the replay")
}

func TestAdminUsageModelsDetailBudgetUnchanged(t *testing.T) {
	t.Parallel()
	const span = 48
	const preBranchMultiplier = 5 // req, tokin, tokout, cost, r402
	n := adminMaxKeysPerRequest/(span*preBranchMultiplier) - 2
	require.Greater(t, n*span*(preBranchMultiplier+3), adminMaxKeysPerRequest, "catalog must exceed the budget at 8x")
	cfg := newAdminTestConfig()
	models := make([]string, n)
	for i := range models {
		models[i] = fmt.Sprintf("m%03d", i)
	}
	cfg.Providers["alpha"].Models = models
	h, gw := newAdminGatewayHandle(t, cfg)
	now := time.Date(2026, 8, 20, 12, 30, 0, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return now }
	seedModelCounter(gw, "alpha/m001", metricReq, windowHour, now, 1)
	seedModelCounter(gw, "alpha/m001", metricPcRead, windowHour, now, 7)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsageModelsPath+"?metric=req&window=hour&span=48&detail=1", "sk-admin1"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got adminUsageModelsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Models, 1)
	require.NotNil(t, got.Models[0].PromptCacheReadTokens)
	assert.EqualValues(t, 7, *got.Models[0].PromptCacheReadTokens)
}

func TestSplitPromptTokensNegativePrompt(t *testing.T) {
	t.Parallel()
	u := usage{prompt: -5, cached: 3, cacheWrite: 2}
	cached, write, rest := splitPromptTokens(u)
	assert.Zero(t, cached)
	assert.Zero(t, write)
	assert.Zero(t, rest)

	cfg := newAdminTestConfig()
	cfg.Metrics = &MetricsConfig{Enabled: true}
	_, gw := newAdminGatewayHandle(t, cfg)
	gw.recordPromptCache("p", "m", u)
	for k, v := range gw.promptCacheStats.snapshot() {
		assert.Zero(t, v, "%v", k)
	}
}
