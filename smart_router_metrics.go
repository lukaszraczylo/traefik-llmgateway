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

	// routerReasonFallback marks a request served, or last tried, on the extra timeout-fallback attempt.
	routerReasonFallback = "fallback"

	eventKindRouterTimeoutFallback = "router_timeout_fallback"
)

type routerRequestKey struct {
	router, tier, reason string
}

type routerEscalationKey struct {
	router, from, to string
}

// routerShadowKey counts requests by the live tier and the tier shadowWeights chose.
type routerShadowKey struct {
	router, live, shadow string
}

// routerDecisionKey counts decision-model calls by outcome.
type routerDecisionKey struct {
	router, outcome string
}

// routerDecisionTierKey counts model answers by the heuristic tier and the tier the model ranked.
type routerDecisionTierKey struct {
	router, heuristic, model string
}

// routerOutcomeKey counts finished requests by first attempted tier, routing reason and outcome.
type routerOutcomeKey struct {
	router, tier, reason, outcome string
}

const (
	routerOutcomeOK        = "ok"
	routerOutcomeError     = "error"
	routerOutcomeEscalated = "escalated"
	// routerOutcomeBad marks an answer that was cut at the output cap, empty, or broken mid-stream.
	routerOutcomeBad = "bad"
	// routerOutcomeSlow marks a request where an attempt missed the first-byte deadline, however it ended.
	routerOutcomeSlow = "slow"
)

var routerOutcomeNames = [...]string{routerOutcomeOK, routerOutcomeError, routerOutcomeEscalated, routerOutcomeBad, routerOutcomeSlow}

// routerStats holds per-instance router counters. Keys come from router and
// tier names in the config, so cardinality is bounded by the config.
type routerStats struct {
	requests    map[routerRequestKey]int64
	escalations map[routerEscalationKey]int64
	shadow      map[routerShadowKey]int64
	outcomes    map[routerOutcomeKey]int64
	decisions   map[routerDecisionKey]int64
	decisionMx  map[routerDecisionTierKey]int64
	mu          sync.Mutex
}

func newRouterStats() *routerStats {
	return &routerStats{
		requests:    make(map[routerRequestKey]int64),
		escalations: make(map[routerEscalationKey]int64),
		shadow:      make(map[routerShadowKey]int64),
		outcomes:    make(map[routerOutcomeKey]int64),
		decisions:   make(map[routerDecisionKey]int64),
		decisionMx:  make(map[routerDecisionTierKey]int64),
	}
}

func (s *routerStats) addDecision(k routerDecisionKey) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.decisions[k]++
	s.mu.Unlock()
}

func (s *routerStats) addDecisionTier(k routerDecisionTierKey) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.decisionMx[k]++
	s.mu.Unlock()
}

// decisionSnapshot copies the decision outcome and tier-matrix counters.
func (s *routerStats) decisionSnapshot() (map[routerDecisionKey]int64, map[routerDecisionTierKey]int64) {
	dec := make(map[routerDecisionKey]int64)
	mx := make(map[routerDecisionTierKey]int64)
	if s == nil {
		return dec, mx
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.decisions {
		dec[k] = v
	}
	for k, v := range s.decisionMx {
		mx[k] = v
	}
	return dec, mx
}

func (s *routerStats) addShadow(k routerShadowKey) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.shadow[k]++
	s.mu.Unlock()
}

func (s *routerStats) addOutcome(k routerOutcomeKey) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.outcomes[k]++
	s.mu.Unlock()
}

