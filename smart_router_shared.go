package traefikllmgateway

import "time"

const (
	statsScopeShared   = "shared"
	statsScopeInstance = "instance"

	kindRouter       = "router"
	metricRouterReq  = "rreq"
	metricRouterEsc  = "resc"
	metricRouterShd  = "rshd"
	metricRouterOut  = "rout"
	routerKeySep     = "/"
	routerReasonsMax = 11
)

var routerReasonNames = [routerReasonsMax]string{smartReasonScore, smartReasonKeyword, smartReasonFloor, smartReasonDefault, routerReasonSkipped, smartReasonSession, smartReasonLoop, smartReasonBump, smartReasonModel, smartReasonSaturated, routerReasonFallback}

func routerReqKeyFor(k routerRequestKey, bucket string) string {
	return windowKeyForBucket(kindRouter, k.router, metricRouterReq+routerKeySep+k.tier+routerKeySep+k.reason, windowMonth, bucket)
}

func routerEscKeyFor(k routerEscalationKey, bucket string) string {
	return windowKeyForBucket(kindRouter, k.router, metricRouterEsc+routerKeySep+k.from+routerKeySep+k.to, windowMonth, bucket)
}

func routerShadowKeyFor(k routerShadowKey, bucket string) string {
	return windowKeyForBucket(kindRouter, k.router, metricRouterShd+routerKeySep+k.live+routerKeySep+k.shadow, windowMonth, bucket)
}

func routerOutcomeKeyFor(k routerOutcomeKey, bucket string) string {
	return windowKeyForBucket(kindRouter, k.router, metricRouterOut+routerKeySep+k.tier+routerKeySep+k.reason+routerKeySep+k.outcome, windowMonth, bucket)
}

// routerSharedEnabled reports whether router counters go to the shared store; false keeps them per instance.
func (g *Gateway) routerSharedEnabled() bool {
	return g.limiter != nil && g.limiter.store != nil && g.limiter.statsAdmin
}

// countRouterShared writes one router counter off the request path; a store failure only logs.
func (g *Gateway) countRouterShared(key func(bucket string) string) {
	if !g.routerSharedEnabled() {
		return
	}
	bucket := bucketFor(g.limiter.now(), windowMonth)
	g.limiter.countAsync([]counterIncr{{key: key(bucket), delta: 1, ttl: monthWindowTTL, enforceTTL: enforceTTLMonthWindow}})
}

// routerCounters is one read of every router counter family and the scope it covers.
type routerCounters struct {
	reqs     map[routerRequestKey]int64
	escs     map[routerEscalationKey]int64
	shadow   map[routerShadowKey]int64
	outcomes map[routerOutcomeKey]int64
	scope    string
}

// routerStatsSnapshot returns the counters the admin view shows and their scope.
func (g *Gateway) routerStatsSnapshot(now time.Time) routerCounters {
	if g.routerSharedEnabled() {
		if c, ok := g.readRouterShared(now); ok {
			c.scope = statsScopeShared
			return c
		}
	}
	reqs, escs := g.routerStats.snapshot()
	shadow, outcomes := g.routerStats.extraSnapshot()
	return routerCounters{reqs: reqs, escs: escs, shadow: shadow, outcomes: outcomes, scope: statsScopeInstance}
}

// readRouterShared reads every config-derived key in one chunked round trip; ok is false on any store failure.
func (g *Gateway) readRouterShared(now time.Time) (routerCounters, bool) {
	bucket := bucketFor(now, windowMonth)
	var reqKeys []routerRequestKey
	var escKeys []routerEscalationKey
	var shdKeys []routerShadowKey
	var outKeys []routerOutcomeKey
	var reqNames, escNames, outNames, shdNames []string
	for _, rt := range g.registry.routers {
		for _, t := range rt.tiers {
			for _, reason := range routerReasonNames {
				k := routerRequestKey{router: rt.name, tier: t.name, reason: reason}
				reqKeys = append(reqKeys, k)
				reqNames = append(reqNames, routerReqKeyFor(k, bucket))
			}
			for _, to := range rt.tiers {
				if t.name == to.name {
					continue
				}
				k := routerEscalationKey{router: rt.name, from: t.name, to: to.name}
				escKeys = append(escKeys, k)
				escNames = append(escNames, routerEscKeyFor(k, bucket))
			}
			for _, reason := range routerReasonNames {
				for _, outcome := range routerOutcomeNames {
					k := routerOutcomeKey{router: rt.name, tier: t.name, reason: reason, outcome: outcome}
					outKeys = append(outKeys, k)
					outNames = append(outNames, routerOutcomeKeyFor(k, bucket))
				}
			}
			if !rt.shadowEnabled() {
				continue
			}
			for _, to := range rt.tiers {
				k := routerShadowKey{router: rt.name, live: t.name, shadow: to.name}
				shdKeys = append(shdKeys, k)
				shdNames = append(shdNames, routerShadowKeyFor(k, bucket))
			}
		}
	}
	names := append(append(append(reqNames, escNames...), outNames...), shdNames...)
	c := routerCounters{
		reqs:     make(map[routerRequestKey]int64),
		escs:     make(map[routerEscalationKey]int64),
		shadow:   make(map[routerShadowKey]int64),
		outcomes: make(map[routerOutcomeKey]int64),
	}
	if len(names) == 0 {
		return c, true
	}
	vals, ok := g.limiter.chunkedGetMulti(names, usageModelsChunkKeys)
	if !ok {
		return routerCounters{}, false
	}
	pos := 0
	for _, k := range reqKeys {
		if vals[pos] > 0 {
			c.reqs[k] = vals[pos]
		}
		pos++
	}
	for _, k := range escKeys {
		if vals[pos] > 0 {
			c.escs[k] = vals[pos]
		}
		pos++
	}
	for _, k := range outKeys {
		if vals[pos] > 0 {
			c.outcomes[k] = vals[pos]
		}
		pos++
	}
	for _, k := range shdKeys {
		if vals[pos] > 0 {
			c.shadow[k] = vals[pos]
		}
		pos++
	}
	return c, true
}
