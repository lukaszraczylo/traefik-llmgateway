package traefikllmgateway

import (
	"fmt"
	"sort"
	"strings"
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
)

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
