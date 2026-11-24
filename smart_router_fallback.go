package traefikllmgateway

import (
	"fmt"
	"net/http"
	"time"
)

// validateSmartTimeoutFallback rejects timeoutFallback without a deadline to miss or a tier to fall to.
func validateSmartTimeoutFallback(router string, cfg *SmartRouterConfig, tiers int, firstByte time.Duration) error {
	if !cfg.TimeoutFallback {
		return nil
	}
	if firstByte <= 0 {
		return fmt.Errorf("llmgateway: smartRouters: router %q: timeoutFallback needs firstByteTimeout", router)
	}
	if tiers < 2 {
		return fmt.Errorf("llmgateway: smartRouters: router %q: timeoutFallback needs at least two tiers", router)
	}
	return nil
}

// routerFallbackRank returns the highest tier below firstRank that has a usable candidate, or -1.
func routerFallbackRank(firstRank int, usable func(int) bool) int {
	for r := firstRank - 1; r >= 0; r-- {
		if usable(r) {
			return r
		}
	}
	return -1
}

// routerFallbackCount sums the finished requests of one router that took the extra attempt.
func routerFallbackCount(router string, outcomes map[routerOutcomeKey]int64) int64 {
	var n int64
	for k, v := range outcomes {
		if k.router == router && k.reason == routerReasonFallback {
			n += v
		}
	}
	return n
}

// addTimeoutFallback appends the one extra attempt when every candidate is spent, one of them missed the
// first-byte deadline, the last failure still allows a retry, and the client is still waiting. It reports
// whether an attempt was added; the caller then continues its loop. The extra attempt may exceed maxAttempts
// by one, goes below any floor or session pin, and never repeats.
func (g *Gateway) addTimeoutFallback(plan *routerPlan, r *http.Request, last int, endpoint string, scopes []limitScope, callErr error) bool {
	if plan.pickFallback == nil || plan.fallbackAt > 0 || !plan.slowSeen || r.Context().Err() != nil || !routerEscalatable(callErr, true) {
		return false
	}
	entry, ok := plan.pickFallback()
	if !ok {
		return false
	}
	from := plan.entries[0].tier
	plan.entries = append(plan.entries, entry)
	plan.fallbackFrom = from
	plan.fallbackAt = len(plan.entries) - 1
	g.recordRouterEventKind(plan, plan.fallbackAt, endpoint, scopes, eventKindRouterTimeoutFallback, &g.routerFallbackGate,
		fmt.Sprintf("router %q: every attempt missed the first-byte deadline; falling back from tier %q to tier %q", plan.router, from, entry.tier))
	return true
}