// extraSnapshot copies the shadow and outcome counters.
func (s *routerStats) extraSnapshot() (map[routerShadowKey]int64, map[routerOutcomeKey]int64) {
	shadow := make(map[routerShadowKey]int64)
	outcomes := make(map[routerOutcomeKey]int64)
	if s == nil {
		return shadow, outcomes
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.shadow {
		shadow[k] = v
	}
	for k, v := range s.outcomes {
		outcomes[k] = v
	}
	return shadow, outcomes
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
	if plan.fallbackAt > 0 {
		return routerReasonFallback
	}
	if plan.entries[0].tier != plan.startTier {
		return routerReasonSkipped
	}
	return plan.reason
}

// recordRouterAttempt counts the request on its first attempt and each
// cross-tier step. A same-tier attempt is plain failover and counts nothing.
func (g *Gateway) recordRouterAttempt(plan *routerPlan, i int, endpoint string, scopes []limitScope) {
	if i == 0 {
		k := routerRequestKey{router: plan.router, tier: plan.entries[0].tier, reason: routerRequestReason(plan)}
		if metricsEnabled(g.cfg) {
			g.routerStats.addRequest(k)
		}
		g.countRouterShared(func(bucket string) string { return routerReqKeyFor(k, bucket) })
		if plan.shadowTier != "" {
			sk := routerShadowKey{router: plan.router, live: plan.startTier, shadow: plan.shadowTier}
			if metricsEnabled(g.cfg) {
				g.routerStats.addShadow(sk)
			}
			g.countRouterShared(func(bucket string) string { return routerShadowKeyFor(sk, bucket) })
		}
		g.recordRouterDecision(plan)
		return
	}
	if i == plan.fallbackAt {
		return
	}
	from, to := plan.entries[i-1].tier, plan.entries[i].tier
	if from == to {
		return
	}
	ek := routerEscalationKey{router: plan.router, from: from, to: to}
	if metricsEnabled(g.cfg) {
		g.routerStats.addEscalation(ek)
	}
	g.countRouterShared(func(bucket string) string { return routerEscKeyFor(ek, bucket) })
	g.recordRouterEvent(plan, i, endpoint, scopes, fmt.Sprintf("router %q escalated from tier %q to tier %q", plan.router, from, to))
}

// recordRouterOutcome counts a finished request once. attempt is the plan
// index of the last attempt; served says whether the client got a 2xx; bad overrides the other outcomes.
func (g *Gateway) recordRouterOutcome(plan *routerPlan, attempt int, served, bad bool) {
	outcome := routerOutcomeError
	if served {
		outcome = routerOutcomeOK
		if plan.entries[attempt].tier != plan.entries[0].tier {
			outcome = routerOutcomeEscalated
		}
	}
	if plan.slowSeen {
		outcome = routerOutcomeSlow
	}
	if bad {
		outcome = routerOutcomeBad
	}
	k := routerOutcomeKey{router: plan.router, tier: plan.entries[0].tier, reason: routerRequestReason(plan), outcome: outcome}
	if metricsEnabled(g.cfg) {
		g.routerStats.addOutcome(k)
	}
	g.countRouterShared(func(bucket string) string { return routerOutcomeKeyFor(k, bucket) })
	g.countCalibration(plan, attempt, served, bad)
	g.onRouterOutcome(plan, attempt, served, bad)
}

// recordRouterExhausted records an event when the last plan entry failed.
func (g *Gateway) recordRouterExhausted(plan *routerPlan, i int, endpoint string, scopes []limitScope) {
	g.recordRouterEvent(plan, i, endpoint, scopes, fmt.Sprintf("router %q exhausted all %d attempts; last tier %q", plan.router, len(plan.entries), plan.entries[i].tier))
}

func (g *Gateway) recordRouterEvent(plan *routerPlan, i int, endpoint string, scopes []limitScope, msg string) {
	g.recordRouterEventKind(plan, i, endpoint, scopes, eventKindRouterEscalation, &g.routerEventGate, msg)
}

func (g *Gateway) recordRouterEventKind(plan *routerPlan, i int, endpoint string, scopes []limitScope, kind string, gate *logGate, msg string) {
	el := g.events
	if el == nil {
		return
	}
	log, suppressed := gate.shouldLog(el.nowFn())
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
		Kind:     kind,
		Message:  msg,
	}
	applyScopeAttribution(&ev, scopes)
	g.recordEvent(ev)
}

// recordRouterDecision counts the decision-model outcome and, for answered calls, the heuristic-vs-model tier pair.
func (g *Gateway) recordRouterDecision(plan *routerPlan) {
	if plan.decisionOutcome == "" {
		return
	}
	dk := routerDecisionKey{router: plan.router, outcome: plan.decisionOutcome}
	if metricsEnabled(g.cfg) {
		g.routerStats.addDecision(dk)
	}
	g.countRouterShared(func(bucket string) string { return routerDecisionKeyFor(dk, bucket) })
	if !decisionAnswered(plan.decisionOutcome) || plan.heuristicTier == "" || plan.decisionTier == "" {
		return
	}
	mk := routerDecisionTierKey{router: plan.router, heuristic: plan.heuristicTier, model: plan.decisionTier}
	if metricsEnabled(g.cfg) {
		g.routerStats.addDecisionTier(mk)
	}
	g.countRouterShared(func(bucket string) string { return routerDecisionTierKeyFor(mk, bucket) })
}

