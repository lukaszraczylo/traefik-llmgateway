package traefikllmgateway

import (
	"sort"
	"sync"
)

const (
	metricPcRead  = "pcr"
	metricPcWrite = "pcw"
	metricPcSave  = "pcsave"
)

const (
	promptCacheKindRead     = "read"
	promptCacheKindWrite    = "write"
	promptCacheKindUncached = "uncached"
)

type promptCacheKey struct {
	provider string
	model    string
	kind     string
}

// promptCacheStats holds per-instance counters; Traefik builds one plugin per route.
type promptCacheStats struct {
	counts map[promptCacheKey]int64
	mu     sync.Mutex
}

func (s *promptCacheStats) add(k promptCacheKey, n int64) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	if s.counts == nil {
		s.counts = make(map[promptCacheKey]int64)
	}
	s.counts[k] += n
	s.mu.Unlock()
}

func (s *promptCacheStats) snapshot() map[promptCacheKey]int64 {
	s.mu.Lock()
	out := make(map[promptCacheKey]int64, len(s.counts))
	for k, v := range s.counts {
		out[k] = v
	}
	s.mu.Unlock()
	return out
}

// promptCacheSplit clamps like computeCostMicros: cached in [0,prompt], write in [0,prompt-cached].
func promptCacheSplit(u usage) (int64, int64, int64) {
	cached := u.cached
	if cached < 0 {
		cached = 0
	}
	if cached > u.prompt {
		cached = u.prompt
	}
	write := u.cacheWrite
	if write < 0 {
		write = 0
	}
	if write > u.prompt-cached {
		write = u.prompt - cached
	}
	rest := u.prompt - cached - write
	if rest < 0 {
		rest = 0
	}
	return cached, write, rest
}

// recordPromptCache counts one real upstream response; never call it for a response-cache hit.
func (g *Gateway) recordPromptCache(provider, model string, u usage) {
	if !metricsEnabled(g.cfg) {
		return
	}
	cached, write, rest := promptCacheSplit(u)
	g.promptCacheStats.add(promptCacheKey{provider, model, promptCacheKindRead}, cached)
	g.promptCacheStats.add(promptCacheKey{provider, model, promptCacheKindWrite}, write)
	g.promptCacheStats.add(promptCacheKey{provider, model, promptCacheKindUncached}, rest)
}

// unifiedPromptCacheSaved resolves the price exactly as unifiedCostMicros does; a free model saves 0.
func unifiedPromptCacheSaved(canonical, bare string, u usage, overrides map[string]*ModelPricing, meta map[string]*ModelMetaConfig) int64 {
	if u.cached <= 0 || modelMetaFree(canonical, bare, meta) {
		return 0
	}
	price, ok := lookupPricing(canonical, overrides)
	if !ok {
		price, ok = lookupPricing(bare, overrides)
	}
	if !ok {
		return 0
	}
	return promptCacheSavedMicros(price, u)
}

func (g *Gateway) writePromptCacheMetrics(m *metricWriter) {
	snap := g.promptCacheStats.snapshot()
	if len(snap) == 0 {
		return
	}
	keys := make([]promptCacheKey, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		x, y := keys[a], keys[b]
		if x.provider != y.provider {
			return x.provider < y.provider
		}
		if x.model != y.model {
			return x.model < y.model
		}
		return x.kind < y.kind
	})
	m.family("llmgateway_prompt_cache_tokens_total", "counter",
		"Prompt tokens by provider, model and kind (read: served from the provider prompt cache, write: written to it, uncached: neither)."+aggregationNotePerProcess)
	for _, k := range keys {
		m.sampleInt("llmgateway_prompt_cache_tokens_total",
			[]metricLabel{{"provider", k.provider}, {"model", k.model}, {"kind", k.kind}}, snap[k])
	}
}

func appendPromptCacheEntries(entries []counterIncr, sc limitScope, read, write, saved int64, wb windowBuckets, et enforceTTLSet) []counterIncr {
	for _, p := range [...]struct {
		metric string
		delta  int64
	}{{metricPcRead, read}, {metricPcWrite, write}, {metricPcSave, saved}} {
		if p.delta <= 0 {
			continue
		}
		entries = append(entries,
			counterIncr{key: windowKeyForBucket(sc.kind, sc.id, p.metric, windowHour, wb.hour), delta: p.delta, ttl: hourWindowTTL, enforceTTL: et.hour},
			counterIncr{key: windowKeyForBucket(sc.kind, sc.id, p.metric, windowDay, wb.day), delta: p.delta, ttl: dayWindowTTL, enforceTTL: et.day},
			counterIncr{key: windowKeyForBucket(sc.kind, sc.id, p.metric, windowMonth, wb.month), delta: p.delta, ttl: monthWindowTTL, enforceTTL: et.month},
		)
	}
	return entries
}
