package traefikllmgateway

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	smartDecisionOff    = "off"
	smartDecisionShadow = "shadow"
	smartDecisionLive   = "live"

	defaultDecisionPath    = "/v1/decide"
	defaultDecisionTimeout = 500 * time.Millisecond
	minDecisionTimeout     = 50 * time.Millisecond
	maxDecisionTimeout     = 2 * time.Second
	defaultDecisionMinProb = 700
	defaultDecisionBand    = 100
	maxDecisionPerMille    = 1000

	decisionSystemCap    = 1024
	decisionAskCap       = 8192
	decisionBodyLimit    = 64 << 10
	decisionCacheSize    = 1024
	decisionCacheTTL     = 10 * time.Minute
	decisionBreakerFails = 3
	decisionBreakerOpen  = 30 * time.Second

	decisionAgree    = "agree"
	decisionOverride = "override"
	decisionLowConf  = "lowconf"
	decisionTimeout  = "timeout"
	decisionError    = "error"
	decisionBreaker  = "breaker"
	decisionCache    = "cache"
)

var routerDecisionOutcomes = [...]string{decisionAgree, decisionOverride, decisionLowConf, decisionTimeout, decisionError, decisionBreaker, decisionCache}

// SmartRouterDecisionConfig asks a THX-02 decision model to settle requests whose
// classifier score sits near a tier boundary. It needs classifier v2 and is off when absent.
type SmartRouterDecisionConfig struct {
	// MinProb is the least per-mille answer probability that may override; nil means 700.
	MinProb *int `json:"minProb,omitempty"`
	// Band is the per-mille score distance from a tier threshold that triggers a call; nil means 100.
	Band *int `json:"band,omitempty"`
	// Provider names the providers entry that serves Path; required.
	Provider string `json:"provider,omitempty"`
	// Path is the decide endpoint; empty means /v1/decide. The provider needs a matching pathBaseUrls key.
	Path string `json:"path,omitempty"`
	// Mode is "off", "shadow" (default) or "live".
	Mode string `json:"mode,omitempty"`
	// Timeout (Go duration, 50ms to 2s) bounds one call; empty means 500ms.
	Timeout string `json:"timeout,omitempty"`
}

// smartDecisionCfg is a validated SmartRouterDecisionConfig with defaults applied.
type smartDecisionCfg struct {
	dec      *smartDecider
	provider string
	path     string
	timeout  time.Duration
	minProb  int
	band     int
	live     bool
}

func compileSmartDecision(router string, cfg *SmartRouterDecisionConfig, classifier string) (*smartDecisionCfg, error) {
	if cfg == nil || cfg.Mode == smartDecisionOff {
		return nil, nil
	}
	d := &smartDecisionCfg{
		path:    defaultDecisionPath,
		timeout: defaultDecisionTimeout,
		minProb: defaultDecisionMinProb,
		band:    defaultDecisionBand,
		dec:     newSmartDecider(time.Now),
	}
	switch cfg.Mode {
	case "", smartDecisionShadow:
	case smartDecisionLive:
		d.live = true
	default:
		return nil, fmt.Errorf("llmgateway: smartRouters: router %q: decision.mode %q must be %q, %q or %q", router, cfg.Mode, smartDecisionOff, smartDecisionShadow, smartDecisionLive)
	}
	if classifier != smartClassifierV2 {
		return nil, fmt.Errorf("llmgateway: smartRouters: router %q: decision needs classifier %q", router, smartClassifierV2)
	}
	if cfg.Provider == "" {
		return nil, fmt.Errorf("llmgateway: smartRouters: router %q: decision.provider is required", router)
	}
	d.provider = cfg.Provider
	if cfg.Path != "" {
		if !strings.HasPrefix(cfg.Path, "/") {
			return nil, fmt.Errorf("llmgateway: smartRouters: router %q: decision.path must start with \"/\"", router)
		}
		d.path = cfg.Path
	}
	if cfg.Timeout != "" {
		to, err := time.ParseDuration(cfg.Timeout)
		if err != nil {
			return nil, fmt.Errorf("llmgateway: smartRouters: router %q: decision.timeout %q is not a duration", router, cfg.Timeout)
		}
		if to < minDecisionTimeout || to > maxDecisionTimeout {
			return nil, fmt.Errorf("llmgateway: smartRouters: router %q: decision.timeout must be between %s and %s, got %s", router, minDecisionTimeout, maxDecisionTimeout, to)
		}
		d.timeout = to
	}
	if cfg.MinProb != nil {
		d.minProb = *cfg.MinProb
	}
	if d.minProb < 1 || d.minProb > maxDecisionPerMille {
		return nil, fmt.Errorf("llmgateway: smartRouters: router %q: decision.minProb must be between 1 and %d, got %d", router, maxDecisionPerMille, d.minProb)
	}
	if cfg.Band != nil {
		d.band = *cfg.Band
	}
	if d.band < 1 || d.band > maxDecisionPerMille {
		return nil, fmt.Errorf("llmgateway: smartRouters: router %q: decision.band must be between 1 and %d, got %d", router, maxDecisionPerMille, d.band)
	}
	return d, nil
}

