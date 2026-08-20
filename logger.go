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
// authStore.identify's six call sites resolves it. A failure logs the
// method, path, and remote address the request came from; a success logs
// the resolved user's name alongside the same method and path. Neither
// line ever includes the presented API key itself — identify resolves a
// key to a *user and hands this function only that user's name, never the
// key it authenticated.
func (g *Gateway) logAuthEvent(ok bool, userName string, r *http.Request) {
	if !ok {
		g.logf("auth failed: %s %q from %s", r.Method, r.URL.Path, r.RemoteAddr)
		return
	}
	g.logf("auth ok: user %q %s %q", userName, r.Method, r.URL.Path)
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
