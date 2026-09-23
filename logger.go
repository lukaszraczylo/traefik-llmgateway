package traefikllmgateway

import (
	"fmt"
	"net/http"
	"os"
)

// logf writes an info-level log line to stderr. No timestamps — Traefik
// adds its own. Never log key material.
func (g *Gateway) logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "llmgw[%s] INFO %s\n", g.name, fmt.Sprintf(format, args...))
}

// warnf writes a warning-level log line to stderr. No timestamps —
// Traefik adds its own. Never log key material.
//
// Use this for a condition an operator may want to know about but that
// the gateway resolved by itself, deterministically, with no request
// affected — a model id served by two providers, for instance. Those
// lines were ERROR until 2026-08-22, which put 144 of them in a healthy
// pod's boot log on a cluster with overlapping catalogs and made real
// errors harder to find. Reserve errorf for something actually broken.
//
// Also calls noteConfigWarning (F10, v0.3 dashboard task): while
// newGateway is still constructing g (collectingWarnings true), the
// formatted message is additionally collected into g.configWarnings —
// GET /admin/api/overview's Warnings field. A warnf call from
// request-handling code, after construction finished, is NOT collected —
// see noteConfigWarning's own doc comment for why.
func (g *Gateway) warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "llmgw[%s] WARN %s\n", g.name, msg)
	g.noteConfigWarning(msg)
}

// configWarningsCap bounds how many construction-time warnings
// g.configWarnings ever holds (F10, v0.3 dashboard task) — a
// misconfigured deployment logging hundreds of warnings during
// construction must not make GET /admin/api/overview's response itself
// unbounded. configWarningsDropped counts what did not fit, so the
// dashboard can say "and N more" instead of silently truncating.
const configWarningsCap = 50

// noteConfigWarning appends msg to g.configWarnings while newGateway is
// still collecting them — collectingWarnings is set true right after g is
// constructed and false just before newGateway returns (llmgateway.go),
// so every warnf call in that window is a CONSTRUCTION-time warning,
// exactly what GET /admin/api/overview's Warnings field surfaces. Once
// collectingWarnings is false (every warnf call from request-handling
// code, for the rest of the process lifetime), this is a no-op: such a
// warning already reached the operator via the stderr line warnf itself
// wrote, and an unbounded stream of runtime warnings must never keep
// growing this slice for the life of the process.
func (g *Gateway) noteConfigWarning(msg string) {
	g.warnMu.Lock()
	defer g.warnMu.Unlock()
	if !g.collectingWarnings {
		return
	}
	if len(g.configWarnings) >= configWarningsCap {
		g.configWarningsDropped++
		return
	}
	g.configWarnings = append(g.configWarnings, msg)
}

// configWarningsSnapshot returns a fresh copy of g.configWarnings — never
// nil, matching GET /admin/api/overview's own "never nil" contract for
// its Warnings field — and the dropped count, safe for concurrent use
// alongside noteConfigWarning.
func (g *Gateway) configWarningsSnapshot() (warnings []string, dropped int) {
	g.warnMu.Lock()
	defer g.warnMu.Unlock()
	out := make([]string, len(g.configWarnings))
	copy(out, g.configWarnings)
	return out, g.configWarningsDropped
}

// errorf writes an error-level log line to stderr. No timestamps — Traefik
// adds its own. Never log key material.
func (g *Gateway) errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "llmgw[%s] ERROR %s\n", g.name, fmt.Sprintf(format, args...))
}

// pricingWarn is g's own per-instance pricing-warning sink, passed to
// costMicrosKnownFor (pricing.go) so an unpriced-model warning always logs
// under this instance's `llmgw[name]` prefix (finding F11 / review-auth
// F8, 2026-09 review: a package-level warn func replaced on every New()
// call made the most recently constructed instance log every other
// instance's warnings).
//
// A plain method, not a stored closure field: it reads g.name fresh on
// every call, exactly like logf/warnf/errorf above, so there is nothing
// new to keep in sync across a config reload that rebuilds g.
func (g *Gateway) pricingWarn(msg string) {
	if msg == warnCapMessage {
		g.logf("%s", msg)
		return
	}
	g.logf("pricing: no price configured for model %q; cost will be recorded as 0", msg)
	// F3 hook 4 (v0.3 dashboard task): msg is the bare unpriced model id
	// here (warnUnknownModelFor's own "warn(model)" call, pricing.go) —
	// the cap-message branch above is a process-wide meta-warning, not
	// about any one model, so it is deliberately excluded from the event
	// feed. Route is the fixed "pricing" label: this fires deep inside
	// cost accounting, with no request-scoped route name available.
	g.recordEvent(gatewayEvent{
		Model: msg, Route: routePricing, Kind: eventKindUnpriced,
		Message: fmt.Sprintf("no price configured for model %q; cost recorded as 0", msg),
	})
}

// logAuthEvent logs one authentication attempt, at the point one of
// authStore.identify's six call sites resolves it. A success logs the
// resolved user's name alongside the request's method and path. Neither
// line this function ever writes includes the presented API key itself —
// identify resolves a key to a *user and hands this function only that
// user's name, never the key it authenticated.
//
// A failure logs one of two distinct lines (security review round 2,
// 2026-08-22, important finding 5): if the caller's source IP
// (clientIP) is currently authThrottled, a distinct "throttle engaged"
// line naming the IP and the limit it exceeded — an operator seeing this
// knows immediately that a SOURCE, not just an individual request, needs
// attention, which the routine per-attempt line alone does not convey.
// Otherwise the routine failure line (method, path, remote address).
// Both lines are independently rate-limited (authStore.shouldLog
// ThrottleEngaged / shouldLogAuthFailure, auth.go — the identical
// technique limiter.logStoreError, limits.go, already applies to
// store-error lines) so an unauthenticated caller driving unlimited
// failed attempts can never drive unlimited synchronous stderr writes on
// the shared Traefik ingress — a slow log pipe would otherwise stall
// request handling for every tenant, not just the one failing auth. Each
// gate reports how many events it suppressed since its last written line
// (security review round 2, important finding 6): a single global gate
// would otherwise make several DISTINCT attacker IPs failing within one
// interval indistinguishable from one noisy IP — suppressedSuffix
// appends that count to the line when non-zero. The success line stays
// completely unbounded — see shouldLogAuthFailure's own doc comment for
// why that is safe.
func (g *Gateway) logAuthEvent(ok bool, userName string, r *http.Request) {
	if !ok {
		ip := clientIP(r)
		if g.auth.authThrottled(ip) {
			if log, suppressed := g.auth.shouldLogThrottleEngaged(); log {
				g.logf("auth throttle: source %s exceeded %d failed attempts in the last %s%s", ip, authFailureLimit, authFailureWindow, suppressedSuffix(suppressed))
			}
			return
		}
		if log, suppressed := g.auth.shouldLogAuthFailure(); log {
			g.logf("auth failed: %s %q from %s%s", r.Method, r.URL.Path, r.RemoteAddr, suppressedSuffix(suppressed))
		}
		return
	}
	g.logf("auth ok: user %q %s %q", userName, r.Method, r.URL.Path)
}

// suppressedSuffix formats n (a logGate's own suppressed-event count,
// auth.go) as a log-line suffix, or "" when there is nothing to report.
func suppressedSuffix(n int64) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%d more suppressed since last log)", n)
}

// authEventUserName extracts u's name for logAuthEvent's userName
// argument, or "" when u is nil — identify's failure path, which never
// resolves a user.
func authEventUserName(u *user) string {
	if u == nil {
		return ""
	}
	return u.name
}
