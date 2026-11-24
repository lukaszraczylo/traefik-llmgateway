package traefikllmgateway

import (
	"fmt"
	"sort"
	"sync"
)

const (
	eventKindRouterEscalation = "router_escalation"

	// routerReasonSkipped marks a request whose first attempted tier is above
	// the classified tier because lower tiers had no usable target.
	routerReasonSkipped = "skipped"
)

type routerRequestKey struct {
	router, tier, reason string
}

type routerEscalationKey struct {
	router, from, to string
}

// routerStats holds per-replica router counters. Keys come from router and
// tier names in the config, so cardinality is bounded by the config.
type routerStats struct {
	requests    map[routerRequestKey]int64
	escalations map[routerEscalationKey]int64
	mu          sync.Mutex
}

func newRouterStats() *routerStats {
	return &routerStats{
		requests:    make(map[routerRequestKey]int64),
		escalations: make(map[routerEscalationKey]int64),
	}
}

func (s *routerStats) addRequest(k routerRequestKey) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.requests[k]++
	s.mu.Unlock()
}

func (s *routerStats) addEscalation(k routerEscalationKey) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.escalations[k]++
	s.mu.Unlock()
}

func (s *routerStats) snapshot() (map[routerRequestKey]int64, map[routerEscalationKey]int64) {
	reqs := make(map[routerRequestKey]int64)
	escs := make(map[routerEscalationKey]int64)
	if s == nil {
		return reqs, escs
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.requests {
		reqs[k] = v
	}
	for k, v := range s.escalations {
		escs[k] = v
	}
	return reqs, escs
}

// routerRequestReason is the classifier reason, or "skipped" when the first
// attempted tier is above the classified one: lower tiers lost every target
// to context-window, vision, cost-budget, access or resolution skips.
func routerRequestReason(plan *routerPlan) string {
	if plan.entries[0].tier != plan.startTier {
		return routerReasonSkipped
	}
	return plan.reason
}

// recordRouterAttempt counts the request on its first attempt and each
// cross-tier step. A same-tier attempt is plain failover and counts nothing.
func (g *Gateway) recordRouterAttempt(plan *routerPlan, i int, endpoint string, scopes []limitScope) {
	if i == 0 {
		if metricsEnabled(g.cfg) {
			g.routerStats.addRequest(routerRequestKey{router: plan.router, tier: plan.entries[0].tier, reason: routerRequestReason(plan)})
		}
		return
	}
	from, to := plan.entries[i-1].tier, plan.entries[i].tier
	if from == to {
		return
	}
	if metricsEnabled(g.cfg) {
		g.routerStats.addEscalation(routerEscalationKey{router: plan.router, from: from, to: to})
	}
	g.recordRouterEvent(plan, i, endpoint, scopes, fmt.Sprintf("router %q escalated from tier %q to tier %q", plan.router, from, to))
}

// recordRouterExhausted records an event when the last plan entry failed.
func (g *Gateway) recordRouterExhausted(plan *routerPlan, i int, endpoint string, scopes []limitScope) {
	g.recordRouterEvent(plan, i, endpoint, scopes, fmt.Sprintf("router %q exhausted all %d attempts; last tier %q", plan.router, len(plan.entries), plan.entries[i].tier))
}

func (g *Gateway) recordRouterEvent(plan *routerPlan, i int, endpoint string, scopes []limitScope, msg string) {
	el := g.events
	if el == nil {
		return
	}
	log, suppressed := g.routerEventGate.shouldLog(el.nowFn())
	if !log {
		return
	}
	if suppressed > 0 {
		msg = fmt.Sprintf("%s (%d more suppressed since last event)", msg, suppressed)
	}
	ev := gatewayEvent{
		Model:    plan.entries[i].cand.canonical,
		Provider: plan.entries[i].cand.providerName,
		Route:    eventRouteForEndpoint(endpoint),
		Kind:     eventKindRouterEscalation,
		Message:  msg,
	}
	applyScopeAttribution(&ev, scopes)
	g.recordEvent(ev)
}

// writeRouterMetrics emits the router counter families; absent until the first observation.
func (g *Gateway) writeRouterMetrics(m *metricWriter) {
	reqs, escs := g.routerStats.snapshot()
	if len(reqs) > 0 {
		keys := make([]routerRequestKey, 0, len(reqs))
		for k := range reqs {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool {
			x, y := keys[a], keys[b]
			if x.router != y.router {
				return x.router < y.router
			}
			if x.tier != y.tier {
				return x.tier < y.tier
			}
			return x.reason < y.reason
		})
		m.family("llmgateway_router_requests_total", "counter",
			"Smart router requests by router, first attempted tier and routing reason (score, keyword, floor, default, or skipped when lower tiers had no usable target)."+aggregationNotePerProcess)
		for _, k := range keys {
			m.sampleInt("llmgateway_router_requests_total",
				[]metricLabel{{"router", k.router}, {"tier", k.tier}, {"reason", k.reason}}, reqs[k])
		}
	}
	if len(escs) > 0 {
		keys := make([]routerEscalationKey, 0, len(escs))
		for k := range escs {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool {
			x, y := keys[a], keys[b]
			if x.router != y.router {
				return x.router < y.router
			}
			if x.from != y.from {
				return x.from < y.from
			}
			return x.to < y.to
		})
		m.family("llmgateway_router_escalations_total", "counter",
			"Smart router attempts that moved to a higher tier after a failure, by router, from_tier and to_tier. Same-tier failover is not counted."+aggregationNotePerProcess)
		for _, k := range keys {
			m.sampleInt("llmgateway_router_escalations_total",
				[]metricLabel{{"router", k.router}, {"from_tier", k.from}, {"to_tier", k.to}}, escs[k])
		}
	}
}