// validateRouterDecisions checks each router's decision provider and path against the providers config.
func validateRouterDecisions(routers map[string]*smartRouter, cfg *Config) error {
	names := make([]string, 0, len(routers))
	for name := range routers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rt := routers[name]
		if rt == nil || rt.decision == nil {
			continue
		}
		d := rt.decision
		pc, ok := cfg.Providers[d.provider]
		if !ok || pc == nil {
			return fmt.Errorf("llmgateway: smartRouters: router %q: decision.provider %q is not a configured provider", name, d.provider)
		}
		if _, found := longestPathPrefix(d.path, pc.PathBaseURLs); !found {
			return fmt.Errorf("llmgateway: smartRouters: router %q: provider %q has no pathBaseUrls prefix matching decision.path %q", name, d.provider, d.path)
		}
	}
	return nil
}

type decisionCacheEntry struct {
	expires time.Time
	key     uint64
	rank    int
	prob    int
}

// smartDecider holds the breaker and answer cache shared by every request of one router.
type smartDecider struct {
	now       func() time.Time
	items     map[uint64]*list.Element
	order     *list.List
	openUntil time.Time
	mu        sync.Mutex
	fails     int
}

func newSmartDecider(nowFn func() time.Time) *smartDecider {
	return &smartDecider{now: nowFn, items: make(map[uint64]*list.Element), order: list.New()}
}

func (d *smartDecider) cacheLen() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.order.Len()
}

func (d *smartDecider) cacheGet(key uint64) (int, int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	el, ok := d.items[key]
	if !ok {
		return 0, 0, false
	}
	entry, _ := el.Value.(*decisionCacheEntry)
	if !d.now().Before(entry.expires) {
		delete(d.items, key)
		d.order.Remove(el)
		return 0, 0, false
	}
	d.order.MoveToFront(el)
	return entry.rank, entry.prob, true
}

func (d *smartDecider) cachePut(key uint64, rank, prob int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	exp := d.now().Add(decisionCacheTTL)
	if el, ok := d.items[key]; ok {
		entry, _ := el.Value.(*decisionCacheEntry)
		entry.rank, entry.prob, entry.expires = rank, prob, exp
		d.order.MoveToFront(el)
		return
	}
	d.items[key] = d.order.PushFront(&decisionCacheEntry{key: key, rank: rank, prob: prob, expires: exp})
	for d.order.Len() > decisionCacheSize {
		oldest := d.order.Back()
		entry, _ := oldest.Value.(*decisionCacheEntry)
		delete(d.items, entry.key)
		d.order.Remove(oldest)
	}
}

func (d *smartDecider) breakerOpen() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.now().Before(d.openUntil)
}

// record counts one call result; a failure at the threshold opens the breaker.
func (d *smartDecider) record(success bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if success {
		d.fails = 0
		return
	}
	d.fails++
	if d.fails >= decisionBreakerFails {
		d.openUntil = d.now().Add(decisionBreakerOpen)
	}
}

// decisionAnswer is one decision-model result; outcome is empty for a fresh answer.
type decisionAnswer struct {
	outcome string
	rank    int
	prob    int
}

type decisionQuestion struct {
	Type         string   `json:"type"`
	Instructions string   `json:"instructions"`
	Criteria     []string `json:"criteria"`
}

type decisionRequest struct {
	Questions map[string]decisionQuestion `json:"questions"`
	State     string                      `json:"state"`
}

type decisionTierAnswer struct {
	Probabilities map[string]float64 `json:"probabilities"`
}

type decisionResponse struct {
	Answers map[string]decisionTierAnswer `json:"answers"`
}

