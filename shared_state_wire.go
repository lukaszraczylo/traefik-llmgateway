package traefikllmgateway

import (
	"sort"
	"time"
)

// wireSharedState layers fleet-wide state over the local trackers. It is a no-op without a shared-state loop.
func (g *Gateway) wireSharedState(cfg *Config) {
	if g.sharedState == nil {
		return
	}
	ids := sharedStateIDs(cfg)
	if g.failoverHealth != nil {
		g.failoverHealth.share(newSharedBreaker(g.sharedState, sharedKindRequest, ids,
			requestBreakerFailureThreshold, requestBreakerOpenDuration, requestBreakerMaxOpenDuration, 2*requestBreakerMaxOpenDuration))
	}
	if g.concurrency != nil {
		g.concurrency.share(newSharedInflight(g.sharedState, ids, maxProviderTimeout(cfg)))
	}
	if g.registry != nil {
		brk, err := validateBreakerConfig(cfg.Breaker)
		if err != nil {
			brk = breakerConfig{failureThreshold: defaultBreakerFailureThreshold, openBase: defaultBreakerOpenDuration, openMax: defaultBreakerMaxOpenDuration}
		}
		// Each instance counts its own consecutive failures and shares only its trip, so the fleet opens on the first one.
		g.registry.shareBreaker(newSharedBreaker(g.sharedState, sharedKindDiscovery, ids,
			sharedDiscoveryFleetThreshold, brk.openBase, brk.openMax, 2*brk.openMax))
	}
	g.shareDecisionBreakers()
	if g.auth != nil {
		g.auth.share(g.sharedState)
	}
	if g.tokenCalib != nil && g.limiter != nil {
		g.tokenCalib.share(func() time.Time { return g.limiter.now() })
	}
}

// maxProviderTimeout is the longest request timeout any provider has, which sets the in-flight read window.
// The timeout is progress-based, so a stream can outlive it and drop out of the count: an under-count only.
func maxProviderTimeout(cfg *Config) time.Duration {
	global, err := resolveRequestTimeout(cfg.RequestTimeout, "requestTimeout")
	if err != nil {
		global = defaultRequestTimeout
	}
	longest := global
	for name, pc := range cfg.Providers {
		if pc == nil {
			continue
		}
		if d, perr := resolveProviderTimeout(name, pc.RequestTimeout, global); perr == nil && d > longest {
			longest = d
		}
	}
	return longest
}

// shareDecisionBreakers gives every router's decider one fleet breaker. Routers on the same decision provider share a key on purpose.
// The breaker covers only the decision providers: a decider is only ever queried with its own router's provider.
func (g *Gateway) shareDecisionBreakers() {
	if g.registry == nil {
		return
	}
	seen := make(map[string]bool)
	var ids []string
	for _, rt := range g.registry.routers {
		if rt == nil || rt.decision == nil || rt.decision.dec == nil || seen[rt.decision.provider] {
			continue
		}
		seen[rt.decision.provider] = true
		ids = append(ids, rt.decision.provider)
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	brk := newSharedBreaker(g.sharedState, sharedKindDecision, ids,
		decisionBreakerFails, decisionBreakerOpen, decisionBreakerOpen, 2*decisionBreakerOpen)
	for _, rt := range g.registry.routers {
		if rt == nil || rt.decision == nil || rt.decision.dec == nil {
			continue
		}
		rt.decision.dec.share(brk, rt.decision.provider)
	}
}
