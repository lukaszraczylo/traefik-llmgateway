package traefikllmgateway

import "time"

const (
	statsScopeShared   = "shared"
	statsScopeInstance = "instance"

	kindRouter       = "router"
	metricRouterReq  = "rreq"
	metricRouterEsc  = "resc"
	routerKeySep     = "/"
	routerReasonsMax = 5
)

var routerReasonNames = [routerReasonsMax]string{smartReasonScore, smartReasonKeyword, smartReasonFloor, smartReasonDefault, routerReasonSkipped}

func routerReqKeyFor(k routerRequestKey, bucket string) string {
	return windowKeyForBucket(kindRouter, k.router, metricRouterReq+routerKeySep+k.tier+routerKeySep+k.reason, windowMonth, bucket)
}

func routerEscKeyFor(k routerEscalationKey, bucket string) string {
	return windowKeyForBucket(kindRouter, k.router, metricRouterEsc+routerKeySep+k.from+routerKeySep+k.to, windowMonth, bucket)
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

// routerStatsSnapshot returns the counters the admin view shows and their scope.
func (g *Gateway) routerStatsSnapshot(now time.Time) (map[routerRequestKey]int64, map[routerEscalationKey]int64, string) {
	if g.routerSharedEnabled() {
		if reqs, escs, ok := g.readRouterShared(now); ok {
			return reqs, escs, statsScopeShared
		}
	}
	reqs, escs := g.routerStats.snapshot()
	return reqs, escs, statsScopeInstance
}

// readRouterShared reads every config-derived key in one chunked round trip; ok is false on any store failure.
func (g *Gateway) readRouterShared(now time.Time) (map[routerRequestKey]int64, map[routerEscalationKey]int64, bool) {
	bucket := bucketFor(now, windowMonth)
	var reqKeys []routerRequestKey
	var escKeys []routerEscalationKey
	var reqNames, escNames []string
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
		}
	}
	keys := append(reqNames, escNames...)
	reqs := make(map[routerRequestKey]int64)
	escs := make(map[routerEscalationKey]int64)
	if len(keys) == 0 {
		return reqs, escs, true
	}
	vals, ok := g.limiter.chunkedGetMulti(keys, usageModelsChunkKeys)
	if !ok {
		return nil, nil, false
	}
	for i, k := range reqKeys {
		if vals[i] > 0 {
			reqs[k] = vals[i]
		}
	}
	for i, k := range escKeys {
		if v := vals[len(reqKeys)+i]; v > 0 {
			escs[k] = v
		}
	}
	return reqs, escs, true
}