// decisionState is the text the decision model sees and the cache key source.
func decisionState(system, ask string) string {
	if len(ask) > decisionAskCap {
		ask = ask[:decisionAskCap]
	}
	if system == "" {
		return ask
	}
	if len(system) > decisionSystemCap {
		system = system[:decisionSystemCap]
	}
	return system + "\n\n" + ask
}

func decisionRequestBody(rt *smartRouter, state string) []byte {
	names := make([]string, len(rt.tiers))
	for i, t := range rt.tiers {
		names[i] = t.name
	}
	req := decisionRequest{
		State: state,
		Questions: map[string]decisionQuestion{"tier": {
			Type:         "score",
			Instructions: "Pick the cheapest tier whose model can answer this request well. Tiers run from cheapest and weakest to strongest: " + strings.Join(names, ", ") + ".",
			Criteria:     names,
		}},
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil
	}
	return b
}

// decodeDecisionAnswer returns the argmax tier and its per-mille probability.
func decodeDecisionAnswer(body []byte, tiers int) (int, int, bool) {
	var resp decisionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, 0, false
	}
	probs := resp.Answers["tier"].Probabilities
	if len(probs) == 0 {
		return 0, 0, false
	}
	for k := range probs {
		i, err := strconv.Atoi(k)
		if err != nil || i < 0 || i >= tiers {
			return 0, 0, false
		}
	}
	best, bestP := -1, -1.0
	for i := 0; i < tiers; i++ {
		p, present := probs[strconv.Itoa(i)]
		if !present {
			continue
		}
		if p < 0 || p > 1 {
			return 0, 0, false
		}
		if p > bestP {
			best, bestP = i, p
		}
	}
	if best < 0 {
		return 0, 0, false
	}
	return best, int(bestP*1000 + 0.5), true
}

// askDecisionModel returns the cached answer, else calls the decision model unless the breaker is open.
func (g *Gateway) askDecisionModel(ctx context.Context, rt *smartRouter, state string) decisionAnswer {
	cfg := rt.decision
	h := fnv.New64a()
	_, _ = h.Write([]byte(state))
	key := h.Sum64()
	if rank, prob, ok := cfg.dec.cacheGet(key); ok {
		return decisionAnswer{outcome: decisionCache, rank: rank, prob: prob}
	}
	if cfg.dec.breakerOpen() {
		return decisionAnswer{outcome: decisionBreaker}
	}
	rank, prob, outcome := g.callDecisionModel(ctx, rt, state)
	if outcome != "" {
		cfg.dec.record(false)
		return decisionAnswer{outcome: outcome}
	}
	cfg.dec.record(true)
	cfg.dec.cachePut(key, rank, prob)
	return decisionAnswer{rank: rank, prob: prob}
}

// callDecisionModel returns the decoded answer, or an outcome of timeout or error.
func (g *Gateway) callDecisionModel(ctx context.Context, rt *smartRouter, state string) (int, int, string) {
	cfg := rt.decision
	oa, isOpenAI := g.adapters[cfg.provider].(*openaiAdapter)
	if !isOpenAI {
		return 0, 0, decisionError
	}
	body := decisionRequestBody(rt, state)
	if body == nil {
		return 0, 0, decisionError
	}
	callCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	probe := &http.Request{Header: http.Header{}}
	oa.injectAuth(probe)
	probe.Header.Set("Content-Type", "application/json")
	url := oa.passthroughBase("", cfg.path) + cfg.path
	// The watchdog gets a longer budget so callCtx's deadline decides the timeout outcome.
	resp, err := upstreamBytes(callCtx, oa.httpClient(), http.MethodPost, url, probe.Header, body, nil, 2*cfg.timeout, cfg.provider)
	if err != nil {
		return 0, 0, decisionFailure(ctx, callCtx)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, decisionBodyLimit))
	if err != nil {
		return 0, 0, decisionFailure(ctx, callCtx)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, 0, decisionError
	}
	rank, prob, ok := decodeDecisionAnswer(data, len(rt.tiers))
	if !ok {
		return 0, 0, decisionError
	}
	return rank, prob, ""
}

// decisionFailure maps a failed call to timeout when only the derived deadline expired.
func decisionFailure(parent, call context.Context) string {
	if parent.Err() == nil && call.Err() == context.DeadlineExceeded {
		return decisionTimeout
	}
	return decisionError
}