// decisionAnswered reports whether the outcome carries a model rank.
func decisionAnswered(outcome string) bool {
	return outcome == decisionAgree || outcome == decisionOverride || outcome == decisionLowConf || outcome == decisionCache
}

// routerMetricSet is the counter source one scrape renders the router families from.
type routerMetricSet struct {
	reqs  map[routerRequestKey]int64
	escs  map[routerEscalationKey]int64
	decs  map[routerDecisionKey]int64
	decMx map[routerDecisionTierKey]int64
	note  string
}

// routerMetricSource reads the shared counters when the store backs them, else this pod's own.
// ok is false when the shared read fails, so the scrape skips the families.
func (g *Gateway) routerMetricSource() (routerMetricSet, bool) {
	if g.routerSharedEnabled() {
		c, ok := g.readRouterShared(g.limiter.now())
		if !ok {
			return routerMetricSet{}, false
		}
		return routerMetricSet{reqs: c.reqs, escs: c.escs, decs: c.decs, decMx: c.decMx, note: aggregationNoteStoreBacked}, true
	}
	reqs, escs := g.routerStats.snapshot()
	decs, decMx := g.routerStats.decisionSnapshot()
	return routerMetricSet{reqs: reqs, escs: escs, decs: decs, decMx: decMx, note: aggregationNotePerProcess}, true
}

// writeRouterMetrics emits the router counter families; absent until the first observation.
func (g *Gateway) writeRouterMetrics(m *metricWriter) {
	g.writeRouterCalibrationMetrics(m)
	src, ok := g.routerMetricSource()
	if !ok {
		return
	}
	reqs, escs, note := src.reqs, src.escs, src.note
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
			"Smart router requests by router, first attempted tier and routing reason (score, keyword, floor, model, default, or skipped when lower tiers had no usable target)."+note)
		for _, k := range keys {
			m.sampleInt("llmgateway_router_requests_total",
				[]metricLabel{{"router", k.router}, {"tier", k.tier}, {"reason", k.reason}}, reqs[k])
		}
	}
	g.writeRouterDecisionMetrics(m, src)
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
			"Smart router attempts that moved to a higher tier after a failure, by router, from_tier and to_tier. Same-tier failover is not counted."+note)
		for _, k := range keys {
			m.sampleInt("llmgateway_router_escalations_total",
				[]metricLabel{{"router", k.router}, {"from_tier", k.from}, {"to_tier", k.to}}, escs[k])
		}
	}
}

func (g *Gateway) writeRouterDecisionMetrics(m *metricWriter, src routerMetricSet) {
	dec, mx, note := src.decs, src.decMx, src.note
	if len(dec) > 0 {
		keys := make([]routerDecisionKey, 0, len(dec))
		for k := range dec {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool {
			if keys[a].router != keys[b].router {
				return keys[a].router < keys[b].router
			}
			return keys[a].outcome < keys[b].outcome
		})
		m.family("llmgateway_router_decision_total", "counter",
			"Smart router decision-model calls by router and outcome (agree, override, lowconf, timeout, error, breaker, cache)."+note)
		for _, k := range keys {
			m.sampleInt("llmgateway_router_decision_total",
				[]metricLabel{{"router", k.router}, {"outcome", k.outcome}}, dec[k])
		}
	}
	if len(mx) > 0 {
		keys := make([]routerDecisionTierKey, 0, len(mx))
		for k := range mx {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(a, b int) bool {
			x, y := keys[a], keys[b]
			if x.router != y.router {
				return x.router < y.router
			}
			if x.heuristic != y.heuristic {
				return x.heuristic < y.heuristic
			}
			return x.model < y.model
		})
		m.family("llmgateway_router_decision_tiers_total", "counter",
			"Smart router answers from the decision model or its cache by router, heuristic tier and model-ranked tier."+note)
		for _, k := range keys {
			m.sampleInt("llmgateway_router_decision_tiers_total",
				[]metricLabel{{"router", k.router}, {"heuristic", k.heuristic}, {"model", k.model}}, mx[k])
		}
	}
}
