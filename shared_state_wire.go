package traefikllmgateway

import "time"

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
}

// maxProviderTimeout is the longest request timeout any provider can run, which bounds how long an in-flight count stays live.
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
