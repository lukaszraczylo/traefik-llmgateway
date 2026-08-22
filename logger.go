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

// errorf writes an error-level log line to stderr. No timestamps — Traefik
// adds its own. Never log key material.
func (g *Gateway) errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "llmgw[%s] ERROR %s\n", g.name, fmt.Sprintf(format, args...))
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
